// Package storage owns persistence. Today it's an in-memory map; next
// lesson it becomes Postgres. The key design move is that the rest of the
// app depends on the Store INTERFACE, not on this concrete map — so we can
// swap implementations without touching handlers. This is lesson 3's
// "accept interfaces" turned into project architecture.
package storage

import (
	"sort"
	"sync"
	"time"

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

	// Book now takes userID (lesson 11): a booking belongs to a PERSON, and
	// recording that is what enables idempotency, "my bookings", and stats.
	//
	// Lesson 12: it returns a BookingResult because a full event no longer
	// means failure — the user is auto-enqueued on the waitlist instead.
	Book(eventID, userID, seats int) (models.BookingResult, error)

	// CancelBooking releases a booking's seats AND promotes whoever has been
	// waiting longest. Cancellation is what makes a waitlist meaningful.
	CancelBooking(eventID, userID int) (models.CancelResult, error)

	// ListUserBookings answers "what have I booked?" with a JOIN.
	ListUserBookings(userID int) ([]models.BookingWithEvent, error)

	// ListUserWaitlist answers "what am I queued for, and where am I?"
	ListUserWaitlist(userID int) ([]models.WaitlistEntryWithEvent, error)

	// EventStats answers "how is each event selling?" with GROUP BY.
	EventStats() ([]models.EventStat, error)
}

// Compile-time proof that *memoryStore satisfies Store. If a signature ever
// drifts, the error shows up HERE, not at some distant call site.
var _ Store = (*memoryStore)(nil)

// memoryStore is UNEXPORTED (lowercase) — outside code can't build one
// directly. They go through NewMemoryStore, which lets us guarantee the map
// is initialized. This "unexported struct + exported constructor" pattern
// is everywhere in Go.
type memoryStore struct {
	mu            sync.Mutex
	events        map[int]models.Event
	nextID        int
	bookings      map[int]models.Booking // lesson 11: who booked what
	nextBookingID int

	waitlist       map[int]models.WaitlistEntry // lesson 12: the queue
	nextWaitlistID int
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
		nextID:   2,
		bookings: map[int]models.Booking{},
		waitlist: map[int]models.WaitlistEntry{},
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

// Book mirrors the Postgres behaviour, including idempotency. Note how much
// more code it takes to do by hand what ON CONFLICT gives us in one clause —
// and it's only safe because the mutex serializes the whole method.
func (s *memoryStore) Book(eventID, userID, seats int) (models.BookingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.events[eventID]
	if !ok {
		return models.BookingResult{}, &models.NotFoundError{EntityID: eventID}
	}

	// Idempotency: already booked?
	for _, b := range s.bookings {
		if b.EventID == eventID && b.UserID == userID {
			b.Idempotent = true
			return models.BookingResult{Outcome: models.OutcomeAlreadyBooked, Booking: &b}, nil
		}
	}
	// Idempotency: already waitlisted?
	for _, w := range s.waitlist {
		if w.EventID == eventID && w.UserID == userID {
			return models.BookingResult{Outcome: models.OutcomeAlreadyWaitlisted, Waitlist: &w}, nil
		}
	}

	// No room -> join the queue.
	if seats > e.Seats {
		s.nextWaitlistID++
		w := models.WaitlistEntry{
			ID: s.nextWaitlistID, EventID: eventID, UserID: userID,
			Seats: seats, CreatedAt: time.Now(),
		}
		s.waitlist[w.ID] = w
		return models.BookingResult{Outcome: models.OutcomeWaitlisted, Waitlist: &w}, nil
	}

	s.nextBookingID++
	b := models.Booking{
		ID:        s.nextBookingID,
		EventID:   eventID,
		UserID:    userID,
		Seats:     seats,
		CreatedAt: time.Now(),
	}
	s.bookings[b.ID] = b

	e.Seats -= seats
	s.events[eventID] = e
	return models.BookingResult{Outcome: models.OutcomeBooked, Booking: &b}, nil
}

func (s *memoryStore) CancelBooking(eventID, userID int) (models.CancelResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.events[eventID]
	if !ok {
		return models.CancelResult{}, &models.NotFoundError{EntityID: eventID}
	}

	freed := 0
	found := false
	for id, b := range s.bookings {
		if b.EventID == eventID && b.UserID == userID {
			freed = b.Seats
			delete(s.bookings, id)
			found = true
			break
		}
	}
	if !found {
		return models.CancelResult{}, &models.NotFoundError{EntityID: eventID}
	}

	available := e.Seats + freed
	promoted := make([]models.Booking, 0)

	// Strict FIFO promotion, same rule as Postgres: oldest first, stop if the
	// next in line doesn't fit.
	for available > 0 {
		queue := make([]models.WaitlistEntry, 0)
		for _, w := range s.waitlist {
			if w.EventID == eventID {
				queue = append(queue, w)
			}
		}
		if len(queue) == 0 {
			break
		}
		sort.Slice(queue, func(i, j int) bool { return queue[i].CreatedAt.Before(queue[j].CreatedAt) })
		next := queue[0]
		if next.Seats > available {
			break
		}
		s.nextBookingID++
		b := models.Booking{
			ID: s.nextBookingID, EventID: next.EventID, UserID: next.UserID,
			Seats: next.Seats, CreatedAt: time.Now(),
		}
		s.bookings[b.ID] = b
		delete(s.waitlist, next.ID)
		available -= next.Seats
		promoted = append(promoted, b)
	}

	e.Seats = available
	s.events[eventID] = e
	return models.CancelResult{
		EventID: eventID, SeatsFreed: freed, SeatsLeft: available, Promoted: promoted,
	}, nil
}

func (s *memoryStore) ListUserWaitlist(userID int) ([]models.WaitlistEntryWithEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Compute position per event — the manual equivalent of ROW_NUMBER()
	// OVER (PARTITION BY event_id ORDER BY created_at).
	byEvent := map[int][]models.WaitlistEntry{}
	for _, w := range s.waitlist {
		byEvent[w.EventID] = append(byEvent[w.EventID], w)
	}
	out := make([]models.WaitlistEntryWithEvent, 0)
	for eventID, queue := range byEvent {
		sort.Slice(queue, func(i, j int) bool { return queue[i].CreatedAt.Before(queue[j].CreatedAt) })
		for i, w := range queue {
			if w.UserID != userID {
				continue
			}
			w.Position = i + 1
			out = append(out, models.WaitlistEntryWithEvent{
				WaitlistEntry: w,
				EventName:     s.events[eventID].Name,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *memoryStore) ListUserBookings(userID int) ([]models.BookingWithEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.BookingWithEvent, 0)
	for _, b := range s.bookings {
		if b.UserID != userID {
			continue
		}
		e := s.events[b.EventID] // the "join", done by hand
		out = append(out, models.BookingWithEvent{
			Booking:   b,
			EventName: e.Name,
			SeatsLeft: e.Seats,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *memoryStore) EventStats() ([]models.EventStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The manual equivalent of GROUP BY: bucket rows by key, then aggregate.
	stats := make(map[int]*models.EventStat, len(s.events))
	for _, e := range s.events {
		stats[e.ID] = &models.EventStat{EventID: e.ID, Name: e.Name, SeatsLeft: e.Seats}
	}
	for _, b := range s.bookings {
		if st, ok := stats[b.EventID]; ok {
			st.BookingCount++
			st.SeatsBooked += b.Seats
		}
	}
	out := make([]models.EventStat, 0, len(stats))
	for _, st := range stats {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SeatsBooked != out[j].SeatsBooked {
			return out[i].SeatsBooked > out[j].SeatsBooked
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}
