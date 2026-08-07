package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"eventreg/internal/models"
)

// ======================= THE DECORATOR PATTERN ==============================
//
// cachedStore IMPLEMENTS Store and WRAPS another Store. Every method either
// delegates straight through or adds caching around the delegation.
//
// This is the biggest payoff yet from the interface we defined in lesson 7:
//
//	main.go:  store = NewCachedStore(NewPostgresStore(pool), rdb, ttl, log)
//
// The handlers still see a `storage.Store` and cannot tell the difference.
// Caching was added to the entire application without touching a single
// handler, model, or SQL query. The alternative — sprinkling `rdb.Get(...)`
// calls through every handler — would smear cache logic across the codebase
// and make it impossible to turn off.
//
// It also composes: NewCachedStore(NewPostgresStore(...)) reads as "a Postgres
// store, with caching". You could add a metrics decorator or a tracing
// decorator the same way, each unaware of the others.
//
// ======================= CACHE-ASIDE (interview) ============================
//
// Also called "lazy loading". The APPLICATION owns the cache; Redis is a dumb
// box that knows nothing about Postgres.
//
//	READ:  1. look in cache
//	       2. HIT  -> return it
//	       3. MISS -> read the database, WRITE it to cache, return it
//
//	WRITE: 1. write to the database
//	       2. INVALIDATE the cached entry
//
// Why cache-aside rather than the alternatives:
//   - write-through (write to cache AND db on every write) keeps the cache
//     warmer but caches data nobody may ever read
//   - read-through (the cache itself fetches from the db) needs a cache that
//     understands your database
//
// Cache-aside is the default because it's simple, the app stays in control, and
// a cache outage degrades to "slower", not "broken".
//
// KEY DECISION: WE INVALIDATE, WE DON'T UPDATE. On a write we DELETE the cached
// value rather than recomputing it. Updating means writing the same data twice
// and racing yourself — two concurrent writers can each update the cache in the
// wrong order and leave it permanently disagreeing with the database. Deleting
// is idempotent and can only ever cause a miss, which is safe.

var _ Store = (*cachedStore)(nil)

type cachedStore struct {
	next Store // the real store (Postgres) — the source of truth
	rdb  *redis.Client
	ttl  time.Duration
	log  *slog.Logger
}

// NewCachedStore wraps a Store with Redis caching.
func NewCachedStore(next Store, rdb *redis.Client, ttl time.Duration, log *slog.Logger) Store {
	return &cachedStore{next: next, rdb: rdb, ttl: ttl, log: log}
}

// genKey holds a monotonically increasing "generation" number for event data.
//
// ============ WHY A GENERATION COUNTER INSTEAD OF DELETING KEYS =============
//
// Pagination means one logical dataset is spread over MANY cache keys:
// limit=20&offset=0, limit=20&offset=20, limit=50&offset=0... Creating one
// event invalidates all of them. How do you delete them?
//
//	KEYS events:list:*   -> NEVER. Redis is single-threaded; KEYS scans the
//	                        entire keyspace and BLOCKS every other client while
//	                        it runs. On a large instance this is an outage.
//	SCAN + DEL           -> safe but slow, many round trips, and racy: new keys
//	                        can appear while you're scanning.
//
// Instead the generation number is part of every cache key:
//
//	events:list:g7:l20:o0
//
// To invalidate EVERYTHING, INCR the generation. Every subsequent read looks
// for g8 keys and misses. The g7 keys are instantly unreachable and Redis
// reclaims them when their TTL expires.
//
// One O(1) command invalidates an unbounded number of keys, with no scanning.
// This is a genuinely common production pattern and a strong thing to be able
// to explain.
const genKey = "events:gen"

// listKey builds the cache key for one page of events.
func (s *cachedStore) listKey(gen string, limit, offset int) string {
	// Parameters that change the RESULT must be in the key. Miss one — say you
	// ignore offset — and page 2 serves page 1's cached data. A subtle, nasty
	// class of bug.
	return fmt.Sprintf("events:list:g%s:l%d:o%d", gen, limit, offset)
}

func (s *cachedStore) generation(ctx context.Context) string {
	gen, err := s.rdb.Get(ctx, genKey).Result()
	if err != nil {
		// redis.Nil means "key doesn't exist" — normal on first run, not an
		// error. Any other error means Redis is unhappy; either way "0" is a
		// safe answer, because a wrong generation only causes a cache miss.
		return "0"
	}
	return gen
}

// List is the cache-aside read path.
func (s *cachedStore) List(limit, offset int) (models.Page[models.Event], error) {
	ctx := context.Background()
	limit, offset = models.NormalizePagination(limit, offset)

	// NORMALIZE BEFORE BUILDING THE KEY. ?limit=0 and ?limit=20 produce the
	// same result, so they must produce the same key — otherwise the cache
	// fragments into duplicate entries for identical data.
	gen := s.generation(ctx)
	key := s.listKey(gen, limit, offset)

	// ---- 1. Look in the cache ----
	if raw, err := s.rdb.Get(ctx, key).Bytes(); err == nil {
		var page models.Page[models.Event]
		if jsonErr := json.Unmarshal(raw, &page); jsonErr == nil {
			s.log.Debug("cache hit", "key", key)
			return page, nil
		}
		// Corrupt entry (a schema change, a truncated write). Drop it and fall
		// through to the database rather than failing the request.
		s.log.Warn("cache entry unreadable; discarding", "key", key)
		s.rdb.Del(ctx, key)
	}

	// ---- 2. MISS: go to the source of truth ----
	//
	// GRACEFUL DEGRADATION: notice we did NOT return an error if Redis was
	// down. A cache failure must never become a request failure — the whole
	// point is that the database can still answer. If Redis is unreachable
	// every request simply becomes a normal Postgres query: slower, correct.
	s.log.Debug("cache miss", "key", key)
	page, err := s.next.List(limit, offset)
	if err != nil {
		return page, err // a REAL failure — the database itself
	}

	// ---- 3. Populate the cache for next time ----
	if buf, mErr := json.Marshal(page); mErr == nil {
		// ==================== WHY A TTL IS MANDATORY ====================
		//
		// Even with explicit invalidation on every write, the TTL is the
		// SAFETY NET. Invalidation can be missed: a bug in a new write path, a
		// direct UPDATE run by hand in psql, a failed INCR because Redis
		// blipped. Without a TTL, a stale entry lives FOREVER and you get the
		// classic "why is production showing old data" bug that survives
		// restarts and defeats everyone.
		//
		// With a TTL, the worst case is bounded: wrong for at most `ttl`.
		//
		// CHOOSING THE TTL is a tolerance-for-staleness question, not a
		// performance one:
		//   - too long  -> users see stale data after a missed invalidation
		//   - too short -> low hit rate; you paid the complexity for nothing
		// 30s suits an event list that changes on every booking. Reference data
		// that changes daily could sit at an hour.
		if sErr := s.rdb.Set(ctx, key, buf, s.ttl).Err(); sErr != nil {
			s.log.Debug("cache write failed", "key", key, "error", sErr)
		}
	}
	return page, nil
}

// invalidateEvents bumps the generation so every cached listing is orphaned.
func (s *cachedStore) invalidateEvents(ctx context.Context) {
	if err := s.rdb.Incr(ctx, genKey).Err(); err != nil {
		// Can't invalidate. Log it and move on — the write to Postgres already
		// succeeded and MUST NOT be failed over a cache problem. The TTL is
		// what limits the damage: stale for at most `ttl`, then self-corrects.
		// This is precisely why the TTL is non-negotiable.
		s.log.Warn("cache invalidation failed; entries will expire via TTL", "error", err)
	}
}

// ---- WRITES: delegate first, then invalidate ----
//
// ORDER MATTERS. Database first, cache second.
//
// Invalidating first would open a window where the cache is empty but the
// database still holds the OLD value — a concurrent reader repopulates the
// cache with stale data, and now it's stale until the TTL saves you.
//
// Writing first means the worst case is a brief window where the cache still
// serves the old value. Both orders have a race; this one's failure mode is
// smaller and self-healing.

func (s *cachedStore) Create(e models.Event) (models.Event, error) {
	created, err := s.next.Create(e)
	if err != nil {
		return created, err
	}
	s.invalidateEvents(context.Background())
	return created, nil
}

func (s *cachedStore) Book(eventID, userID, seats int) (models.BookingResult, error) {
	res, err := s.next.Book(eventID, userID, seats)
	if err != nil {
		return res, err
	}
	// A booking changes seat counts, which appear in the cached listing.
	// Anything that mutates data you cache must invalidate it — forgetting one
	// write path is the #1 source of stale-cache bugs.
	s.invalidateEvents(context.Background())
	return res, nil
}

func (s *cachedStore) CancelBooking(eventID, userID int) (models.CancelResult, error) {
	res, err := s.next.CancelBooking(eventID, userID)
	if err != nil {
		return res, err
	}
	s.invalidateEvents(context.Background())
	return res, nil
}

// ---- READS WE DELIBERATELY DON'T CACHE ----
//
// Caching is not free: every cached path is another thing that can go stale,
// and another invalidation you must remember. Cache where it PAYS.
//
//	Get(id)            single indexed primary-key lookup — Postgres already
//	                   answers in well under a millisecond. Caching it adds a
//	                   network round trip to save almost nothing.
//	ListUserBookings   per-user, so the hit rate would be near zero — each key
//	                   serves exactly one person.
//	ListUserWaitlist   same.
//	EventStats         a genuine candidate (aggregate over a JOIN, same for
//	                   everyone) — left uncached deliberately so there's an
//	                   obvious exercise if you want one.
//
// List() earns its cache: it runs COUNT(*) plus a paginated scan, the result is
// identical for every anonymous visitor, and browsing events is the single most
// common request during a launch rush.

func (s *cachedStore) Get(id int) (models.Event, error) { return s.next.Get(id) }

func (s *cachedStore) ListUserBookings(userID int) ([]models.BookingWithEvent, error) {
	return s.next.ListUserBookings(userID)
}

func (s *cachedStore) ListUserWaitlist(userID int) ([]models.WaitlistEntryWithEvent, error) {
	return s.next.ListUserWaitlist(userID)
}

func (s *cachedStore) EventStats() ([]models.EventStat, error) { return s.next.EventStats() }
