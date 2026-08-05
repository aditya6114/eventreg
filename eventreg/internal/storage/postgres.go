// This file adds a SECOND implementation of the Store interface — backed by
// real Postgres via the pgx driver. Nothing in handlers or models changes:
// postgresStore has the same four methods, so it IS a Store (lesson 3's
// implicit satisfaction). main.go picks which one to build.
//
// pgx is the de-facto Postgres driver/toolkit for Go. We use its connection
// POOL (pgxpool): a set of reusable open connections. Opening a TCP+auth
// connection per request is slow; a pool hands requests a ready connection
// and returns it after — this is the "connection pooling" interview topic
// (why: bounded connections, lower latency, backpressure under load).
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eventreg/internal/models"
)

// Compile-time proof that *postgresStore satisfies Store (same trick as the
// memory store). If a SQL method's signature drifts, the error lands here.
var _ Store = (*postgresStore)(nil)

type postgresStore struct {
	pool *pgxpool.Pool // the connection pool, shared by all requests
}

// NewPool creates and verifies the shared connection pool.
//
// WHY THIS IS SEPARATE (lesson 10 refactor): the app now has TWO stores — one
// for events, one for users — and they must share ONE pool. A pool per store
// would double the connections to Postgres for no benefit, and Postgres has a
// hard `max_connections` limit (default 100) that is easy to exhaust. One pool
// per PROCESS is the rule; hand it to whatever needs it.
//
// This is also better dependency injection: main.go now owns the pool's
// lifetime and can Close() it on shutdown (graceful shutdown, Week 2).
func NewPool(connString string) (*pgxpool.Pool, error) {
	ctx := context.Background()

	// pgxpool.New parses the URL and creates the pool (lazily connecting).
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err) // %w wraps (lesson 3)
	}

	// Ping forces one real connection so we fail fast at startup if the DB
	// is unreachable, instead of on the first request.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

// NewPostgresStore builds the events store on an existing pool.
//
// NOTE on context: the Store interface methods don't take a context.Context,
// so internally we use context.Background(). Threading a real per-request
// context through (for timeouts/cancellation) is a later checklist item.
//
// NOTE: the schema is NO LONGER created here. Lesson 9 moved it to versioned
// migrations (internal/db), which main.go runs before building the store.
// A store's job is to read and write data, not to define the schema.
func NewPostgresStore(pool *pgxpool.Pool) (Store, error) {
	s := &postgresStore{pool: pool}
	if err := s.Seed(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// Seed inserts a couple of demo events if the table is empty, so a fresh
// database isn't blank when you curl /events. This is SEED DATA (convenience),
// deliberately kept separate from MIGRATIONS (schema structure) — a distinction
// worth keeping: schema changes are mandatory and versioned; seed data is
// optional and environment-specific (you would not seed production).
func (s *postgresStore) Seed(ctx context.Context) error {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		return fmt.Errorf("count events: %w", err)
	}
	if count > 0 {
		return nil // already has data; don't duplicate on every restart
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO events (name, seats) VALUES ($1,$2), ($3,$4)`,
		"Coldplay", 50000, "Local Gig", 200)
	if err != nil {
		return fmt.Errorf("seed events: %w", err)
	}
	return nil
}

func (s *postgresStore) List() ([]models.Event, error) {
	ctx := context.Background()
	// Query returns multiple rows. $-placeholders aren't needed here (no
	// params), but ALWAYS use them for user input — string-concatenating SQL
	// is how you get SQL injection. pgx sends params separately from the SQL.
	rows, err := s.pool.Query(ctx, `SELECT id, name, seats FROM events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // lesson 4: defer the cleanup right after acquiring

	out := make([]models.Event, 0)
	for rows.Next() { // advance one row at a time
		var e models.Event
		// Scan copies the row's columns INTO your struct fields, by position.
		// Column order here must match the &pointers order.
		if err := rows.Scan(&e.ID, &e.Name, &e.Seats); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err() // check for an error that ended the iteration
}

func (s *postgresStore) Get(id int) (models.Event, error) {
	ctx := context.Background()
	var e models.Event
	// QueryRow expects exactly one row; $1 is the first parameter (id).
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, seats FROM events WHERE id = $1`, id).
		Scan(&e.ID, &e.Name, &e.Seats)
	if errors.Is(err, pgx.ErrNoRows) {
		// Translate the driver's "no rows" into OUR domain error, so the
		// handler's central mapper turns it into a 404 (lessons 3 & 7).
		return models.Event{}, &models.NotFoundError{EntityID: id}
	}
	if err != nil {
		return models.Event{}, err
	}
	return e, nil
}

func (s *postgresStore) Create(e models.Event) (models.Event, error) {
	ctx := context.Background()
	// RETURNING id gives us the SERIAL the DB generated, in one round-trip.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO events (name, seats) VALUES ($1, $2) RETURNING id`,
		e.Name, e.Seats).Scan(&e.ID)
	if err != nil {
		return models.Event{}, err
	}
	return e, nil
}

// Book is the important one — the database version of preventing double
// booking. It runs inside a TRANSACTION and locks the row with FOR UPDATE:
//
//   BEGIN
//   SELECT ... FOR UPDATE   <- locks this event's row; concurrent bookers
//                              for the SAME event WAIT here, serializing them
//   (check seats, decide)
//   UPDATE ...              <- only one goroutine/connection at a time
//   COMMIT                  <- releases the lock
//
// This is the Postgres analogue of lesson 4's mutex, but it works ACROSS
// processes (all API replicas hit the same DB), which a mutex can't. This is
// the core of your #1 interview answer.
// LESSON 11 VERSION: now records WHO booked, and is IDEMPOTENT.
//
// The transaction now does three things atomically:
//
//	BEGIN
//	SELECT ... FOR UPDATE          <- lock the event row (lesson 8)
//	INSERT INTO bookings ... ON CONFLICT DO NOTHING
//	                               <- if this user already booked, insert no-ops
//	UPDATE events SET seats = ...  <- only if the insert actually happened
//	COMMIT
//
// ATOMICITY IS THE POINT: the booking row and the seat decrement either BOTH
// happen or NEITHER does. Without a transaction you could decrement seats and
// then fail to insert the booking — seats gone, no record of who has them.
// LESSON 12: a full event is no longer a dead end. If seats aren't available
// the user is auto-enqueued on the WAITLIST, so the outcome is one of four
// cases (booked / waitlisted / already_booked / already_waitlisted) — all of
// them decided inside the SAME locked transaction, so the "is there room?"
// answer can't change between checking and acting.
func (s *postgresStore) Book(eventID, userID, seats int) (models.BookingResult, error) {
	ctx := context.Background()

	tx, err := s.pool.Begin(ctx) // start the transaction
	if err != nil {
		return models.BookingResult{}, err
	}
	// Defer Rollback: if we return early (error) it undoes everything. After a
	// successful Commit, Rollback is a harmless no-op. This defer-rollback
	// pattern guarantees we never leak an open transaction.
	defer tx.Rollback(ctx)

	var e models.Event
	err = tx.QueryRow(ctx,
		`SELECT id, name, seats FROM events WHERE id = $1 FOR UPDATE`, eventID).
		Scan(&e.ID, &e.Name, &e.Seats)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.BookingResult{}, &models.NotFoundError{EntityID: eventID}
	}
	if err != nil {
		return models.BookingResult{}, err
	}

	// IDEMPOTENCY CHECK 1: already booked?
	//
	// We now check BEFORE attempting the insert, because the decision tree has
	// grown: an existing booking, an existing waitlist entry, room to book, or
	// no room. Reading first inside the lock is safe — nobody else can change
	// this event's rows while we hold it.
	if existing, gErr := s.getBookingTx(ctx, tx, eventID, userID); gErr == nil {
		// DUPLICATE REQUEST. Return the EXISTING booking and DON'T touch the
		// seat count — the retry must not consume more seats. That's what
		// idempotent means: doing it twice equals doing it once.
		//
		// We return the original booking rather than an error, because from
		// the client's point of view their request succeeded: they have a
		// booking. A retry after a dropped response should look identical to
		// the first call.
		existing.Idempotent = true
		if err := tx.Commit(ctx); err != nil {
			return models.BookingResult{}, err
		}
		return models.BookingResult{Outcome: models.OutcomeAlreadyBooked, Booking: &existing}, nil
	} else if !errors.Is(gErr, pgx.ErrNoRows) {
		return models.BookingResult{}, gErr
	}

	// IDEMPOTENCY CHECK 2: already on the waitlist?
	if w, gErr := s.getWaitlistTx(ctx, tx, eventID, userID); gErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return models.BookingResult{}, err
		}
		return models.BookingResult{Outcome: models.OutcomeAlreadyWaitlisted, Waitlist: &w}, nil
	} else if !errors.Is(gErr, pgx.ErrNoRows) {
		return models.BookingResult{}, gErr
	}

	// NOT ENOUGH ROOM -> join the waitlist instead of failing.
	//
	// This is the product change lesson 12 is about: "sold out" stops being a
	// dead end. The user keeps their place in line, and a later cancellation
	// can promote them automatically.
	//
	// ON CONFLICT DO NOTHING still guards the concurrent-duplicate case, since
	// the check above and this insert are only atomic because of the row lock.
	if seats > e.Seats {
		var w models.WaitlistEntry
		err = tx.QueryRow(ctx,
			`INSERT INTO waitlist (event_id, user_id, seats)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (event_id, user_id) DO NOTHING
			 RETURNING id, event_id, user_id, seats, created_at`,
			eventID, userID, seats).
			Scan(&w.ID, &w.EventID, &w.UserID, &w.Seats, &w.CreatedAt)
		if err != nil {
			return models.BookingResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return models.BookingResult{}, err
		}
		return models.BookingResult{Outcome: models.OutcomeWaitlisted, Waitlist: &w}, nil
	}

	// THERE IS ROOM: create the booking and decrement seats, atomically.
	var b models.Booking
	err = tx.QueryRow(ctx,
		`INSERT INTO bookings (event_id, user_id, seats)
		 VALUES ($1, $2, $3)
		 RETURNING id, event_id, user_id, seats, created_at`,
		eventID, userID, seats).
		Scan(&b.ID, &b.EventID, &b.UserID, &b.Seats, &b.CreatedAt)
	if err != nil {
		return models.BookingResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE events SET seats = seats - $1 WHERE id = $2`, seats, eventID); err != nil {
		return models.BookingResult{}, err
	}

	if err := tx.Commit(ctx); err != nil { // make it permanent + release lock
		return models.BookingResult{}, err
	}
	return models.BookingResult{Outcome: models.OutcomeBooked, Booking: &b}, nil
}

// CancelBooking frees a booking's seats and immediately promotes whoever has
// been waiting longest. Cancellation is what makes a waitlist worth having.
//
// All of it is ONE transaction under the event's row lock, so the freed seats
// cannot be grabbed by a concurrent booker before the queue gets its turn —
// the people who already waited have priority over someone arriving now.
func (s *postgresStore) CancelBooking(eventID, userID int) (models.CancelResult, error) {
	ctx := context.Background()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return models.CancelResult{}, err
	}
	defer tx.Rollback(ctx)

	// Lock the event first — same lock every booking takes, so cancels and
	// books are serialized against each other.
	var e models.Event
	err = tx.QueryRow(ctx,
		`SELECT id, name, seats FROM events WHERE id = $1 FOR UPDATE`, eventID).
		Scan(&e.ID, &e.Name, &e.Seats)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.CancelResult{}, &models.NotFoundError{EntityID: eventID}
	}
	if err != nil {
		return models.CancelResult{}, err
	}

	// DELETE ... RETURNING tells us how many seats came back, in one statement.
	// If no row is deleted, this user had no booking -> 404.
	var freed int
	err = tx.QueryRow(ctx,
		`DELETE FROM bookings WHERE event_id = $1 AND user_id = $2 RETURNING seats`,
		eventID, userID).Scan(&freed)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.CancelResult{}, &models.NotFoundError{EntityID: eventID}
	}
	if err != nil {
		return models.CancelResult{}, err
	}

	available := e.Seats + freed

	promoted, remaining, err := s.promoteTx(ctx, tx, eventID, available)
	if err != nil {
		return models.CancelResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE events SET seats = $1 WHERE id = $2`, remaining, eventID); err != nil {
		return models.CancelResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return models.CancelResult{}, err
	}
	return models.CancelResult{
		EventID:    eventID,
		SeatsFreed: freed,
		SeatsLeft:  remaining,
		Promoted:   promoted,
	}, nil
}

// promoteTx converts waitlist entries into bookings, oldest first, while seats
// remain. Runs inside the caller's transaction and lock.
//
// STRICT FIFO — a fairness decision worth defending. We take the EARLIEST entry
// and stop if it doesn't fit, rather than skipping ahead to a smaller request
// that would. Skipping would let someone who asked for 1 seat repeatedly jump
// ahead of someone who asked for 4, so the larger request could starve forever.
// "First come, first served" is easier to explain to users and impossible to
// game. The cost is leaving seats unfilled sometimes; that's the trade-off.
//
// The composite index idx_waitlist_event_created (migration 2) exists exactly
// for this query — filter by event, order by arrival, take one.
func (s *postgresStore) promoteTx(ctx context.Context, tx pgx.Tx, eventID, available int) ([]models.Booking, int, error) {
	promoted := make([]models.Booking, 0)

	for available > 0 {
		var w models.WaitlistEntry
		err := tx.QueryRow(ctx,
			`SELECT id, event_id, user_id, seats, created_at
			 FROM waitlist
			 WHERE event_id = $1
			 ORDER BY created_at
			 LIMIT 1`, eventID).
			Scan(&w.ID, &w.EventID, &w.UserID, &w.Seats, &w.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			break // queue empty
		}
		if err != nil {
			return nil, 0, err
		}
		if w.Seats > available {
			break // strict FIFO: don't skip past them
		}

		// Promote: booking in, waitlist row out. Both inside the transaction,
		// so a user can never exist in both tables (or neither) if this fails.
		var b models.Booking
		err = tx.QueryRow(ctx,
			`INSERT INTO bookings (event_id, user_id, seats)
			 VALUES ($1, $2, $3)
			 RETURNING id, event_id, user_id, seats, created_at`,
			w.EventID, w.UserID, w.Seats).
			Scan(&b.ID, &b.EventID, &b.UserID, &b.Seats, &b.CreatedAt)
		if err != nil {
			return nil, 0, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM waitlist WHERE id = $1`, w.ID); err != nil {
			return nil, 0, err
		}

		available -= w.Seats
		promoted = append(promoted, b)
	}
	return promoted, available, nil
}

func (s *postgresStore) getWaitlistTx(ctx context.Context, tx pgx.Tx, eventID, userID int) (models.WaitlistEntry, error) {
	var w models.WaitlistEntry
	err := tx.QueryRow(ctx,
		`SELECT id, event_id, user_id, seats, created_at
		 FROM waitlist WHERE event_id = $1 AND user_id = $2`,
		eventID, userID).
		Scan(&w.ID, &w.EventID, &w.UserID, &w.Seats, &w.CreatedAt)
	return w, err
}

// ListUserWaitlist returns the user's queue entries WITH their position.
//
// Position is computed with a WINDOW FUNCTION rather than stored:
//
//	ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY created_at)
//
// A window function computes a value across a set of rows RELATED to the
// current row, without collapsing them the way GROUP BY does. Here: "number the
// rows within each event, ordered by arrival". PARTITION BY restarts the
// numbering per event, so each event has its own 1, 2, 3...
//
// We number the whole waitlist in a subquery, THEN filter to this user —
// filtering first would number only their rows and everyone would be #1.
func (s *postgresStore) ListUserWaitlist(userID int) ([]models.WaitlistEntryWithEvent, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.event_id, w.user_id, w.seats, w.created_at, w.position, e.name
		 FROM (
		     SELECT id, event_id, user_id, seats, created_at,
		            ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY created_at) AS position
		     FROM waitlist
		 ) w
		 JOIN events e ON e.id = w.event_id
		 WHERE w.user_id = $1
		 ORDER BY w.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.WaitlistEntryWithEvent, 0)
	for rows.Next() {
		var r models.WaitlistEntryWithEvent
		if err := rows.Scan(&r.ID, &r.EventID, &r.UserID, &r.Seats, &r.CreatedAt,
			&r.Position, &r.EventName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// getBookingTx fetches an existing booking INSIDE the caller's transaction, so
// it sees the same consistent snapshot (and holds the same lock).
func (s *postgresStore) getBookingTx(ctx context.Context, tx pgx.Tx, eventID, userID int) (models.Booking, error) {
	var b models.Booking
	err := tx.QueryRow(ctx,
		`SELECT id, event_id, user_id, seats, created_at
		 FROM bookings WHERE event_id = $1 AND user_id = $2`,
		eventID, userID).
		Scan(&b.ID, &b.EventID, &b.UserID, &b.Seats, &b.CreatedAt)
	return b, err
}

// ListUserBookings — a JOIN.
//
// WHY JOIN INSTEAD OF TWO QUERIES: without it you'd fetch the user's bookings,
// then loop and fetch each event by id — the classic N+1 QUERY PROBLEM (1 query
// + N more). At 50 bookings that's 51 round-trips. A JOIN asks the database to
// stitch the rows together once and returns everything in ONE trip.
//
// INNER JOIN (the default) returns only rows that match on BOTH sides. That's
// correct here: a booking always has an event (the foreign key guarantees it),
// and a booking with no event shouldn't exist.
//
// The index from migration 2 (idx_bookings_user_id) is what makes the WHERE
// fast — without it Postgres scans every booking in the table.
func (s *postgresStore) ListUserBookings(userID int) ([]models.BookingWithEvent, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT b.id, b.event_id, b.user_id, b.seats, b.created_at,
		        e.name, e.seats
		 FROM bookings b
		 JOIN events e ON e.id = b.event_id
		 WHERE b.user_id = $1
		 ORDER BY b.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.BookingWithEvent, 0)
	for rows.Next() {
		var r models.BookingWithEvent
		if err := rows.Scan(&r.ID, &r.EventID, &r.UserID, &r.Seats, &r.CreatedAt,
			&r.EventName, &r.SeatsLeft); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventStats — GROUP BY with aggregates.
//
// GROUP BY collapses many rows into one row per group, and AGGREGATE functions
// (COUNT, SUM, AVG, MIN, MAX) summarize each group.
//
// LEFT JOIN, NOT INNER JOIN — the key detail. An INNER JOIN would silently DROP
// events that have zero bookings, so your "how is everything selling?" report
// would hide exactly the events you most need to see. LEFT JOIN keeps every row
// from the left table (events) and fills NULLs where there's no match.
//
// COALESCE(SUM(b.seats), 0) turns that NULL into 0 — SUM over zero rows is NULL,
// not 0, which is a classic SQL gotcha.
//
// THE RULE: every non-aggregated column in the SELECT must appear in GROUP BY.
// Postgres enforces it — otherwise it wouldn't know which value to show for a
// group containing several different ones.
func (s *postgresStore) EventStats() ([]models.EventStat, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT e.id, e.name, e.seats,
		        COUNT(b.id)                  AS booking_count,
		        COALESCE(SUM(b.seats), 0)    AS seats_booked
		 FROM events e
		 LEFT JOIN bookings b ON b.event_id = e.id
		 GROUP BY e.id, e.name, e.seats
		 ORDER BY seats_booked DESC, e.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.EventStat, 0)
	for rows.Next() {
		var st models.EventStat
		if err := rows.Scan(&st.EventID, &st.Name, &st.SeatsLeft,
			&st.BookingCount, &st.SeatsBooked); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
