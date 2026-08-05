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

// NewPostgresStore connects, verifies the connection, ensures the schema
// exists, and returns a ready Store. It returns an error (unlike the memory
// constructor) because connecting can genuinely fail.
//
// NOTE on context: the Store interface methods don't take a context.Context,
// so internally we use context.Background(). Threading a real per-request
// context through (for timeouts/cancellation) is a later checklist item;
// we keep the interface unchanged for now so the swap stays one line.
func NewPostgresStore(connString string) (Store, error) {
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

	s := &postgresStore{pool: pool}
	if err := s.init(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// init creates the table and seeds it if empty. This is a TEMPORARY stand-in
// for real migrations — the golang-migrate lesson replaces it. Exec runs a
// statement that returns no rows.
func (s *postgresStore) init(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS events (
			id    SERIAL PRIMARY KEY,   -- auto-incrementing id
			name  TEXT   NOT NULL,
			seats INT    NOT NULL CHECK (seats >= 0)  -- DB-level guard: never negative
		)`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	// Seed only if empty, so restarts don't duplicate rows.
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		return fmt.Errorf("count events: %w", err)
	}
	if count == 0 {
		_, err = s.pool.Exec(ctx,
			`INSERT INTO events (name, seats) VALUES ($1,$2), ($3,$4)`,
			"Coldplay", 50000, "Local Gig", 200)
		if err != nil {
			return fmt.Errorf("seed events: %w", err)
		}
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
func (s *postgresStore) Book(id, seats int) (models.Event, error) {
	ctx := context.Background()

	tx, err := s.pool.Begin(ctx) // start the transaction
	if err != nil {
		return models.Event{}, err
	}
	// Defer Rollback: if we return early (error) it undoes everything. After a
	// successful Commit, Rollback is a harmless no-op. This defer-rollback
	// pattern guarantees we never leak an open transaction.
	defer tx.Rollback(ctx)

	var e models.Event
	err = tx.QueryRow(ctx,
		`SELECT id, name, seats FROM events WHERE id = $1 FOR UPDATE`, id).
		Scan(&e.ID, &e.Name, &e.Seats)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Event{}, &models.NotFoundError{EntityID: id}
	}
	if err != nil {
		return models.Event{}, err
	}

	if seats > e.Seats {
		return models.Event{}, models.ErrSoldOut // rollback fires via defer
	}

	if _, err := tx.Exec(ctx,
		`UPDATE events SET seats = seats - $1 WHERE id = $2`, seats, id); err != nil {
		return models.Event{}, err
	}

	if err := tx.Commit(ctx); err != nil { // make it permanent + release lock
		return models.Event{}, err
	}
	e.Seats -= seats
	return e, nil
}
