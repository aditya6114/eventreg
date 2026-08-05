package models

import "time"

// Booking records that a specific user reserved seats for a specific event.
// Until now the app only decremented a counter — it had no idea WHO booked.
type Booking struct {
	ID        int       `json:"id"`
	EventID   int       `json:"event_id"`
	UserID    int       `json:"user_id"`
	Seats     int       `json:"seats"`
	CreatedAt time.Time `json:"created_at"`

	// Idempotent tells the caller "this booking already existed; your request
	// was a duplicate and changed nothing." Not persisted — it's a property of
	// THIS response, not of the row. Lets the handler answer 200 (here it is
	// again) instead of 201 (something new was made).
	Idempotent bool `json:"idempotent,omitempty"`
}

// BookingWithEvent is a JOIN result: a booking plus the event it points at, so
// "my bookings" can be answered in ONE query instead of N+1.
type BookingWithEvent struct {
	Booking
	EventName  string `json:"event_name"`
	SeatsLeft  int    `json:"event_seats_left"`
}

// EventStat is an AGGREGATE result (GROUP BY): per-event booking totals.
type EventStat struct {
	EventID      int    `json:"event_id"`
	Name         string `json:"name"`
	SeatsLeft    int    `json:"seats_left"`
	BookingCount int    `json:"booking_count"` // COUNT(*) — how many bookings
	SeatsBooked  int    `json:"seats_booked"`  // SUM(seats) — how many seats sold
}
