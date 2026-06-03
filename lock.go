package redislock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-lynx/lynx/pkg/timex"
	"github.com/redis/go-redis/v9"
)

func (rl *RedisLock) currentClient(ctx context.Context) (redis.UniversalClient, error) {
	if rl == nil {
		return nil, fmt.Errorf("redis lock is nil")
	}
	if rl.provider == nil {
		return nil, ErrRedisClientNotFound
	}
	client, err := rl.provider.UniversalClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRedisClientNotFound, err)
	}
	if client == nil {
		return nil, ErrRedisClientNotFound
	}
	return client, nil
}

// GetKey returns the business lock key (as passed to NewLock / Lock).
func (rl *RedisLock) GetKey() string {
	return rl.key
}

// GetExpiration returns the configured lock TTL.
func (rl *RedisLock) GetExpiration() time.Duration {
	return rl.expiration
}

// GetExpiresAt returns the absolute expiration time (guarded by mutex).
func (rl *RedisLock) GetExpiresAt() time.Time {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	return rl.expiresAt
}

// GetAcquiredAt returns when the lock was acquired (guarded by mutex for consistency with renewal).
func (rl *RedisLock) GetAcquiredAt() time.Time {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	return rl.acquiredAt
}

// GetToken returns the most recently acquired fencing token (generated on non-reentrant acquisition).
// If 0, the lock has not been acquired for the first time in this process (or only reentry occurred).
// Fencing semantics: the resource layer must reject requests with an older token. See LIMITATIONS.md.
func (rl *RedisLock) GetToken() int64 {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	return rl.token
}

// GetRemainingTime returns the remaining TTL until expiry (guarded by mutex).
func (rl *RedisLock) GetRemainingTime() time.Duration {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	return time.Until(rl.expiresAt)
}

// GetStatus returns remaining TTL and whether the lock is already expired (single snapshot under mutex).
func (rl *RedisLock) GetStatus() (remainingTime time.Duration, isExpired bool) {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	now := time.Now()
	remainingTime = rl.expiresAt.Sub(now)
	isExpired = now.After(rl.expiresAt)
	return
}

// IsExpired reports whether the lock’s local expiry time has passed (guarded by mutex).
func (rl *RedisLock) IsExpired() bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	return time.Now().After(rl.expiresAt)
}

// Renew extends the lock TTL in Redis to newExpiration; caller must hold the lock.
func (rl *RedisLock) Renew(ctx context.Context, newExpiration time.Duration) error {
	client, err := rl.currentClient(ctx)
	if err != nil {
		currentCallback().OnLockRenewalFailed(rl.key, err)
		return err
	}
	// Set optional timeout for single call
	runCtx := ctx
	var cancel context.CancelFunc
	if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
		runCtx, cancel = context.WithTimeout(ctx, to)
	}
	start := time.Now()
	// Execute renewal script (avoid network calls while holding lock)
	result, err := renewScript.Run(runCtx, client, []string{rl.ownerKey, rl.countKey},
		rl.value, newExpiration.Milliseconds()).Result()
	if cancel != nil {
		cancel()
	}
	observeScriptLatency("renew", time.Since(start))
	if err != nil {
		currentCallback().OnLockRenewalFailed(rl.key, err)
		return fmt.Errorf("renewal script execution failed: %w", err)
	}

	n, ok := result.(int64)
	if !ok {
		err := fmt.Errorf("unknown renewal result type: %T", result)
		currentCallback().OnLockRenewalFailed(rl.key, err)
		return err
	}
	switch n {
	case 1: // Renewal successful
		now := time.Now()
		rl.mutex.Lock()
		rl.expiration = newExpiration
		rl.expiresAt = now.Add(newExpiration)
		rl.mutex.Unlock()
		currentCallback().OnLockRenewed(rl.key, newExpiration)
		return nil
	case 0, -1, -2: // Lock does not exist or not current holder
		currentCallback().OnLockRenewalFailed(rl.key, ErrLockRenewalFailed)
		return ErrLockRenewalFailed
	default:
		err := fmt.Errorf("unknown renewal result: %d", n)
		currentCallback().OnLockRenewalFailed(rl.key, err)
		return err
	}
}

// Release releases the lock (or one reentry count); returns ErrLockNotHeld if not owner or already released.
func (rl *RedisLock) Release(ctx context.Context) error {
	client, err := rl.currentClient(ctx)
	if err != nil {
		return err
	}
	// Set optional timeout for single call
	runCtx := ctx
	var cancel context.CancelFunc
	if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
		runCtx, cancel = context.WithTimeout(ctx, to)
	}
	start := time.Now()
	// Execute unlock script (unified semantics: partial release does not refresh TTL, pass 0)
	result, err := unlockScript.Run(runCtx, client, []string{rl.ownerKey, rl.countKey}, rl.value, int64(0)).Result()
	if cancel != nil {
		cancel()
	}
	observeScriptLatency("unlock", time.Since(start))
	if err != nil {
		return fmt.Errorf("unlock script execution failed: %w", err)
	}

	// Check if lock was successfully released
	n, ok := result.(int64)
	if !ok {
		return fmt.Errorf("unknown unlock result type: %T", result)
	}
	switch n {
	case 2: // Partial release (still held)
		incUnlock("partial")
		return nil
	case 1: // Fully released lock
		incUnlock("full")
		duration := time.Since(rl.acquiredAt)
		removeManagedLock(rl)
		currentCallback().OnLockReleased(rl.key, duration)
		return nil
	case 0: // Lock does not exist
		incUnlock("not_held")
		return ErrLockNotHeld
	case -1: // Lock exists but not current holder
		incUnlock("not_held")
		return ErrLockNotHeld
	default:
		incUnlock("error")
		return fmt.Errorf("unknown unlock result: %d", n)
	}
}

// IsLocked returns whether the current instance holds the lock in Redis (by value match).
func (rl *RedisLock) IsLocked(ctx context.Context) (bool, error) {
	client, err := rl.currentClient(ctx)
	if err != nil {
		return false, err
	}
	// Set optional timeout for single call
	runCtx := ctx
	var cancel context.CancelFunc
	if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
		runCtx, cancel = context.WithTimeout(ctx, to)
	}
	value, err := client.Get(runCtx, rl.ownerKey).Result()
	if cancel != nil {
		cancel()
	}
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == rl.value, nil
}

// Acquire attempts to acquire (or reenter) the lock based on the current RedisLock instance
// If called again on the same instance, the Lua script will treat it as reentrant and renew
// because the value remains unchanged
func (rl *RedisLock) Acquire(ctx context.Context) error {
	client, err := rl.currentClient(ctx)
	if err != nil {
		currentCallback().OnLockAcquireFailed(rl.key, err)
		return err
	}
	// Set optional timeout for single call
	runCtx := ctx
	var cancel context.CancelFunc
	if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
		runCtx, cancel = context.WithTimeout(ctx, to)
	}
	start := time.Now()
	result, err := lockScript.Run(runCtx, client, []string{rl.ownerKey, rl.countKey}, rl.value, rl.expiration.Milliseconds()).Result()
	if cancel != nil {
		cancel()
	}
	observeScriptLatency("acquire", time.Since(start))
	if err != nil {
		incAcquire("error")
		return fmt.Errorf("lock script execution failed: %w", err)
	}

	n, ok := result.(int64)
	if !ok {
		incAcquire("error")
		return fmt.Errorf("unknown lock result type: %T", result)
	}
	if n > 0 {
		now := time.Now()
		rl.mutex.Lock()
		rl.acquiredAt = now
		rl.expiresAt = now.Add(rl.expiration)
		rl.mutex.Unlock()
		// If this is the first acquisition (non-reentrant), generate and record fencing token
		if n == 1 {
			// A single command is sufficient since no other holder can exist while holding the lock
			// Optional timeout
			tctx := ctx
			var tcancel context.CancelFunc
			if to := DefaultLockOptions.ScriptCallTimeout; to > 0 {
				tctx, tcancel = context.WithTimeout(ctx, to)
			}
			token, err := client.Incr(tctx, rl.tokenKey).Result()
			if tcancel != nil {
				tcancel()
			}
			if err != nil {
				incAcquire("error")
				releaseCtx, releaseCancel := cleanupContext(DefaultLockOptions.ScriptCallTimeout)
				releaseErr := rl.Release(releaseCtx)
				releaseCancel()
				if releaseErr != nil {
					return fmt.Errorf("fencing token generation failed: %w; best-effort release failed: %v", err, releaseErr)
				}
				return fmt.Errorf("fencing token generation failed: %w", err)
			}
			rl.mutex.Lock()
			rl.token = token
			rl.mutex.Unlock()
		}
		incAcquire("success")
		currentCallback().OnLockAcquired(rl.key, rl.expiration)
		return nil
	}
	// Occupied by another holder
	incAcquire("conflict")
	currentCallback().OnLockAcquireFailed(rl.key, ErrLockAcquireConflict)
	return ErrLockAcquireConflict
}

// AcquireWithRetry acquires (or reenters) the lock and retries according to strategy
func (rl *RedisLock) AcquireWithRetry(ctx context.Context, strategy RetryStrategy) error {
	retries := 0
	for {
		if strategy.MaxRetries > 0 && retries >= strategy.MaxRetries {
			return ErrMaxRetriesExceeded
		}
		if retries > 0 {
			// Add jitter to avoid hot spot collisions
			delay := timex.JitterAround(strategy.RetryDelay, 0.5)
			if !waitForContextDelay(ctx, delay) {
				return ctx.Err()
			}
		}
		err := rl.Acquire(ctx)
		if err == nil {
			return nil
		}
		if err != ErrLockAcquireConflict {
			return err
		}
		// Continue retrying according to strategy on conflict
		if strategy.MaxRetries == 0 {
			return ErrLockAcquireConflict
		}
		retries++
	}
}

// EnableAutoRenew registers the current lock to the global renewal manager (starts if not already started)
func (rl *RedisLock) EnableAutoRenew(options LockOptions) {
	addManagedLock(rl)
	globalLockManager.startRenewalService(options)
}
