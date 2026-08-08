// Package holds implements temporary seat reservations in Redis.
//
// ========================== THE PROBLEM IT SOLVES ==========================
//
// Right now booking is instant: you POST, and either you have a seat or you
// don't. Real ticketing doesn't work that way — you pick seats, you get a
// countdown ("you have 5:00 to complete payment"), and those seats are held
// for you while you type in your card details.
//
// That window needs a reservation that:
//   - blocks other people from taking those seats,
//   - is visible to EVERY API replica,
//   - and RELEASES ITSELF if the user abandons checkout.
//
// That last requirement is the interesting one. If a user closes their laptop
// mid-checkout, nobody is left to clean up. A lock that needs an explicit
// release will leak, and leaked seat locks mean an event that shows as sold
// out while being half empty.
//
// Redis TTL solves it: the reservation expires on its own. This is the single
// best reason to put holds in Redis rather than Postgres — the database has no
// native "this row deletes itself in 5 minutes", so you'd need a sweeper job,
// and now you own a cron that must never fail.
//
// ========================= WHY NOT A GO MUTEX (again) ======================
//
// Lesson 4's sync.Mutex coordinates goroutines in ONE process. Two API
// replicas behind Nginx have two separate mutexes that know nothing about each
// other, so both would happily hold the same seat. Redis is the shared point
// every replica talks to — which is what makes it a DISTRIBUTED lock.
//
// This is the missing half of the double-booking answer:
//
//	Postgres row lock (lesson 8)  -> the durable, final guarantee at commit
//	Redis hold (this)             -> the fast, temporary reservation before it
package holds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrInsufficientSeats means the request would exceed what's available once
// other people's active holds are accounted for.
var ErrInsufficientSeats = errors.New("not enough seats available to hold")

// Hold is a granted reservation.
type Hold struct {
	EventID   int       `json:"event_id"`
	UserID    int       `json:"user_id"`
	Seats     int       `json:"seats"`
	ExpiresAt time.Time `json:"expires_at"`
	// SecondsLeft is convenience for a countdown UI.
	SecondsLeft int `json:"seconds_left"`
}

type Manager struct {
	rdb *redis.Client
	ttl time.Duration
}

func New(rdb *redis.Client, ttl time.Duration) *Manager {
	return &Manager{rdb: rdb, ttl: ttl}
}

// ======================= DATA STRUCTURES (checklist item) ==================
//
// Two Redis structures per event, because one can't do both jobs:
//
//	holds:{eventID}      SORTED SET   member = userID, score = expiry (unix ms)
//	holdseats:{eventID}  HASH         field  = userID, value = seats held
//
// WHY A SORTED SET: it keeps members ordered by score, so "every hold that
// expired before now" is one range query — ZRANGEBYSCORE -inf now. With a
// plain SET you'd have to examine every member. This is the same structure
// that powers leaderboards (score = points instead of expiry).
//
// WHY A HASH ALONGSIDE: the ZSET's score is already used for expiry, so it
// can't also carry the seat count. A hash gives us userID -> seats.
//
// WHY NOT ONE KEY PER HOLD WITH A NATIVE TTL (hold:{event}:{user}, EXPIRE 5m)?
// That's simpler and self-cleaning — but then "how many seats are currently
// held for this event?" requires finding all matching keys, i.e. SCAN. On a
// single-threaded server, per request, that's the thing we're avoiding. Two
// container keys with lazy purging keeps every operation a bounded number of
// commands.
func zkey(eventID int) string { return fmt.Sprintf("holds:%d", eventID) }
func hkey(eventID int) string { return fmt.Sprintf("holdseats:%d", eventID) }

// ======================== WHY THIS MUST BE A LUA SCRIPT ====================
//
// The operation is READ-THEN-WRITE: sum the active holds, decide if there's
// room, and only then reserve. Done as separate commands from Go:
//
//	Replica A: HGETALL -> 8 held of 10 capacity, wants 2 -> OK
//	Replica B: HGETALL -> 8 held of 10 capacity, wants 2 -> OK
//	Replica A: HSET (now 10)
//	Replica B: HSET (now 12)          <-- OVERSOLD
//
// This is EXACTLY the lesson-4 counter race and the lesson-8 seat race, for a
// third time. Individual Redis commands are atomic; a SEQUENCE of them is not.
//
// SETNX is the two-command version of this fix — "set only if absent" collapses
// check-and-set into ONE atomic command, which is why it exists at all. Our
// check is richer than "does a key exist" (it's a capacity sum), so we need the
// general tool: a Lua script.
//
// Redis runs a script ATOMICALLY on its single thread — no other client can
// interleave between our commands. It is effectively a Redis transaction with
// logic in it, and it's why "just use Lua" is the standard answer to any
// check-then-act problem in Redis.
//
// KEYS vs ARGV is not stylistic: keys must be declared in KEYS so Redis Cluster
// can verify they hash to the same node. Passing a key through ARGV works on a
// single node and breaks in a cluster — a nasty thing to discover late.
var acquireScript = redis.NewScript(`
-- KEYS[1] = holds ZSET, KEYS[2] = holdseats HASH
-- ARGV[1] = now_ms, ARGV[2] = userID, ARGV[3] = seats wanted,
-- ARGV[4] = expires_at_ms, ARGV[5] = capacity, ARGV[6] = container TTL ms

-- 1. LAZY PURGE of expired holds.
--    Nothing is scheduled to clean these up, so every caller pays a tiny
--    cleanup cost. Lazy purging is a common Redis idiom: do the maintenance on
--    the read path instead of owning a background job that can silently die.
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if #expired > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
  redis.call('HDEL', KEYS[2], unpack(expired))
end

-- 2. Sum seats held by OTHER users. We skip this user's own existing hold so
--    that re-holding REPLACES it rather than stacking on top (idempotent-ish:
--    calling hold twice doesn't reserve double).
local total = 0
local flat = redis.call('HGETALL', KEYS[2])
for i = 1, #flat, 2 do
  if flat[i] ~= ARGV[2] then
    total = total + tonumber(flat[i+1])
  end
end

-- 3. Capacity check, now safe because nothing can interleave.
local want = tonumber(ARGV[3])
local capacity = tonumber(ARGV[5])
if total + want > capacity then
  return {0, capacity - total}          -- denied, and how many ARE available
end

-- 4. Reserve.
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[2])
redis.call('HSET', KEYS[2], ARGV[2], want)

-- 5. TTL on the CONTAINERS themselves, so an event nobody touches again
--    doesn't leave keys in Redis forever. Refreshed on every write.
redis.call('PEXPIRE', KEYS[1], ARGV[6])
redis.call('PEXPIRE', KEYS[2], ARGV[6])

return {1, capacity - total - want}
`)

// Acquire reserves seats for a user, if capacity allows.
//
// `capacity` is the event's CURRENT remaining seats, read from Postgres by the
// caller. Redis doesn't know about Postgres — the application is what joins
// the two, exactly as in cache-aside.
func (m *Manager) Acquire(ctx context.Context, eventID, userID, seats, capacity int) (Hold, error) {
	now := time.Now()
	expiresAt := now.Add(m.ttl)

	res, err := acquireScript.Run(ctx,
		m.rdb,
		[]string{zkey(eventID), hkey(eventID)},
		now.UnixMilli(),
		fmt.Sprintf("%d", userID),
		seats,
		expiresAt.UnixMilli(),
		capacity,
		(24 * time.Hour).Milliseconds(),
	).Slice()
	if err != nil {
		return Hold{}, err
	}

	ok, _ := res[0].(int64)
	if ok != 1 {
		available, _ := res[1].(int64)
		return Hold{}, fmt.Errorf("%w: %d available", ErrInsufficientSeats, available)
	}

	return Hold{
		EventID:     eventID,
		UserID:      userID,
		Seats:       seats,
		ExpiresAt:   expiresAt,
		SecondsLeft: int(m.ttl.Seconds()),
	}, nil
}

// Release drops a user's hold — called when they cancel checkout, and when a
// booking succeeds (the hold has served its purpose; the seats are now really
// gone from Postgres, so leaving the hold would double-count them).
func (m *Manager) Release(ctx context.Context, eventID, userID int) error {
	uid := fmt.Sprintf("%d", userID)
	pipe := m.rdb.Pipeline()
	pipe.ZRem(ctx, zkey(eventID), uid)
	pipe.HDel(ctx, hkey(eventID), uid)
	_, err := pipe.Exec(ctx)
	// redis.Nil just means there was nothing to remove — releasing a hold that
	// already expired is a normal, successful no-op, not an error.
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}

// heldScript purges expired holds and returns the current total, atomically.
var heldScript = redis.NewScript(`
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if #expired > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
  redis.call('HDEL', KEYS[2], unpack(expired))
end
local total = 0
local flat = redis.call('HGETALL', KEYS[2])
for i = 1, #flat, 2 do
  total = total + tonumber(flat[i+1])
end
return total
`)

// HeldSeats reports how many seats are currently held for an event.
func (m *Manager) HeldSeats(ctx context.Context, eventID int) (int, error) {
	n, err := heldScript.Run(ctx, m.rdb,
		[]string{zkey(eventID), hkey(eventID)},
		time.Now().UnixMilli(),
	).Int()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// TTL exposes the configured hold duration so handlers can report it.
func (m *Manager) TTL() time.Duration { return m.ttl }
