// Package ratelimit implements request throttling backed by Redis.
//
// ==================== WHY RATE LIMITING (and why in Redis) =================
//
// Two concrete threats to THIS project:
//
//  1. LOGIN BRUTE FORCE. /auth/login is a password oracle. Without a limit an
//     attacker can try thousands of passwords per second. bcrypt makes each
//     guess expensive for THEM, but it also makes each guess expensive for US
//     — 100ms of CPU per attempt is itself a denial-of-service vector.
//  2. THE LAUNCH STAMPEDE. One impatient student holding down F5, or a script,
//     can consume capacity that thousands of legitimate users need.
//
// WHY REDIS AND NOT AN IN-MEMORY COUNTER (the interview point):
// A Go map + mutex would work for ONE process. With two API replicas behind
// Nginx, each keeps its own counters, so a limit of 10 becomes an effective 20
// — and it drifts with every replica you add. The counter must live somewhere
// BOTH replicas can see. This is the same "a mutex can't span processes"
// lesson as double-booking, in a different costume.
package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// Limiter enforces N requests per window, per key, using a FIXED WINDOW.
//
// ================= FIXED WINDOW vs SLIDING WINDOW vs TOKEN BUCKET ==========
//
// FIXED WINDOW (what we implement)
//
//	Divide time into fixed buckets ("12:00:00-12:00:59") and count per bucket.
//	  + Two commands (INCR, EXPIRE). Trivially cheap, trivially correct.
//	  - THE BOUNDARY BURST: a client can send N requests at 12:00:59 and N more
//	    at 12:01:00 — 2N in one second, while never breaking "N per minute".
//	    Fine for abuse prevention; not fine if the limit protects a hard
//	    downstream quota.
//
// SLIDING WINDOW LOG
//
//	Store a timestamp per request in a sorted set; count entries in the last
//	60s and drop older ones (ZREMRANGEBYSCORE + ZCARD + ZADD).
//	  + Exact. No boundary burst.
//	  - Memory grows with request count — you store every request.
//
// SLIDING WINDOW COUNTER
//
//	Keep the current and previous fixed windows and interpolate between them.
//	  + Nearly as accurate as the log, at fixed-window memory cost.
//	  - Fiddlier to implement and reason about. The usual production choice.
//
// TOKEN BUCKET
//
//	A bucket holds up to B tokens and refills at R tokens/second; each request
//	takes one. Empty bucket = rejected.
//	  + Allows BURSTS (up to B) while bounding the sustained rate — often what
//	    you actually want, since real users are bursty.
//	  - Needs two values (tokens, last-refill time) updated atomically -> Lua.
//
// WE CHOSE FIXED WINDOW deliberately: it's the clearest demonstration of
// INCR+EXPIRE (the checklist item), and its weakness is acceptable for abuse
// prevention. Knowing WHY you'd upgrade — and to which — is the interview
// answer.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
	prefix string // namespaces this limiter's keys from others
}


func New(rdb *redis.Client, limit int, window time.Duration, prefix string) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window, prefix: prefix}
}

// Allow reports whether this key may proceed, plus how many requests remain
// and when the window resets (for the response headers).
func (l *Limiter) Allow(ctx context.Context, key string) (allowed bool, remaining int, resetIn time.Duration) {
	// The key embeds the WINDOW NUMBER, so a new bucket is a new key and the
	// old one simply expires. No cleanup job, no scanning — the same "encode
	// state in the key name" trick as the cache generation counter.
	windowNum := time.Now().Unix() / int64(l.window.Seconds())
	k := fmt.Sprintf("rl:%s:%s:%d", l.prefix, key, windowNum)

	// ==================== WHY A PIPELINE, AND WHY IT'S SAFE ================
	//
	// A pipeline sends both commands in ONE network round-trip instead of two.
	// It is NOT a transaction — but it doesn't need to be here:
	//
	//   - INCR is atomic on its own (Redis is single-threaded), so two
	//     concurrent requests can never both read "5" and both write "6". This
	//     is exactly the lesson-4 lost-update problem, and Redis is immune to
	//     it by construction.
	//   - EXPIRE is idempotent. Setting the same TTL repeatedly is harmless.
	//
	// So the only interleaving risk is a crash between INCR and EXPIRE leaving
	// a key with no TTL — a counter stuck forever, locking that client out.
	// We guard against that below by re-checking the TTL.
	pipe := l.rdb.Pipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, l.window)
	ttlCmd := pipe.TTL(ctx, k)

	if _, err := pipe.Exec(ctx); err != nil {
		// FAIL OPEN, NOT CLOSED. Redis is down -> ALLOW the request.
		//
		// This is a real judgement call. Failing CLOSED (deny everything) turns
		// a cache outage into a total outage — the rate limiter becomes the
		// most critical component in the system, which is backwards for
		// something whose job is protecting against abuse. Failing open means
		// that during a Redis outage you're unprotected but still SERVING.
		//
		// The opposite choice is right when the limiter enforces something with
		// real consequences (billing quotas, a paid third-party API). Know
		// which situation you're in — and say so in an interview.
		return true, l.limit, l.window
	}

	count := int(incr.Val())

	// Repair a missing TTL (the crash-between-commands case above).
	if ttl := ttlCmd.Val(); ttl < 0 {
		l.rdb.Expire(ctx, k, l.window)
		ttl = l.window
	}
	resetIn = ttlCmd.Val()
	if resetIn < 0 {
		resetIn = l.window
	}

	remaining = l.limit - count
	if remaining < 0 {
		remaining = 0
	}
	return count <= l.limit, remaining, resetIn
}

// Middleware enforces the limit, keyed by client IP.
//
// KEYING BY IP IS A COMPROMISE worth understanding:
//   - Users behind one NAT (a campus network — very relevant here!) share an
//     IP and therefore share a budget. A whole university could throttle
//     itself.
//   - IPv6 clients can rotate addresses cheaply, weakening the limit.
//
// Better keys when you have them: the authenticated user id for logged-in
// routes, or an API key. IP is the only option for anonymous endpoints like
// /auth/login — which is exactly where brute-force protection matters most.
// A production system layers both: per-IP AND per-account.
func (l *Limiter) Middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// c.RealIP() honours X-Forwarded-For / X-Real-IP, which is essential
		// once Nginx sits in front (Week 3) — otherwise EVERY request appears
		// to come from the proxy and shares one bucket. It also means those
		// headers must be trusted only from your own proxy, or a client can
		// forge them and bypass the limit entirely.
		key := c.RealIP()

		allowed, remaining, resetIn := l.Allow(c.Request().Context(), key)

		// Standard-ish informational headers so a well-behaved client can
		// self-throttle instead of hammering until it's blocked.
		h := c.Response().Header()
		h.Set("X-RateLimit-Limit", strconv.Itoa(l.limit))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		h.Set("X-RateLimit-Reset", strconv.Itoa(int(resetIn.Seconds())))

		if !allowed {
			// Retry-After tells the client exactly how long to wait. Omitting
			// it invites the retry storm you were trying to prevent.
			h.Set("Retry-After", strconv.Itoa(int(resetIn.Seconds())+1))
			// 429 Too Many Requests — the correct code. Not 403 (that's
			// "authenticated but forbidden") and not 503.
			return echo.NewHTTPError(http.StatusTooManyRequests,
				"rate limit exceeded, please slow down")
		}
		return next(c)
	}
}
