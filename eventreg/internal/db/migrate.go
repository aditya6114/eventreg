// Package db owns schema migrations.
//
// WHY MIGRATIONS EXIST (the checklist's "why schemas need versioning"):
// Lesson 8 created the events table with CREATE TABLE IF NOT EXISTS inside
// the store's init(). That works exactly once. It cannot answer any of the
// questions a real project asks:
//
//   - "What version is production's schema on?"          -> no record kept
//   - "How do I add a column to an existing table?"      -> IF NOT EXISTS won't
//   - "How do I undo a bad schema change?"               -> impossible
//   - "How does a teammate get the same schema?"         -> hope and copy-paste
//
// Migrations solve this by treating the schema as an ORDERED, VERSIONED list
// of change scripts. golang-migrate keeps a `schema_migrations` table in your
// database recording which version has been applied, so running migrations is
// idempotent: already-applied ones are skipped, new ones run in order.
//
// THE ONE RULE: never edit a migration that has already been applied anywhere.
// Its effect is already baked into that database, and editing it makes the
// file disagree with reality. To change something, add a NEW migration.
package db

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the "pgx5" driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// go:embed compiles the .sql files INTO the binary at build time. Without it
// the program would have to find migrations/ on disk at runtime — which breaks
// the moment you ship a single binary in a Docker image (Week 2) or a
// Kubernetes pod (Week 4). Embedding means the schema travels with the code.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every pending migration, in version order.
func Migrate(connString string) error {
	// iofs adapts our embedded filesystem into a migration source.
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	// golang-migrate selects its database driver from the URL SCHEME, and the
	// pgx/v5 driver registers itself as "pgx5". Our DATABASE_URL uses the
	// standard "postgres://" scheme (which pgxpool wants), so we swap just the
	// scheme here rather than forcing you to keep two different URLs around.
	migrateURL := connString
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(migrateURL, prefix) {
			migrateURL = "pgx5://" + strings.TrimPrefix(migrateURL, prefix)
			break
		}
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	// Close releases the migrator's own DB connection. It returns TWO errors
	// (source error, database error) — a slightly unusual signature worth
	// noticing. We ignore them here since we're shutting down anyway.
	defer func() { _, _ = m.Close() }()

	// Up applies everything pending. ErrNoChange means "already up to date",
	// which is a totally normal outcome on every restart — not a failure.
	// This is errors.Is from lesson 3 doing real work again.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Version reports the current schema version and whether the database is
// "dirty".
//
// DIRTY is worth understanding: if a migration fails halfway, golang-migrate
// marks the DB dirty and REFUSES to run anything else, because it no longer
// knows what state the schema is in. Silently continuing could corrupt data.
// You fix it by inspecting the schema manually, then using `force <version>`
// to tell the tool what's actually true. Seeing "Dirty database version N"
// is not a bug in the tool — it's the tool protecting you.
func Version(connString string) (version uint, dirty bool, err error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return 0, false, err
	}
	migrateURL := connString
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(migrateURL, prefix) {
			migrateURL = "pgx5://" + strings.TrimPrefix(migrateURL, prefix)
			break
		}
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return 0, false, err
	}
	defer func() { _, _ = m.Close() }()
	return m.Version()
}
