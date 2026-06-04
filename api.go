package redislock

import (
	"context"
	"fmt"
	"time"

	"github.com/go-lynx/lynx/log"
	"github.com/go-lynx/lynx/pkg/timex"
)

// Lock acquires a distributed lock for the specified key and executes the callback function, automatically releasing the lock after execution.
// - Uses DefaultLockOptions as base configuration, only overriding Expiration.
// - Uses Lua script for atomic lock acquisition/reentrancy, avoiding race conditions.
// - If renewal is enabled, registers in global manager and automatically renews until function execution ends.
func Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error {
	options := DefaultLockOptions
	options.Expiration = expiration
	return LockWithOptions(ctx, key, options, fn)
}

// LockWithToken acquires a distributed lock and runs fn; the callback receives a fencing token (see LIMITATIONS.md).
// - Based on DefaultLockOptions, only overriding Expiration, retry strategy uses DefaultRetryStrategy.
// - token is only incremented on "first acquisition" (non-reentrant); reentry does not generate a new token.
func LockWithToken(ctx context.Context, key string, expiration time.Duration, fn func(token int64) error) (retErr error) {
	if fn == nil {
		return ErrLockFnRequired
	}
	options := DefaultLockOptions
	options.Expiration = expiration

	lock, err := NewLock(ctx, key, options)
	if err != nil {
		return err
	}

	if err := lock.AcquireWithRetry(ctx, options.RetryStrategy); err != nil {
		return err
	}

	if options.RenewalEnabled {
		lock.EnableAutoRenew(options)
	}

	// Always release on return.
	defer func() {
		to := options.ScriptCallTimeout
		if to <= 0 {
			to = DefaultLockOptions.ScriptCallTimeout
		}
		rctx, cancel := cleanupContext(to)
		start := time.Now()
		if releaseErr := lock.Release(rctx); releaseErr != nil {
			log.ErrorCtx(ctx, "failed to release redis lock", "error", releaseErr)
			if retErr == nil {
				retErr = releaseErr
			}
		}
		if cancel != nil {
			cancel()
		}
		observeScriptLatency("unlock", time.Since(start))
	}()

	// The token is the fencing token: non-zero on first acquisition, 0 on reentry.
	retErr = fn(lock.GetToken())
	return retErr
}

// UnlockByValue releases lock using key + value method (no need to hold RedisLock instance).
// Semantic explanation:
//   - When count > 0, this operation is a "partial release". This implementation uniformly passes TTL=0 to the script,
//     indicating not to refresh TTL (keeping the remaining expiration time unchanged).
//   - When key does not exist or value does not match, returns ErrLockNotHeld.
//
// Timeout explanation:
// - Single script call uses DefaultLockOptions.ScriptCallTimeout as optional per-call timeout.
func UnlockByValue(ctx context.Context, key, value string) error {
	if err := ValidateKey(key); err != nil {
		return newLockError(ErrCodeInvalidOptions, "invalid lock key", err)
	}
	provider, err := resolveRedisProvider()
	if err != nil {
		return err
	}
	client, err := provider.UniversalClient(ctx)
	if err != nil || client == nil {
		if err == nil {
			err = ErrRedisClientNotFound
		}
		return fmt.Errorf("%w: %v", ErrRedisClientNotFound, err)
	}
	ownerKey, countKey := buildLockKeys(key)
	runCtx := ctx
	var cancel context.CancelFunc
	if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
		runCtx, cancel = context.WithTimeout(ctx, to)
	}
	// Pass TTL=0 so a partial release decrements the count without refreshing TTL.
	result, err := unlockScript.Run(runCtx, client, []string{ownerKey, countKey}, value, int64(0)).Result()
	if cancel != nil {
		cancel()
	}
	if err != nil {
		return fmt.Errorf("unlock script execution failed: %w", err)
	}
	n, ok := result.(int64)
	if !ok {
		return fmt.Errorf("unknown unlock result type: %T", result)
	}
	switch n {
	case 2:
		// Partial release (still held)
		incUnlock("partial")
		return nil
	case 1:
		incUnlock("full")
		return nil
	case 0:
		incUnlock("not_held")
		return ErrLockNotHeld
	case -1:
		incUnlock("not_held")
		return ErrLockNotHeld
	default:
		incUnlock("error")
		return fmt.Errorf("unknown unlock result: %d", n)
	}
}

// NewLock creates a reusable lock instance (supports reentrancy within the same instance).
// Behavior:
// - Does not actively trigger locking, only builds RedisLock object; caller must explicitly call Acquire() to obtain or reenter lock.
// - Multiple Acquire calls on the same instance are treated as reentrant by the script due to unchanged value, and TTL is refreshed.
// - Redis Cluster: internal ownerKey and countKey use the same hashtag to ensure same slot for Lua atomic operations.
func NewLock(ctx context.Context, key string, options LockOptions) (*RedisLock, error) {
	options = normalizeLockOptions(options)
	if err := ValidateKey(key); err != nil {
		return nil, newLockError(ErrCodeInvalidOptions, "invalid lock key", err)
	}
	if err := options.Validate(); err != nil {
		return nil, newLockError(ErrCodeInvalidOptions, "invalid lock options", err)
	}
	provider, err := resolveRedisProvider()
	if err != nil {
		return nil, err
	}
	ownerKey, countKey := buildLockKeys(key)
	tokenKey := buildTokenKey(key)
	value := generateLockValue()
	lock := &RedisLock{
		provider:         provider,
		key:              key,
		value:            value,
		expiration:       options.Expiration,
		renewalThreshold: options.RenewalThreshold,
		ownerKey:         ownerKey,
		countKey:         countKey,
		tokenKey:         tokenKey,
	}
	return lock, nil
}

// LockWithRetry acquires lock and executes function, supports retry by strategy.
// - Based on DefaultLockOptions, overrides Expiration and RetryStrategy, others use defaults.
// - Uses random jitter (0.5~1.5x) during retries to reduce hot spot collisions.
func LockWithRetry(ctx context.Context, key string, expiration time.Duration, fn func() error, strategy RetryStrategy) error {
	options := DefaultLockOptions
	options.Expiration = expiration
	options.RetryStrategy = strategy
	return LockWithOptions(ctx, key, options, fn)
}

// LockWithOptions uses complete configuration options to acquire lock and execute callback function.
// Key behaviors:
// - Script calls can configure per-call timeout (options.ScriptCallTimeout). When set, cancel immediately after each call.
// - After successful acquisition, if renewal is enabled, register in global manager and start renewal service.
// - Release lock via defer before function returns. Release and status check (IsLocked) both use short timeout context to avoid blocking caller.
// - Unified partial release semantics: release script passes TTL=0 to not refresh TTL, only reduce count.
// Errors:
// - Acquisition failures due to contention will trigger OnLockAcquireFailed callback and decide whether to continue based on retry strategy.
func LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) (retErr error) {
	options = normalizeLockOptions(options)
	if fn == nil {
		return ErrLockFnRequired
	}
	if err := ValidateKey(key); err != nil {
		return newLockError(ErrCodeInvalidOptions, "invalid lock key", err)
	}
	if err := options.Validate(); err != nil {
		return newLockError(ErrCodeInvalidOptions, "invalid lock options", err)
	}
	provider, err := resolveRedisProvider()
	if err != nil {
		return err
	}
	value := generateLockValue()
	ownerKey, countKey := buildLockKeys(key)
	tokenKey := buildTokenKey(key)
	lock := &RedisLock{
		provider:         provider,
		key:              key,
		value:            value,
		expiration:       options.Expiration,
		renewalThreshold: options.RenewalThreshold,
		ownerKey:         ownerKey,
		countKey:         countKey,
		tokenKey:         tokenKey,
	}
	// Try to acquire lock; track total wait time for the wait_duration_seconds metric.
	waitStart := time.Now()
	for retries := 0; ; retries++ {
		if options.RetryStrategy.MaxRetries > 0 && retries >= options.RetryStrategy.MaxRetries {
			observeWaitDuration("timeout", time.Since(waitStart))
			return ErrMaxRetriesExceeded
		}
		if retries > 0 {
			// Jitter the delay so contending acquirers don't retry in lockstep.
			delay := timex.JitterAround(options.RetryStrategy.RetryDelay, 0.5)
			if !waitForContextDelay(ctx, delay) {
				observeWaitDuration("timeout", time.Since(waitStart))
				return ctx.Err()
			}
		}
		runCtx := ctx
		var cancel context.CancelFunc
		if to := options.ScriptCallTimeout; to > 0 {
			runCtx, cancel = context.WithTimeout(ctx, to)
		}
		client, err := lock.currentClient(runCtx)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			observeWaitDuration("error", time.Since(waitStart))
			return err
		}
		start := time.Now()
		result, err := lockScript.Run(runCtx, client, []string{lock.ownerKey, lock.countKey},
			lock.value, lock.expiration.Milliseconds()).Result()
		if cancel != nil {
			cancel()
		}
		observeScriptLatency("acquire", time.Since(start))
		if err != nil {
			incAcquire("error")
			observeWaitDuration("error", time.Since(waitStart))
			return fmt.Errorf("lock script execution failed: %w", err)
		}
		n, ok := result.(int64)
		if !ok {
			observeWaitDuration("error", time.Since(waitStart))
			return fmt.Errorf("unknown lock result type: %T", result)
		}
		if n > 0 {
			observeWaitDuration("success", time.Since(waitStart))
			// Single timestamp under mutex to avoid racing the renewal goroutine.
			now := time.Now()
			lock.mutex.Lock()
			lock.expiresAt = now.Add(lock.expiration)
			lock.acquiredAt = now
			lock.mutex.Unlock()
			incAcquire("success")
			currentCallback().OnLockAcquired(key, lock.expiration)
			if options.RenewalEnabled {
				addManagedLock(lock)
				globalLockManager.startRenewalService(options)
			}
			inManager := options.RenewalEnabled
			defer func() {
				// Release on an independent short-timeout context so a cancelled
				// business ctx cannot block best-effort release.
				to := options.ScriptCallTimeout
				if to <= 0 {
					to = DefaultLockOptions.ScriptCallTimeout
				}
				rctx, cancel := cleanupContext(to)
				start := time.Now()
				if releaseErr := lock.Release(rctx); releaseErr != nil {
					log.ErrorCtx(ctx, "failed to release redis lock", "error", releaseErr)
					if retErr == nil {
						retErr = releaseErr
					}
					if cancel != nil {
						cancel()
					}
					return
				}
				if cancel != nil {
					cancel()
				}
				observeScriptLatency("unlock", time.Since(start))
				// Only drop from the manager once fully released; a reentrant
				// partial release must keep being renewed.
				if inManager {
					cctx, ccancel := cleanupContext(to)
					stillHeld, checkErr := lock.IsLocked(cctx)
					if ccancel != nil {
						ccancel()
					}
					if checkErr != nil {
						// On an uncertain check, keep it managed so renewal isn't lost.
						log.ErrorCtx(ctx, "failed to check lock held after release", "error", checkErr)
						return
					}
					if !stillHeld {
						removeManagedLock(lock)
					}
				}
			}()
			retErr = fn()
			return retErr
		}
		incAcquire("conflict")
		currentCallback().OnLockAcquireFailed(key, ErrLockAcquireConflict)
		if options.RetryStrategy.MaxRetries == 0 {
			observeWaitDuration("conflict", time.Since(waitStart))
			return ErrLockAcquireConflict
		}
		continue
	}
}

func cleanupContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}
