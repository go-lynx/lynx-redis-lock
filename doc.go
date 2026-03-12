// Package redislock provides a Redis-based distributed lock for the Lynx framework.
//
// It supports standalone, Cluster, and Sentinel via redis.UniversalClient. Features include:
// atomic acquire/release/renew via Lua scripts, reentrancy per lock instance, optional
// auto-renewal, retry with jitter, and fencing token (see LockWithToken). For design
// limits (single-node vs Redlock, process pause, renewal failure), see LIMITATIONS.md.
package redislock
