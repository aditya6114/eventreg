// Package cache wraps the Redis client.
//
// ============================ WHAT REDIS IS (interview) ====================
//
// An IN-MEMORY key-value store. Data lives in RAM, not on disk, which is the
// whole reason it's fast: a RAM read is ~100 nanoseconds, an SSD read is
// ~100 microseconds — roughly 1000x slower. Typical Redis operations complete
// in well under a millisecond, and the network round-trip usually costs more
// than the operation itself.
//
// WHY SINGLE-THREADED IS A FEATURE, NOT A LIMITATION
//
// Redis executes commands on ONE thread, one at a time. That sounds like a
// bottleneck, and it's the detail interviewers probe. Three reasons it works:
//
//  1. EVERY COMMAND IS ATOMIC, FOR FREE. There is no concurrency inside Redis,
//     so no locks, no mutexes, no race conditions between commands. INCR
//     genuinely cannot lose an update. This is exactly the lesson-4 problem —
//     and Redis solves it by not having concurrency at all.
//  2. NO LOCK OVERHEAD. Locking, context switching, and cache-line contention
//     are a large share of a multithreaded server's cost. Redis pays none of it.
//  3. IT ISN'T CPU-BOUND ANYWAY. Operations are simple memory accesses; the
//     limit is network I/O and RAM bandwidth, not compute. Adding threads
//     wouldn't help.
//
// The real consequence to remember: ONE SLOW COMMAND BLOCKS EVERYTHING. This is
// why `KEYS *` is effectively banned in production — it scans the entire
// keyspace on that single thread while every other client waits. (Use SCAN,
// which is incremental; or design your keys so you never need to search them,
// as we do below.)
//
// ==================== REDIS vs POSTGRES: WHEN TO USE WHICH ==================
//
//	                    Postgres                    Redis
//	Durability          ACID, survives a crash      in-memory, may lose data
//	Query power         SQL: joins, aggregates      lookup by key
//	Speed               ~1-10ms                     ~0.1ms
//	Data size           limited by disk             limited by RAM (expensive)
//	Relationships       foreign keys, constraints   none
//
// THE RULE: Postgres is the SYSTEM OF RECORD — the truth, the thing you'd cry
// about losing. Redis holds data that is either DERIVED (a cache you can
// rebuild) or DELIBERATELY EPHEMERAL (a rate-limit counter, a 5-minute seat
// hold). If losing a Redis key means losing information you can't reconstruct,
// it was in the wrong place.
//
// Our usage follows that exactly:
//   - cached event listings  -> derived from Postgres, rebuildable
//   - rate-limit counters    -> ephemeral by design (lesson 18)
//   - seat holds             -> deliberately expiring (lesson 18)
package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// New parses a redis:// URL and returns a connected client, or an error.
//
// Like the Postgres pool, we Ping at startup to FAIL FAST rather than
// discovering the problem on the first cache read.
func New(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url) // redis://host:port/db
	if err != nil {
		return nil, err
	}

	// These timeouts matter more for a cache than for a database.
	//
	// A cache exists to make things FASTER. If Redis is slow or unreachable, a
	// long timeout makes every request WAIT before falling back to Postgres —
	// so the cache has made the system slower than having no cache at all.
	// Short timeouts mean "give up quickly and go to the source".
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond

	rdb := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return rdb, nil
}
