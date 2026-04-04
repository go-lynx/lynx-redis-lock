package redislock

import (
	"context"
	"fmt"
	"time"

	redisplug "github.com/go-lynx/lynx-redis"
)

// Provider exposes redis-lock through an injectable facade while resolving the underlying
// Redis client via lynx-redis's stable provider on each call.
type Provider interface {
	NewLock(ctx context.Context, key string, options LockOptions) (*RedisLock, error)
	Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error
	LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error
	LockWithToken(ctx context.Context, key string, expiration time.Duration, fn func(token int64) error) error
	UnlockByValue(ctx context.Context, key, value string) error
}

type provider struct{}

// GetProvider returns the injectable redis lock facade.
func GetProvider() Provider {
	return provider{}
}

func (provider) NewLock(ctx context.Context, key string, options LockOptions) (*RedisLock, error) {
	return NewLock(ctx, key, options)
}

func (provider) Lock(ctx context.Context, key string, expiration time.Duration, fn func() error) error {
	return Lock(ctx, key, expiration, fn)
}

func (provider) LockWithOptions(ctx context.Context, key string, options LockOptions, fn func() error) error {
	return LockWithOptions(ctx, key, options, fn)
}

func (provider) LockWithToken(ctx context.Context, key string, expiration time.Duration, fn func(token int64) error) error {
	return LockWithToken(ctx, key, expiration, fn)
}

func (provider) UnlockByValue(ctx context.Context, key, value string) error {
	return UnlockByValue(ctx, key, value)
}

func resolveRedisProvider() (redisplug.Provider, error) {
	provider := redisplug.GetProvider()
	if provider == nil {
		return nil, fmt.Errorf("%w: redis provider not found", ErrRedisClientNotFound)
	}
	return provider, nil
}
