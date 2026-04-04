package redislock

import (
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
	go func() {
		resultCh <- lm.waitForRetryDelay(5 * time.Second)
	}()

	time.Sleep(30 * time.Millisecond)
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
