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
// Then (JSON POSTs need the Content-Type header — lesson 6 gotcha):
//   curl localhost:8082/health
//   curl localhost:8082/events
//   curl localhost:8082/events/1
//   curl -H "Content-Type: application/json" -X POST localhost:8082/events/1/book -d "{\"seats\":5}"
package main

import (
	"log"
	"os"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

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
	if url := os.Getenv("DATABASE_URL"); url != "" {
		s, err := storage.NewPostgresStore(url)
		if err != nil {
			log.Fatal("postgres:", err) // can't start without the DB it was told to use
		}
		store = s
		log.Println("storage: postgres")
	} else {
		store = storage.NewMemoryStore()
		log.Println("storage: in-memory (set DATABASE_URL to use postgres)")
	}

	// 2. Build the HTTP layer, injecting the store dependency.
	h := handlers.New(store)

	// 3. Build the Echo app + cross-cutting middleware.
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// 4. Let the handler register its routes (and error handler) on Echo.
	h.Register(e)

	// 5. Start serving (blocks). Different port again so it can run alongside
	//    lesson5/lesson6.
	e.Logger.Fatal(e.Start(":8082"))
}
