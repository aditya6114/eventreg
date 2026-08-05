package storage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eventreg/internal/models"
)

// UserStore is a SEPARATE interface from Store, not extra methods bolted onto
// it. That's INTERFACE SEGREGATION: a consumer should depend only on what it
// actually uses. The auth handler needs users and has no business seeing
// Book() or List(); the event handler needs events and has no business seeing
// password hashes. Two small interfaces express that; one big one wouldn't.
//
// It also keeps testing cheap — faking UserStore means writing two methods,
// not six.
type UserStore interface {
	CreateUser(email, passwordHash string) (models.User, error)
	GetUserByEmail(email string) (models.User, error)
	GetUserByID(id int) (models.User, error)
}

// ---------------------------------------------------------------- postgres --

var _ UserStore = (*postgresUserStore)(nil)

type postgresUserStore struct{ pool *pgxpool.Pool }

// NewPostgresUserStore shares the SAME pool as the event store (see NewPool).
func NewPostgresUserStore(pool *pgxpool.Pool) UserStore {
	return &postgresUserStore{pool: pool}
}

func (s *postgresUserStore) CreateUser(email, passwordHash string) (models.User, error) {
	ctx := context.Background()
	u := models.User{Email: normalizeEmail(email), PasswordHash: passwordHash}

	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, created_at`,
		u.Email, u.PasswordHash).Scan(&u.ID, &u.CreatedAt)

	if err != nil {
		// CATCHING A SPECIFIC CONSTRAINT VIOLATION.
		//
		// The users table has UNIQUE(email). Rather than doing "SELECT to check
		// if the email exists, then INSERT" — which is a read-then-write RACE,
		// exactly like the seat-counter bug — we just INSERT and let the
		// database be the referee. If two signups for the same email arrive
		// simultaneously, the DB guarantees exactly one wins, and the loser
		// lands here.
		//
		// pgx exposes the Postgres error code. 23505 = unique_violation.
		// Checking the CODE beats string-matching the message, which changes
		// between versions and locales.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return models.User{}, models.ErrEmailTaken
		}
		return models.User{}, err
	}
	return u, nil
}

func (s *postgresUserStore) GetUserByEmail(email string) (models.User, error) {
	ctx := context.Background()
	var u models.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = $1`,
		normalizeEmail(email)).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		// Return the GENERIC credentials error, not "no such user" — see the
		// comment on models.ErrInvalidCredentials about user enumeration.
		return models.User{}, models.ErrInvalidCredentials
	}
	if err != nil {
		return models.User{}, err
	}
	return u, nil
}

func (s *postgresUserStore) GetUserByID(id int) (models.User, error) {
	ctx := context.Background()
	var u models.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.User{}, &models.NotFoundError{EntityID: id}
	}
	if err != nil {
		return models.User{}, err
	}
	return u, nil
}

// ------------------------------------------------------------------ memory --

var _ UserStore = (*memoryUserStore)(nil)

type memoryUserStore struct {
	mu     sync.Mutex
	byID   map[int]models.User
	nextID int
}

// NewMemoryUserStore keeps the no-database fallback working, so you can still
// run the API (and exercise auth) without Postgres.
func NewMemoryUserStore() UserStore {
	return &memoryUserStore{byID: map[int]models.User{}}
}

func (s *memoryUserStore) CreateUser(email, passwordHash string) (models.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	email = normalizeEmail(email)
	// In memory we must check for duplicates ourselves — note how much more
	// fragile this is than the DB's UNIQUE constraint. It's only safe here
	// because the mutex serializes the whole check-then-insert.
	for _, u := range s.byID {
		if u.Email == email {
			return models.User{}, models.ErrEmailTaken
		}
	}
	s.nextID++
	u := models.User{
		ID:           s.nextID,
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
	}
	s.byID[u.ID] = u
	return u, nil
}

func (s *memoryUserStore) GetUserByEmail(email string) (models.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = normalizeEmail(email)
	for _, u := range s.byID {
		if u.Email == email {
			return u, nil
		}
	}
	return models.User{}, models.ErrInvalidCredentials
}

func (s *memoryUserStore) GetUserByID(id int) (models.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return models.User{}, &models.NotFoundError{EntityID: id}
	}
	return u, nil
}

// normalizeEmail lowercases and trims so "Bob@X.com " and "bob@x.com" are the
// same account. Done in ONE place so both implementations agree — if each store
// normalized differently you'd get subtle "account not found" bugs.
func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
