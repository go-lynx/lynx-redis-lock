package redislock

import (
	"context"
	"testing"

	redisplug "github.com/go-lynx/lynx-redis"
	goredis "github.com/redis/go-redis/v9"
)

type fakeRedisProvider struct {
	client goredis.UniversalClient
}

func (p fakeRedisProvider) UniversalClient(context.Context) (goredis.UniversalClient, error) {
	return p.client, nil
}

func (p fakeRedisProvider) SingleClient(context.Context) (*goredis.Client, error) {
	if client, ok := p.client.(*goredis.Client); ok {
		return client, nil
	}
	return nil, nil
}

func (p fakeRedisProvider) Mode(context.Context) (string, error) {
	return "standalone", nil
}

var _ redisplug.Provider = fakeRedisProvider{}

func TestRedisLock_CurrentClientUsesProvider(t *testing.T) {
	raw := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	defer raw.Close()

	lock := &RedisLock{
		provider: fakeRedisProvider{client: raw},
		key:      "order-1",
	}
	client, err := lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected provider-backed client, got error: %v", err)
	}
	if client != raw {
		t.Fatalf("expected current client to come from provider")
	}
}

func TestRedisLock_CurrentClientFollowsProviderAfterSwap(t *testing.T) {
	first := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	defer first.Close()
	second := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6380"})
	defer second.Close()

	provider := &fakeRedisProvider{client: first}
	lock := &RedisLock{
		provider: provider,
		key:      "order-2",
	}

	client, err := lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected initial provider-backed client, got error: %v", err)
	}
	if client != first {
		t.Fatalf("expected first client, got %v", client)
	}

	provider.client = second

	client, err = lock.currentClient(context.Background())
	if err != nil {
		t.Fatalf("expected swapped provider-backed client, got error: %v", err)
	}
	if client != second {
		t.Fatalf("expected second client after provider swap, got %v", client)
	}
}
