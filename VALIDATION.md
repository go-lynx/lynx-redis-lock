# Validation

## Automated Baseline

Current workspace baseline:

```bash
go test ./... -count=1
go vet ./...
```

Result summary:

```text
ok   github.com/go-lynx/lynx-redis-lock    (passes unit tests)
go vet ./...                               (passes)
```

## What This Means

- The workspace now has committed unit tests for the renewal manager's retry delay behavior, including prompt exit when shutdown cancels the renewal context.
- The README documents API usage and operational boundaries, but there is still no Redis-backed automated coverage for acquire/release, reentrancy, Lua edge cases, or fencing token semantics.
- Before tightening guarantees further, add executable integration tests against a real Redis deployment for the Lua-script paths and long-running renewal flows.

## Recommended Manual Checks

- Initialize `lynx-redis`, then verify `Lock`, `LockWithOptions`, and `NewLock(...).Acquire/Release` against a real Redis deployment.
- Verify the same `*RedisLock` instance can reenter while a second instance on the same key cannot.
- Exercise renewal with a long-running critical section and inspect `GetStats()` / Prometheus metrics for `renew_total`, `skipped_renewals_total`, and `active_locks`.
- Verify `LockWithToken` produces monotonically increasing fencing tokens and that downstream consumers reject stale tokens if that protection is required.
