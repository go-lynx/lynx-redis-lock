# Redis Distributed Lock Plugin for Lynx Framework

The Redis Distributed Lock Plugin provides a robust, high-performance distributed locking mechanism for the Lynx framework using Redis as the coordination backend. It supports automatic renewal, retry mechanisms, reentrancy (same instance), and works with **standalone, Cluster, and Sentinel** via `redis.UniversalClient`.

**Design and limitations:** See [LIMITATIONS.md](./LIMITATIONS.md) for single-node vs Redlock, process pause/TTL, fencing token usage, renewal failure behavior, and shutdown/script timeout.

## Features

### Core Locking Capabilities
- **Distributed Locking**: Redis-based distributed lock implementation
- **Automatic Renewal**: Configurable automatic lock renewal to prevent expiration
- **Retry Mechanisms**: Intelligent retry logic with exponential backoff
- **Lock Timeout**: Configurable lock expiration and timeout handling

### Advanced Features
- **Reentrant Locks**: Reentrancy by reusing the same `*RedisLock` instance (multiple `Acquire`/`Release` on one instance)
- **Lock Monitoring**: Real-time lock status monitoring and statistics
- **Graceful Shutdown**: Proper cleanup and resource management
- **Performance Optimization**: High-performance lock operations with minimal overhead
- **Error Handling**: Comprehensive error handling and recovery mechanisms

### Monitoring & Observability
- **Prometheus Metrics**: Comprehensive monitoring and alerting
- **Health Checks**: Real-time lock health monitoring
- **Performance Analytics**: Lock acquisition and release performance metrics
- **Error Tracking**: Detailed error categorization and reporting
- **Statistics Collection**: Lock usage statistics and performance data

## Architecture

The plugin follows the Lynx framework's layered architecture:

```
┌─────────────────────────────────────────────────────────────┐
│                    Application Layer                        │
├─────────────────────────────────────────────────────────────┤
│                    Lock Plugin Layer                        │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │   Client    │  │   Manager   │  │   Configuration    │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│                    Lock Management Layer                    │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │   Lock      │  │   Renewal   │  │   Retry Logic      │ │
│  │   Instance  │  │   Service   │  │     Handler        │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│                    Redis Layer                             │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │   Lua       │  │   Redis     │  │   Connection       │ │
│  │   Scripts   │  │   Client    │  │     Pool           │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

## Configuration

### Basic Configuration

```yaml
lynx:
  redis:
    # Redis connection configuration
    addrs: ["localhost:6379"]
    password: ""
    db: 0
    
    # Lock configuration
    lock:
      default_timeout: 30s
      default_retry_interval: 100ms
      max_retries: 3
      renewal_enabled: true
      renewal_threshold: 0.5
      renewal_interval: 10s
```

### Advanced Configuration

```yaml
lynx:
  redis:
    addrs: ["redis1:6379", "redis2:6379", "redis3:6379"]
    password: "your-redis-password"
    db: 0
    
    # Connection pool configuration
    pool:
      max_active: 100
      max_idle: 10
      idle_timeout: 300s
      max_conn_lifetime: 3600s
    
    # Lock configuration
    lock:
      default_timeout: 60s
      default_retry_interval: 200ms
      max_retries: 5
      renewal_enabled: true
      renewal_threshold: 0.3
      renewal_interval: 5s
      
      # Retry strategy
      retry_strategy:
        type: "exponential_backoff"
        initial_interval: 100ms
        max_interval: 5s
        multiplier: 2.0
        max_elapsed_time: 30s
      
      # Monitoring
      monitoring:
        enable_metrics: true
        metrics_path: "/metrics"
        health_check_interval: 30s
```

## Usage

### Basic Usage

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"
    "github.com/go-lynx/lynx-redis-lock"
)

func main() {
    err := redislock.Lock(context.Background(), "my-lock", 30*time.Second, func() error {
        // Critical section - your business logic here
        fmt.Println("Executing critical section")
        time.Sleep(5 * time.Second)
        return nil
    })

    if err != nil {
        log.Printf("Failed to acquire lock: %v", err)
    }
}
```

### Advanced Usage with Options

```go
// Configure lock options
options := redislock.LockOptions{
    Expiration:       60 * time.Second,
    RetryStrategy:    redislock.RetryStrategy{MaxRetries: 3, RetryDelay: 100 * time.Millisecond},
    RenewalEnabled:   true,
    RenewalThreshold: 0.5,
}

err := redislock.LockWithOptions(context.Background(), "my-lock", options, func() error {
    // Long-running critical section
    fmt.Println("Executing long-running critical section")
    time.Sleep(30 * time.Second)
    return nil
})
```

### Manual Lock Management

```go
// Create a reusable lock instance and acquire it (works with standalone/Cluster/Sentinel via GetUniversalRedis)
options := redislock.LockOptions{Expiration: 30 * time.Second}
lock, err := redislock.NewLock(ctx, "my-lock", options)
if err != nil {
    return err
}
if err := lock.Acquire(ctx); err != nil {
    return err
}
defer lock.Release(ctx)

// Check if current instance holds the lock
held, err := lock.IsLocked(ctx)
if err == nil && held {
    fmt.Println("Lock is held")
}

// Optional: manual renewal
_ = lock.Renew(ctx, 30*time.Second)
```

### Reentrant Locks

Reentrancy is **per lock instance**: use one `*RedisLock` and call `Acquire` multiple times (and the same number of `Release`). Each new `Lock()` or `NewLock()` creates a different instance and different holder identity, so they do **not** reenter.

```go
options := redislock.LockOptions{Expiration: 30 * time.Second}
lock, _ := redislock.NewLock(ctx, "my-lock", options)
_ = lock.Acquire(ctx)
defer lock.Release(ctx)
// Reenter with the same instance
_ = lock.Acquire(ctx)
defer lock.Release(ctx)
// Critical section
```

Or use `LockWithToken` and inside the callback use the same `lock` for nested work if you need the fencing token in the callback.

## API Reference

### Core Functions

- `Lock(ctx, key, expiration, fn) error` - Acquire lock and execute function (uses default options).
- `LockWithOptions(ctx, key, options, fn) error` - Acquire lock with full options.
- `LockWithRetry(ctx, key, expiration, fn, strategy) error` - Acquire with custom retry strategy.
- `LockWithToken(ctx, key, expiration, fn func(token int64) error) error` - Acquire and run callback with fencing token (see LIMITATIONS.md).
- `NewLock(ctx, key, options) (*RedisLock, error)` - Create a reusable lock instance (then call `Acquire`/`Release`); reentrancy is per instance.
- `UnlockByValue(ctx, key, value) error` - Release by key and value (e.g. from another process that stored the value).

### Lock Instance Methods

- `Acquire(ctx) error` - Acquire or reenter (same instance).
- `AcquireWithRetry(ctx, strategy) error` - Acquire with retries.
- `Release(ctx) error` - Release (or partial release when reentrant).
- `Renew(ctx, newExpiration) error` - Manually extend TTL.
- `IsLocked(ctx) (bool, error)` - Whether the current instance holds the lock.
- `GetKey() string`, `GetExpiration() time.Duration`, `GetExpiresAt() time.Time`, `GetToken() int64` - Status accessors.

### Configuration Options

```go
type LockOptions struct {
    Expiration       time.Duration
    RetryStrategy     RetryStrategy  // MaxRetries, RetryDelay
    RenewalEnabled     bool
    RenewalThreshold   float64
    WorkerPoolSize     int
    RenewalConfig      RenewalConfig
    ScriptCallTimeout  time.Duration
}
```

## Monitoring and Metrics

### Statistics

```go
stats := redislock.GetStats()
// total_locks, active_locks, renewal_count, renewal_errors, skipped_renewals, etc.
log.Printf("Active locks: %d, Renewal count: %d", stats["active_locks"], stats["renewal_count"])
```

### Graceful shutdown

```go
// Stop renewal service and wait for active locks to drop to zero (or timeout)
if err := redislock.Shutdown(ctx); err != nil {
    log.Printf("Shutdown: %v", err)
}
```
See [LIMITATIONS.md](./LIMITATIONS.md) for shutdown semantics (no forced release in Redis).

### Prometheus Metrics

The plugin exposes comprehensive Prometheus metrics:

#### Lock Metrics
- `lynx_redis_lock_acquisitions_total` - Total lock acquisitions
- `lynx_redis_lock_releases_total` - Total lock releases
- `lynx_redis_lock_errors_total` - Total lock errors
- `lynx_redis_lock_duration_seconds` - Lock hold duration
- `lynx_redis_lock_active` - Currently active locks

#### Retry Metrics
- `lynx_redis_lock_retries_total` - Total retry attempts
- `lynx_redis_lock_retry_duration_seconds` - Retry duration
- `lynx_redis_lock_retry_failures_total` - Retry failures

#### Renewal Metrics
- `lynx_redis_lock_renewals_total` - Total lock renewals
- `lynx_redis_lock_renewal_errors_total` - Renewal errors
- `lynx_redis_lock_expirations_total` - Lock expirations

## Performance Tuning

### Lock Configuration

```yaml
lynx:
  redis:
    lock:
      # Optimize for high concurrency
      default_timeout: 10s
      max_retries: 5
      retry_interval: 50ms
      
      # Enable renewal for long operations
      renewal_enabled: true
      renewal_threshold: 0.3
      renewal_interval: 3s
      
      # Retry strategy
      retry_strategy:
        type: "exponential_backoff"
        initial_interval: 50ms
        max_interval: 2s
        multiplier: 1.5
```

### Redis Configuration

```yaml
lynx:
  redis:
    # Use Redis cluster for high availability
    addrs: ["redis1:6379", "redis2:6379", "redis3:6379"]
    
    # Optimize connection pool
    pool:
      max_active: 200
      max_idle: 50
      idle_timeout: 300s
```

## Troubleshooting

### Common Issues

1. **Lock Acquisition Failures**
   - Check Redis connectivity
   - Verify lock key uniqueness
   - Review timeout settings

2. **Lock Not Releasing**
   - Check for panic in critical section
   - Verify proper error handling
   - Monitor lock expiration

3. **Performance Issues**
   - Optimize lock timeout settings
   - Review retry configuration
   - Monitor Redis performance

4. **Deadlock Issues**
   - Use consistent lock ordering
   - Implement lock timeouts
   - Monitor lock statistics

### Debug Mode

Enable debug logging for detailed troubleshooting:

```yaml
lynx:
  redis:
    lock:
      logging:
        level: "DEBUG"
        enable_metrics: true
```

## Best Practices

### Lock Design
- Use descriptive lock keys
- Keep critical sections short
- Implement proper error handling
- Use appropriate timeouts

### Performance
- Optimize lock duration
- Use connection pooling
- Monitor lock statistics
- Implement circuit breakers

### Reliability
- Handle lock failures gracefully
- Implement retry logic
- Use lock renewal for long operations
- Monitor lock health

### Security
- Use secure Redis connections
- Implement proper authentication
- Validate lock keys
- Monitor lock usage

## Dependencies

- `github.com/redis/go-redis/v9` - Redis client (lock uses `UniversalClient` for standalone/cluster/sentinel)
- `github.com/go-lynx/lynx` - Lynx framework core
- `github.com/go-lynx/lynx-redis` - Redis plugin (provides `GetUniversalRedis()`)
- `github.com/prometheus/client_golang` - Prometheus metrics

## License

This plugin is part of the Lynx framework and follows the same license terms.

## Contributing

Contributions are welcome! Please see the main Lynx framework contribution guidelines.

## Support

For support and questions:
- GitHub Issues: [Lynx Framework Issues](https://github.com/go-lynx/lynx/issues)
- Documentation: [Lynx Documentation](https://lynx.go-lynx.com)
- Community: [Lynx Community](https://community.go-lynx.com)