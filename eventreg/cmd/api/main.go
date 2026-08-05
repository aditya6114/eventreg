// Command api is the entry point for the Event Ticketing service.
//
// This is the ONLY package main in the real project. Convention: runnable
// programs live under cmd/<name>/, so a repo can ship several binaries
// (cmd/api, later maybe cmd/migrate, cmd/notifier) that all share the
// internal/ libraries.
//
// main() is a "composition root": it CONSTRUCTS the concrete pieces and
// WIRES them together, then starts the server. It contains almost no logic
// of its own — logic lives in the internal packages. Read this file to
// understand how the app is assembled; read internal/* to understand what
// it does.
//
// Run it from the eventreg folder:
//   go run ./cmd/api
//
// Then — GETs are simple everywhere:
//   curl localhost:8082/health
//   curl localhost:8082/events
//   curl localhost:8082/events/1
//
// JSON POSTs need the Content-Type header (Echo's c.Bind dispatches on it —
// lesson 6 gotcha). The body syntax differs by shell:
//
//   bash / cmd.exe:
//     curl -H "Content-Type: application/json" -X POST localhost:8082/events/1/book -d "{\"seats\":5}"
//
//   PowerShell 5.1 — the above FAILS. PowerShell strips embedded quotes when
//   passing args to a native .exe, so curl receives broken JSON and the API
//   correctly answers 400. Use either:
//     Invoke-RestMethod -Uri http://localhost:8082/events/1/book -Method Post `
//       -ContentType "application/json" -Body '{"seats":5}'
//   ...or curl with the --% stop-parsing token (needed to see 4xx bodies,
//   since Invoke-RestMethod throws on 4xx/5xx instead of printing them):
//     curl.exe --% -H "Content-Type: application/json" -X POST localhost:8082/events/1/book -d "{\"seats\":5}"
package main

import (
	"log"
	"os"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"eventreg/internal/db"
	"eventreg/internal/handlers"
	"eventreg/internal/storage"
)

func main() {
	// 1. Build the storage layer. THIS is the payoff of the interface: the
	//    choice of backend lives here and NOWHERE else. If DATABASE_URL is
	//    set we use Postgres; otherwise we fall back to the in-memory store.
	//    Both return a storage.Store, so `h` below can't tell them apart.
	//    Set it (PowerShell) with:
	//      $env:DATABASE_URL="postgres://eventgo:secret@localhost:5432/eventgo"
	var store storage.Store
	var users storage.UserStore
	if url := os.Getenv("DATABASE_URL"); url != "" {
		// Run migrations BEFORE building the stores. The app should never talk
		// to a database whose schema it hasn't verified — starting up against
		// an out-of-date schema produces confusing runtime errors instead of
		// one clear failure here. Migrations are idempotent, so this is safe
		// on every restart: already-applied versions are skipped.
		if err := db.Migrate(url); err != nil {
			log.Fatal("migrate: ", err)
		}
		if v, dirty, err := db.Version(url); err == nil {
			log.Printf("schema: version %d (dirty=%t)", v, dirty)
		}

		// ONE pool, shared by both stores. See storage.NewPool for why.
		pool, err := storage.NewPool(url)
		if err != nil {
			log.Fatal("postgres: ", err) // can't start without the DB it was told to use
		}
		defer pool.Close() // released on shutdown

		s, err := storage.NewPostgresStore(pool)
		if err != nil {
			log.Fatal("postgres: ", err)
		}
		store = s
		users = storage.NewPostgresUserStore(pool)
		log.Println("storage: postgres")
	} else {
		store = storage.NewMemoryStore()
		users = storage.NewMemoryUserStore()
		log.Println("storage: in-memory (set DATABASE_URL to use postgres)")
	}

	// 2. Auth config. The JWT secret MUST come from the environment — a secret
	//    hard-coded in source is in your git history forever, and anyone who
	//    reads the repo can mint valid tokens for any user. The dev fallback
	//    below is a convenience that loudly warns; production must set it.
	secret := []byte(os.Getenv("JWT_SECRET"))
	if len(secret) == 0 {
		secret = []byte("dev-only-insecure-secret-change-me")
		log.Println("WARNING: JWT_SECRET not set — using an insecure dev secret")
	}

	// 3. Build the HTTP layer, injecting the dependencies each part needs.
	h := handlers.New(store)
	authH := handlers.NewAuth(users, secret, 24*time.Hour)

	// 4. Build the Echo app + cross-cutting middleware.
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// 5. Register routes. Auth registers its own; the event handler is handed
	//    the auth middleware so it can protect the routes that need a user.
	authH.Register(e)
	h.Register(e, authH.RequireAuth)

	// 6. Start serving (blocks).
	e.Logger.Fatal(e.Start(":8082"))
}
