package redislock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockManagerWaitForRetryDelayCompletes(t *testing.T) {
	lm := &lockManager{}

	start := time.Now()
	if !lm.waitForRetryDelay(30 * time.Millisecond) {
		t.Fatal("expected waitForRetryDelay to complete when no shutdown is requested")
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("expected waitForRetryDelay to wait close to the requested duration, got %s", elapsed)
	}
}

func TestLockManagerWaitForRetryDelayStopsOnShutdown(t *testing.T) {
	lm := &lockManager{
		locks: make(map[string]*RedisLock),
	}
	lm.startRenewalService(DefaultLockOptions)

	resultCh := make(chan bool, 1)
	readyCh := make(chan struct{})
	go func() {
		close(readyCh)
		resultCh <- lm.waitForRetryDelay(5 * time.Second)
	}()

	<-readyCh
	start := time.Now()
	lm.stopRenewalService()

	select {
	case completed := <-resultCh:
		if completed {
			t.Fatal("expected waitForRetryDelay to abort when renewal service stops")
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Fatalf("expected waitForRetryDelay to stop promptly after shutdown, got %s", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waitForRetryDelay did not stop promptly after shutdown")
	}
}

func TestManagedLockAddRemoveIsIdempotent(t *testing.T) {
	resetGlobalLockManagerForTest(t)

	lock := &RedisLock{key: "order-1"}
	addManagedLock(lock)
	addManagedLock(lock)

	stats := GetStats()
	if stats["active_locks"] != 1 {
		t.Fatalf("expected one active lock after duplicate add, got %d", stats["active_locks"])
	}
	if stats["total_locks"] != 1 {
		t.Fatalf("expected one total lock after duplicate add, got %d", stats["total_locks"])
	}

	if !removeManagedLock(lock) {
		t.Fatal("expected first remove to succeed")
	}
	if removeManagedLock(lock) {
		t.Fatal("expected second remove to be a no-op")
	}
	if got := GetStats()["active_locks"]; got != 0 {
		t.Fatalf("expected active locks to stay at zero, got %d", got)
	}
}

func TestManagedLockReplaceDoesNotInflateActiveCount(t *testing.T) {
	resetGlobalLockManagerForTest(t)

	oldLock := &RedisLock{key: "order-1"}
	newLock := &RedisLock{key: "order-1"}
	addManagedLock(oldLock)
	addManagedLock(newLock)

	if got := GetStats()["active_locks"]; got != 1 {
		t.Fatalf("expected replacement to keep one active lock, got %d", got)
	}
	if removeManagedLock(oldLock) {
		t.Fatal("expected stale lock remove to be ignored")
	}
	if got := GetStats()["active_locks"]; got != 1 {
		t.Fatalf("expected stale remove to keep active lock count, got %d", got)
	}
	if !removeManagedLock(newLock) {
		t.Fatal("expected current lock remove to succeed")
	}
	if got := GetStats()["active_locks"]; got != 0 {
		t.Fatalf("expected no active locks, got %d", got)
	}
}

func TestCleanupContextIgnoresCanceledBusinessContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupCtx, cleanupCancel := cleanupContext(50 * time.Millisecond)
	defer cleanupCancel()

	if err := ctx.Err(); err == nil {
		t.Fatal("expected business context to be canceled")
	}
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context should not inherit business cancellation: %v", err)
	}
}

func TestSetCallbackConcurrentAccess(t *testing.T) {
	var calls int64
	callback := callbackFunc{
		acquired: func(string, time.Duration) {
			atomic.AddInt64(&calls, 1)
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			SetCallback(callback)
			currentCallback().OnLockAcquired("key", time.Second)
		}()
	}
	wg.Wait()
	SetCallback(nil)

	if atomic.LoadInt64(&calls) == 0 {
		t.Fatal("expected callback to be invoked")
	}
}

func TestNormalizeLockOptionsAppliesRenewalDefaults(t *testing.T) {
	options := normalizeLockOptions(LockOptions{
		Expiration:     time.Second,
		RenewalEnabled: true,
	})

	if options.RenewalThreshold != DefaultLockOptions.RenewalThreshold {
		t.Fatalf("expected default renewal threshold, got %v", options.RenewalThreshold)
	}
	if options.WorkerPoolSize != DefaultLockOptions.WorkerPoolSize {
		t.Fatalf("expected default worker pool size, got %d", options.WorkerPoolSize)
	}
	if options.RenewalConfig.BaseDelay != DefaultLockOptions.RenewalConfig.BaseDelay {
		t.Fatalf("expected default renewal base delay, got %s", options.RenewalConfig.BaseDelay)
	}
	if options.RenewalConfig.CheckInterval != DefaultLockOptions.RenewalConfig.CheckInterval {
		t.Fatalf("expected default renewal check interval, got %s", options.RenewalConfig.CheckInterval)
	}
}

func TestLockWithTokenRequiresCallback(t *testing.T) {
	if err := LockWithToken(context.Background(), "token-lock", time.Second, nil); !errors.Is(err, ErrLockFnRequired) {
		t.Fatalf("expected ErrLockFnRequired, got %v", err)
	}
}

type callbackFunc struct {
	acquired func(string, time.Duration)
}

func (c callbackFunc) OnLockAcquired(key string, duration time.Duration) {
	if c.acquired != nil {
		c.acquired(key, duration)
	}
}
func (callbackFunc) OnLockReleased(string, time.Duration) {}
func (callbackFunc) OnLockRenewed(string, time.Duration)  {}
func (callbackFunc) OnLockRenewalFailed(string, error)    {}
func (callbackFunc) OnLockAcquireFailed(string, error)    {}

func resetGlobalLockManagerForTest(t *testing.T) {
	t.Helper()
	globalLockManager.stopRenewalService()
	globalLockManager.mutex.Lock()
	globalLockManager.locks = make(map[string]*RedisLock)
	globalLockManager.workerPool = nil
	atomic.StoreInt64(&globalLockManager.stats.TotalLocks, 0)
	atomic.StoreInt64(&globalLockManager.stats.ActiveLocks, 0)
	atomic.StoreInt64(&globalLockManager.stats.RenewalCount, 0)
	atomic.StoreInt64(&globalLockManager.stats.RenewalErrors, 0)
	atomic.StoreInt64(&globalLockManager.stats.SkippedRenewals, 0)
	atomic.StoreInt64(&globalLockManager.stats.RenewLatencyNs, 0)
	atomic.StoreInt64(&globalLockManager.stats.RenewLatencyCount, 0)
	globalLockManager.mutex.Unlock()
}
