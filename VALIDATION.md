# Validation

## Automated Baseline

Current workspace baseline:

```bash
go test ./...
```

Result summary:

```text
?   	github.com/go-lynx/lynx-redis-lock    [no test files]
```

## What This Means

- This module currently has no committed Go test files in the workspace.
- The README documents API usage and operational boundaries, but there is no automated coverage for acquire/release, reentrancy, renewal, fencing token, or shutdown semantics yet.
- Before tightening guarantees in the docs, add executable tests for the renewal manager and Lua-script edge cases.

## Recommended Manual Checks

- Initialize `lynx-redis`, then verify `Lock`, `LockWithOptions`, and `NewLock(...).Acquire/Release` against a real Redis deployment.
- Verify the same `*RedisLock` instance can reenter while a second instance on the same key cannot.
- Exercise renewal with a long-running critical section and inspect `GetStats()` / Prometheus metrics for `renew_total`, `skipped_renewals_total`, and `active_locks`.
- Verify `LockWithToken` produces monotonically increasing fencing tokens and that downstream consumers reject stale tokens if that protection is required.
