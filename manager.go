package redislock

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/pkg/timex"
)

// globalLockManager renews all auto-renew locks from one background goroutine.
// Design notes:
//   - a bounded worker pool caps concurrency so renewals never storm Redis;
//   - each renewal call may carry a per-call timeout (RenewalConfig.CallTimeout);
//   - stats use atomics to avoid lock contention on the hot path.
var globalLockManager = &lockManager{
	locks: make(map[string]*RedisLock),
}

var globalCallback LockCallback = NoOpCallback{}
var globalCallbackMu sync.RWMutex

// SetCallback installs the process-wide lock-event callback; nil resets to no-op.
func SetCallback(callback LockCallback) {
	if callback == nil {
		callback = NoOpCallback{}
	}
	globalCallbackMu.Lock()
	defer globalCallbackMu.Unlock()
	globalCallback = callback
}

func currentCallback() LockCallback {
	globalCallbackMu.RLock()
	defer globalCallbackMu.RUnlock()
	if globalCallback == nil {
		return NoOpCallback{}
	}
	return globalCallback
}

func addManagedLock(lock *RedisLock) {
	if lock == nil {
		return
	}
	globalLockManager.mutex.Lock()
	defer globalLockManager.mutex.Unlock()
	if existing, exists := globalLockManager.locks[lock.key]; exists {
		if existing != lock {
			globalLockManager.locks[lock.key] = lock
		}
		return
	}
	globalLockManager.locks[lock.key] = lock
	atomic.AddInt64(&globalLockManager.stats.TotalLocks, 1)
	atomic.AddInt64(&globalLockManager.stats.ActiveLocks, 1)
	activeLocksInc()
}

func removeManagedLock(lock *RedisLock) bool {
	if lock == nil {
		return false
	}
	globalLockManager.mutex.Lock()
	defer globalLockManager.mutex.Unlock()
	existing, exists := globalLockManager.locks[lock.key]
	if !exists || existing != lock {
		return false
	}
	delete(globalLockManager.locks, lock.key)
	decrementActiveLocks()
	return true
}

func decrementActiveLocks() {
	for {
		current := atomic.LoadInt64(&globalLockManager.stats.ActiveLocks)
		if current <= 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&globalLockManager.stats.ActiveLocks, current, current-1) {
			activeLocksDec()
			return
		}
	}
}

// startRenewalService starts the global renewal goroutine and worker pool (idempotent; only one goroutine runs).
func (lm *lockManager) startRenewalService(options LockOptions) {
	lm.mutex.Lock()
	if lm.running {
		lm.mutex.Unlock()
		return
	}
	lm.renewCtx, lm.renewCancel = context.WithCancel(context.Background())
	lm.running = true
	workerPoolSize := options.WorkerPoolSize
	if workerPoolSize <= 0 {
		workerPoolSize = DefaultLockOptions.WorkerPoolSize
	}
	lm.workerPool = make(chan struct{}, workerPoolSize)
	lm.mutex.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorCtx(context.Background(), "panic in renewal service goroutine", "recover", r)
				// Mark the service as stopped so it can be restarted on the next lock acquisition.
				lm.mutex.Lock()
				lm.running = false
				lm.mutex.Unlock()
			}
		}()
		checkInterval := options.RenewalConfig.CheckInterval
		if checkInterval <= 0 {
			checkInterval = DefaultRenewalConfig.CheckInterval
		}
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				lm.processRenewals(options)
			case <-lm.renewCtx.Done():
				return
			}
		}
	}()
}

// stopRenewalService stops the renewal service
func (lm *lockManager) stopRenewalService() {
	lm.mutex.Lock()
	defer lm.mutex.Unlock()

	if !lm.running {
		return
	}

	lm.renewCancel()
	lm.running = false
	// Do not close workerPool to avoid panic when other goroutines send to it.
}

// processRenewals processes lock renewals (using worker pool pattern).
// Renewal is only attempted when remaining TTL is within the threshold (e.g. default 30%:
// for a 30s lock, first renewal runs when ~9s or less remains). Note: when debugging
// with breakpoints the entire process is suspended, so the renewal goroutine does not
// run and Redis TTL continues to expire; use time.Sleep + logging to verify renewal in tests.
func (lm *lockManager) processRenewals(options LockOptions) {
	lm.mutex.RLock()

	locksToRenew := make([]*RedisLock, 0, len(lm.locks))

	for _, lock := range lm.locks {
		// Snapshot under the lock's mutex to avoid racing the holder/renewal.
		lock.mutex.Lock()
		expiresAtSnap := lock.expiresAt
		expirationSnap := lock.expiration
		thresholdSnap := lock.renewalThreshold
		lock.mutex.Unlock()

		// Renew once the remaining TTL drops below expiration*threshold.
		thresholdDur := time.Duration(float64(expirationSnap) * thresholdSnap)
		if time.Until(expiresAtSnap) <= thresholdDur {
			locksToRenew = append(locksToRenew, lock)
		}
	}
	lm.mutex.RUnlock()

	for _, lock := range locksToRenew {
		select {
		case <-lm.renewCtx.Done():
			return
		case lm.workerPool <- struct{}{}:
			// Capture the channel reference before launching the goroutine so that
			// concurrent resets of lm.workerPool (e.g. in tests) cannot race with
			// the deferred release.
			pool := lm.workerPool
			go func(l *RedisLock) {
				defer func() {
					<-pool
					if r := recover(); r != nil {
						log.ErrorCtx(context.Background(), "panic in lock renewal worker", "key", l.key, "recover", r)
					}
				}()
				lm.renewLockWithRetry(l, options)
			}(lock)
		default:
			// Pool saturated: skip this lock this round rather than block the loop.
			atomic.AddInt64(&lm.stats.SkippedRenewals, 1)
			incSkippedRenewal()
		}
	}
}

// renewLockWithRetry renews a lock with exponential backoff; on terminal failure
// it drops the lock from the manager (the key then expires naturally in Redis).
func (lm *lockManager) renewLockWithRetry(lock *RedisLock, options LockOptions) {
	config := options.RenewalConfig
	maxRetries := config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultRenewalConfig.MaxRetries
	}

	for i := 0; i < maxRetries; i++ {
		ctx := lm.renewCtx
		var cancel context.CancelFunc
		if to := config.CallTimeout; to > 0 {
			ctx, cancel = context.WithTimeout(ctx, to)
		}

		err := lm.renewLock(ctx, lock)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			atomic.AddInt64(&lm.stats.RenewalCount, 1)
			return
		}

		atomic.AddInt64(&lm.stats.RenewalErrors, 1)

		// Exponential backoff retry + jitter (50%~150%) to reduce concentrated competition
		if i < maxRetries-1 {
			delay := timex.ExponentialBackoff(config.BaseDelay, config.MaxDelay, i, 0.5)
			if !lm.waitForRetryDelay(delay) {
				return
			}
		}
	}

	// Retry failed: remove lock from manager and stop renewing this lock.
	// We do NOT run the unlock script here; the key in Redis will expire naturally after remaining TTL.
	// See LIMITATIONS.md "Behavior after renewal failure" for semantics and recommendations.
	removeManagedLock(lock)

	log.ErrorCtx(context.Background(), "lock renewal failed after retries",
		"key", lock.key, "retries", maxRetries)
}

func (lm *lockManager) waitForRetryDelay(delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	lm.mutex.RLock()
	ctx := lm.renewCtx
	lm.mutex.RUnlock()
	if ctx == nil {
		time.Sleep(delay)
		return true
	}

	return waitForContextDelay(ctx, delay)
}

func waitForContextDelay(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(delay)
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// renewLock renew a single lock (improved version)
func (lm *lockManager) renewLock(ctx context.Context, lock *RedisLock) error {
	client, err := lock.currentClient(ctx)
	if err != nil {
		return err
	}

	// Read snapshot to avoid concurrent read-write conflicts
	lock.mutex.Lock()
	expiresAtSnap := lock.expiresAt
	expirationSnap := lock.expiration
	thresholdSnap := lock.renewalThreshold
	lock.mutex.Unlock()

	// Check if renewal is needed (based on snapshot)
	if time.Until(expiresAtSnap) > time.Duration(float64(expirationSnap)*thresholdSnap) {
		return nil
	}

	// Execute renewal script (using cancellable context)
	start := time.Now()
	result, err := renewScript.Run(ctx, client, []string{lock.ownerKey, lock.countKey},
		lock.value, expirationSnap.Milliseconds()).Result()
	// Record renewal latency
	latency := time.Since(start)
	atomic.AddInt64(&lm.stats.RenewLatencyNs, latency.Nanoseconds())
	atomic.AddInt64(&lm.stats.RenewLatencyCount, 1)
	observeScriptLatency("renew", latency)
	if err != nil {
		incRenew("error")
		return fmt.Errorf("renewal script execution failed: %w", err)
	}

	n, ok := result.(int64)
	if !ok {
		return fmt.Errorf("unknown renewal result type: %T", result)
	}
	switch n {
	case 1: // Renewal successful
		lock.mutex.Lock()
		lock.expiresAt = time.Now().Add(lock.expiration)
		lock.mutex.Unlock()
		incRenew("success")
		return nil
	case 0, -1, -2: // Lock does not exist or not current holder
		removeManagedLock(lock)
		// More detailed distinction
		switch n {
		case 0:
			incRenew("not_exist")
		case -1:
			incRenew("not_owner")
		case -2:
			incRenew("fail")
		}
		return ErrLockRenewalFailed
	default:
		return fmt.Errorf("unknown renewal result: %v", result)
	}
}

// GetStats gets lock manager statistics
func GetStats() map[string]int64 {
	m := map[string]int64{
		"total_locks":         atomic.LoadInt64(&globalLockManager.stats.TotalLocks),
		"active_locks":        atomic.LoadInt64(&globalLockManager.stats.ActiveLocks),
		"renewal_count":       atomic.LoadInt64(&globalLockManager.stats.RenewalCount),
		"renewal_errors":      atomic.LoadInt64(&globalLockManager.stats.RenewalErrors),
		"skipped_renewals":    atomic.LoadInt64(&globalLockManager.stats.SkippedRenewals),
		"renew_latency_ns":    atomic.LoadInt64(&globalLockManager.stats.RenewLatencyNs),
		"renew_latency_count": atomic.LoadInt64(&globalLockManager.stats.RenewLatencyCount),
	}
	// Append current worker pool queue usage (reading len/cap as atomic snapshot, no locking required)
	// worker_queue_len represents currently occupied tokens; worker_queue_cap represents maximum concurrency capability.
	if globalLockManager.workerPool != nil {
		m["worker_queue_len"] = int64(len(globalLockManager.workerPool))
		m["worker_queue_cap"] = int64(cap(globalLockManager.workerPool))
	}
	return m
}

// Shutdown gracefully shuts down the lock manager
func Shutdown(ctx context.Context) error {
	globalLockManager.stopRenewalService()

	// Wait for all locks to be released or timeout.
	// Use a timer (not time.After) so we can stop it and avoid a goroutine leak.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			return fmt.Errorf("shutdown timeout, %d locks still active",
				atomic.LoadInt64(&globalLockManager.stats.ActiveLocks))
		case <-ticker.C:
			if atomic.LoadInt64(&globalLockManager.stats.ActiveLocks) == 0 {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
