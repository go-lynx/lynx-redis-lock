# Limitations

## Scope of the Lock

- This implementation uses Redis Lua scripts plus a single `redis.UniversalClient` selected from `lynx-redis`; it is not a quorum-based Redlock implementation.
- Under Redis Cluster, the owner/count/token keys share the same hash tag so the scripts stay atomic on one slot, but the lock still relies on a single primary for correctness.
- If your failure model requires quorum locking across independent Redis masters, that behavior still needs implementation before the README can promise it.

## TTL and Process Suspension

- Auto-renew runs in background goroutines inside the current process. If the process is paused by a long GC stop-the-world event, debugger breakpoint, host suspension, or CPU starvation, Redis TTL keeps advancing while renewal stops.
- When that happens, the lock can expire even though the original holder is still running after resume. Keep critical sections idempotent and short enough to tolerate TTL loss.

## Fencing Tokens

- `LockWithToken` and `GetToken()` only provide value if the downstream protected resource enforces monotonic token checks.
- The lock package does not enforce fencing at the storage or business-resource layer for you. If the downstream system ignores the token, stale holders can still write after lease loss.

## Renewal Failure Behavior

- On repeated renewal failure, the manager removes the lock from its in-memory renewal set and stops retrying. It does not force an immediate unlock in Redis.
- After that point, the key expires naturally according to remaining TTL. Callers must treat renewal failure as potential lease loss and design callbacks to stop safely or reject stale work.

## Shutdown and Timeouts

- `Shutdown(ctx)` stops the renewal service in-process; it does not scan Redis and force-release remote keys.
- `ScriptCallTimeout` and `RenewalConfig.CallTimeout` only bound client-side Redis script calls. They do not extend TTL or change Redis-side expiration semantics.

## Configuration Surface

- There is no dedicated YAML section such as `lynx.redis.lock` for lock behavior. Runtime Redis connectivity comes from `lynx-redis`, while lock semantics are controlled through `LockOptions` in code.
- Lock keys must pass `ValidateKey`: non-empty, printable ASCII only, maximum length 255, and no `{` or `}`.
