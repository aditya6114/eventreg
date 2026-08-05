package models

import "time"

// WaitlistEntry is a place in the queue for a full event.
type WaitlistEntry struct {
	ID        int       `json:"id"`
	EventID   int       `json:"event_id"`
	UserID    int       `json:"user_id"`
	Seats     int       `json:"seats"`
	CreatedAt time.Time `json:"created_at"`

	// Position is COMPUTED at read time (1-based), never stored. Storing a
	// position column would mean renumbering every row behind someone who
	// leaves the queue — an expensive write amplification and a fresh source
	// of races. created_at already encodes the order, so we derive position
	// from it instead. Prefer deriving over duplicating.
	Position int `json:"position,omitempty"`
}

type WaitlistEntryWithEvent struct {
	WaitlistEntry
	EventName string `json:"event_name"`
}

// Outcomes of a booking attempt. Making this an explicit value (rather than
// inferring from which pointer is non-nil) keeps the handler's status-code
// mapping obvious and the JSON self-describing for clients.
const (
	OutcomeBooked            = "booked"             // new booking      -> 201
	OutcomeWaitlisted        = "waitlisted"         // event full       -> 202
	OutcomeAlreadyBooked     = "already_booked"     // idempotent replay-> 200
	OutcomeAlreadyWaitlisted = "already_waitlisted" // idempotent replay-> 200
)

// BookingResult is what Book() returns now that a request has FOUR possible
// endings instead of two. Exactly one of Booking/Waitlist is set.
type BookingResult struct {
	Outcome  string         `json:"outcome"`
	Booking  *Booking       `json:"booking,omitempty"`
	Waitlist *WaitlistEntry `json:"waitlist,omitempty"`
}

// CancelResult reports what a cancellation freed and who it promoted — so the
// caller can see the knock-on effect, which is the interesting part.
type CancelResult struct {
	EventID    int       `json:"event_id"`
	SeatsFreed int       `json:"seats_freed"`
	SeatsLeft  int       `json:"seats_left"`
	Promoted   []Booking `json:"promoted"` // waitlisted users who just got in
}
