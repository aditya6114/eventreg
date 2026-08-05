// Package storage owns persistence. Today it's an in-memory map; next
// lesson it becomes Postgres. The key design move is that the rest of the
// app depends on the Store INTERFACE, not on this concrete map — so we can
// swap implementations without touching handlers. This is lesson 3's
// "accept interfaces" turned into project architecture.
package storage

import (
	"sync"
	"eventreg/internal/models" // import path = module name + folder path
)

// Store is the contract the handlers depend on. It's small and focused
// (lesson 3: prefer small interfaces). A Postgres store, a mock store for
// tests, and this memory store all satisfy it.
//
// NOTE (lesson 8): List/Create now return an error. The in-memory map can't
// fail, but a database CAN (network down, bad SQL, etc.), so the contract
// must allow for it. This is interfaces evolving to fit a real implementation
// — a normal thing that happens once a concrete backend appears.
type Store interface {
	List() ([]models.Event, error)
	Get(id int) (models.Event, error)
	Create(e models.Event) (models.Event, error)
	Book(id, seats int) (models.Event, error)
}

// Compile-time proof that *memoryStore satisfies Store. If a signature ever
// drifts, the error shows up HERE, not at some distant call site.
var _ Store = (*memoryStore)(nil)

// memoryStore is UNEXPORTED (lowercase) — outside code can't build one
// directly. They go through NewMemoryStore, which lets us guarantee the map
// is initialized. This "unexported struct + exported constructor" pattern
// is everywhere in Go.
type memoryStore struct {
	mu     sync.Mutex
	events map[int]models.Event
	nextID int
}

// NewMemoryStore returns a Store (the interface), not *memoryStore (the
// concrete type). Returning the interface keeps callers decoupled and is a
// common convention: "accept interfaces, return... well, here an interface
// on purpose so main stays implementation-agnostic."
func NewMemoryStore() Store {
	return &memoryStore{
		events: map[int]models.Event{
			1: {ID: 1, Name: "Coldplay", Seats: 50000},
			2: {ID: 2, Name: "Local Gig", Seats: 200},
		},
		nextID: 2,
	}
}

func (s *memoryStore) List() ([]models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.Event, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
	}
	return out, nil // memory never fails, so error is always nil here
}

func (s *memoryStore) Get(id int) (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return models.Event{}, &models.NotFoundError{EntityID: id}
	}
	return e, nil
}

func (s *memoryStore) Create(e models.Event) (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e.ID = s.nextID
	s.events[e.ID] = e
	return e, nil
}

func (s *memoryStore) Book(id, seats int) (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return models.Event{}, &models.NotFoundError{EntityID: id}
	}
	if seats > e.Seats {
		return models.Event{}, models.ErrSoldOut
	}
	e.Seats -= seats
	s.events[id] = e
	return e, nil
}
