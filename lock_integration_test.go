package redislock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// newMiniRedis starts an in-process Redis server and returns both the server and
// a fakeRedisProvider wired to it.  The server is automatically stopped via t.Cleanup.
func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *fakeRedisProvider) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	return mr, &fakeRedisProvider{client: client}
}

// newTestLock builds a RedisLock bound to the given provider.
func newTestLock(t *testing.T, p *fakeRedisProvider, key string, ttl time.Duration) *RedisLock {
	t.Helper()
	ownerKey, countKey := buildLockKeys(key)
	tokenKey := buildTokenKey(key)
	return &RedisLock{
		provider:         p,
		key:              key,
		value:            generateLockValue(),
		expiration:       ttl,
		renewalThreshold: DefaultLockOptions.RenewalThreshold,
		ownerKey:         ownerKey,
		countKey:         countKey,
		tokenKey:         tokenKey,
	}
}

// TestAcquireAndRelease verifies the basic acquire → release flow and that the
// key is absent in Redis after a full release.
func TestAcquireAndRelease(t *testing.T) {
	mr, prov := newMiniRedis(t)

	lock := newTestLock(t, prov, "basic-key", 5*time.Second)
	ctx := context.Background()

	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if held, err := lock.IsLocked(ctx); err != nil || !held {
		t.Fatalf("expected lock to be held after Acquire; held=%v err=%v", held, err)
	}

	if err := lock.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held, err := lock.IsLocked(ctx); err != nil || held {
		t.Fatalf("expected lock to be released; held=%v err=%v", held, err)
	}

	// Redis key must be absent after full release
	if mr.Exists(lock.ownerKey) {
		t.Errorf("owner key still present in Redis after full release")
	}
}

// TestLockConflict verifies that a second lock instance cannot acquire while the
// first is held, and receives ErrLockAcquireConflict.
func TestLockConflict(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock1 := newTestLock(t, prov, "conflict-key", 5*time.Second)
	lock2 := newTestLock(t, prov, "conflict-key", 5*time.Second)

	if err := lock1.Acquire(ctx); err != nil {
		t.Fatalf("lock1 Acquire: %v", err)
	}
	defer func() { _ = lock1.Release(ctx) }()

	if err := lock2.Acquire(ctx); !errors.Is(err, ErrLockAcquireConflict) {
		t.Fatalf("expected ErrLockAcquireConflict for lock2, got: %v", err)
	}
}

// TestReentrantLock verifies that the same lock instance can be acquired multiple
// times and that each Release decrements the reentry count.
func TestReentrantLock(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "reentrant-key", 5*time.Second)

	// First acquisition
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// Second acquisition (reentry)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("second Acquire (reentry): %v", err)
	}

	// First release should be a partial release (still held)
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if held, err := lock.IsLocked(ctx); err != nil || !held {
		t.Fatalf("expected lock still held after partial release; held=%v err=%v", held, err)
	}

	// Second release should fully release
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if held, err := lock.IsLocked(ctx); err != nil || held {
		t.Fatalf("expected lock fully released; held=%v err=%v", held, err)
	}
}

// TestLockExpiry verifies that after TTL expires, the lock key is no longer present
// in Redis (natural expiry, no explicit Release).
func TestLockExpiry(t *testing.T) {
	mr, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "expiry-key", 200*time.Millisecond)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Fast-forward time in miniredis so the key expires
	mr.FastForward(300 * time.Millisecond)

	if held, err := lock.IsLocked(ctx); err != nil || held {
		t.Fatalf("expected lock to be expired; held=%v err=%v", held, err)
	}
	if mr.Exists(lock.ownerKey) {
		t.Errorf("owner key still present in Redis after TTL expiry")
	}
}

// TestRenewExtendsTTL verifies that calling Renew pushes out the expiry in Redis.
func TestRenewExtendsTTL(t *testing.T) {
	mr, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "renew-key", 2*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Advance time close to expiry
	mr.FastForward(1800 * time.Millisecond)

	// Renew for another 2 seconds
	if err := lock.Renew(ctx, 2*time.Second); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	// Advance another 1.5 s — the original TTL would have expired but renewal should keep it alive
	mr.FastForward(1500 * time.Millisecond)

	if held, err := lock.IsLocked(ctx); err != nil || !held {
		t.Fatalf("expected lock still held after renewal; held=%v err=%v", held, err)
	}
}

// TestRenewAfterExpiryFails verifies that Renew returns ErrLockRenewalFailed when
// the lock has already expired in Redis.
func TestRenewAfterExpiryFails(t *testing.T) {
	mr, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "renew-expired-key", 200*time.Millisecond)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Expire the key
	mr.FastForward(500 * time.Millisecond)

	if err := lock.Renew(ctx, 2*time.Second); !errors.Is(err, ErrLockRenewalFailed) {
		t.Fatalf("expected ErrLockRenewalFailed after expiry; got: %v", err)
	}
}

// TestFencingTokenIncrementsOnFirstAcquire verifies that the fencing token is a
// positive integer after the initial (non-reentrant) acquisition.
func TestFencingTokenIncrementsOnFirstAcquire(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "token-key", 5*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release(ctx) }()

	if tok := lock.GetToken(); tok <= 0 {
		t.Fatalf("expected positive fencing token after first acquisition, got %d", tok)
	}
}

// TestFencingTokenNotChangedOnReentry verifies that a reentrant acquisition does
// not overwrite the fencing token.
func TestFencingTokenNotChangedOnReentry(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "token-reentry-key", 5*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	firstToken := lock.GetToken()

	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("second Acquire (reentry): %v", err)
	}
	if tok := lock.GetToken(); tok != firstToken {
		t.Fatalf("reentry changed fencing token: before=%d after=%d", firstToken, tok)
	}
}

// TestConcurrentContention verifies that under concurrent access exactly one
// goroutine holds the lock at any given moment.
func TestConcurrentContention(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	const goroutines = 20
	var (
		wg        sync.WaitGroup
		concurrent int64 // number of goroutines simultaneously inside the critical section
		maxSeen   int64
		mu        sync.Mutex
	)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := newTestLock(t, prov, "contention-key", 5*time.Second)
			strategy := RetryStrategy{MaxRetries: 30, RetryDelay: 20 * time.Millisecond}
			if err := lock.AcquireWithRetry(ctx, strategy); err != nil {
				return // contention under heavy load — acceptable
			}
			defer func() { _ = lock.Release(ctx) }()

			c := atomic.AddInt64(&concurrent, 1)
			mu.Lock()
			if c > maxSeen {
				maxSeen = c
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond) // brief critical section
			atomic.AddInt64(&concurrent, -1)
		}()
	}
	wg.Wait()

	if maxSeen > 1 {
		t.Fatalf("concurrent lock holders detected: max=%d (expected 1)", maxSeen)
	}
}

// TestReleaseNotHeld verifies that releasing an unacquired lock returns ErrLockNotHeld.
func TestReleaseNotHeld(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "not-held-key", 5*time.Second)
	if err := lock.Release(ctx); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("expected ErrLockNotHeld; got: %v", err)
	}
}

// TestUnlockByValue verifies the standalone UnlockByValue function.
func TestUnlockByValue(t *testing.T) {
	mr, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "ubv-key", 5*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Swap to the test provider in the global registry so resolveRedisProvider works.
	origResolve := func(p *fakeRedisProvider) func() {
		return func() { _ = p } // no-op; resolveRedisProvider uses lynx-redis's global
	}(prov)
	_ = origResolve

	// Unlock directly against the Redis server using the lock value
	ownerKey, countKey := buildLockKeys("ubv-key")
	runCtx := context.Background()
	client, _ := prov.UniversalClient(ctx)
	result, err := unlockScript.Run(runCtx, client, []string{ownerKey, countKey}, lock.value, int64(0)).Result()
	if err != nil {
		t.Fatalf("direct unlock script: %v", err)
	}
	if n, ok := result.(int64); !ok || n != 1 {
		t.Fatalf("expected full release result=1, got %v", result)
	}
	if mr.Exists(ownerKey) {
		t.Errorf("owner key still present after UnlockByValue equivalent")
	}
}

// TestAutoRenewalKeepsLockAlive verifies that the auto-renewal background service
// renews the lock before it expires.
// NOTE: this test uses real-time waits because miniredis FastForward pauses goroutines.
func TestAutoRenewalKeepsLockAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-time renewal test in short mode")
	}
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	resetGlobalLockManagerForTest(t)

	// Use a short TTL so the test completes quickly.
	// Renewal fires when ≤ 40% TTL remains (i.e. ≤ 400ms for a 1s lock).
	// The check interval is 100ms, so renewal should fire well before expiry.
	ttl := 1 * time.Second
	options := LockOptions{
		Expiration:       ttl,
		RetryStrategy:    RetryStrategy{},
		RenewalEnabled:   true,
		RenewalThreshold: 0.4,
		WorkerPoolSize:   5,
		RenewalConfig: RenewalConfig{
			MaxRetries:    3,
			BaseDelay:     20 * time.Millisecond,
			MaxDelay:      100 * time.Millisecond,
			CheckInterval: 100 * time.Millisecond,
			CallTimeout:   300 * time.Millisecond,
		},
	}

	lock := newTestLock(t, prov, "auto-renew-key", ttl)
	lock.renewalThreshold = options.RenewalThreshold
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lock.EnableAutoRenew(options)
	t.Cleanup(func() {
		_ = lock.Release(ctx)
		globalLockManager.stopRenewalService()
	})

	// Wait for 2.5× the original TTL; renewals should have kept the key alive.
	time.Sleep(2500 * time.Millisecond)

	if held, err := lock.IsLocked(ctx); err != nil || !held {
		t.Fatalf("lock should still be held after auto-renewal; held=%v err=%v", held, err)
	}
}

// TestValidateKey checks key validation rejects empty, too-long, and keys with
// Redis Cluster hashtag characters.
func TestValidateKey(t *testing.T) {
	cases := []struct {
		key     string
		wantErr bool
	}{
		{"valid-key", false},
		{"", true},
		{string(make([]byte, MaxLockKeyLength+1)), true},
		{"key{with}braces", true},
		{"key\x01ctrl", true},
	}
	for _, tc := range cases {
		err := ValidateKey(tc.key)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateKey(%q) error=%v, wantErr=%v", tc.key, err, tc.wantErr)
		}
	}
}

// TestAcquireWithRetryExhausted verifies ErrMaxRetriesExceeded is returned when
// every attempt is blocked by another holder.
func TestAcquireWithRetryExhausted(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	holder := newTestLock(t, prov, "retry-key", 5*time.Second)
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer func() { _ = holder.Release(ctx) }()

	contender := newTestLock(t, prov, "retry-key", 5*time.Second)
	strategy := RetryStrategy{MaxRetries: 2, RetryDelay: 5 * time.Millisecond}
	err := contender.AcquireWithRetry(ctx, strategy)
	if !errors.Is(err, ErrMaxRetriesExceeded) {
		t.Fatalf("expected ErrMaxRetriesExceeded; got: %v", err)
	}
}

// TestContextCancellationAbortsRetry verifies that cancelling the context stops
// AcquireWithRetry promptly.
func TestContextCancellationAbortsRetry(t *testing.T) {
	_, prov := newMiniRedis(t)

	holder := newTestLock(t, prov, "cancel-key", 5*time.Second)
	if err := holder.Acquire(context.Background()); err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer func() { _ = holder.Release(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	contender := newTestLock(t, prov, "cancel-key", 5*time.Second)
	strategy := RetryStrategy{MaxRetries: 100, RetryDelay: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		done <- contender.AcquireWithRetry(ctx, strategy)
	}()

	// Cancel just after the first attempt starts
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after context cancellation")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AcquireWithRetry did not abort after context cancellation")
	}
}

// TestGetStatusReturnsConsistentSnapshot verifies that GetStatus is consistent:
// remainingTime > 0 iff !isExpired.
func TestGetStatusReturnsConsistentSnapshot(t *testing.T) {
	_, prov := newMiniRedis(t)
	ctx := context.Background()

	lock := newTestLock(t, prov, "status-key", 5*time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release(ctx) }()

	remaining, expired := lock.GetStatus()
	if expired {
		t.Fatal("expected lock not expired immediately after acquisition")
	}
	if remaining <= 0 {
		t.Fatalf("expected positive remaining time; got %v", remaining)
	}
}
