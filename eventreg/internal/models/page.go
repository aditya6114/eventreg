package models

// Page is a paginated slice of results.
//
// GENERICS (Go 1.18+): `[T any]` makes this ONE type usable as
// Page[Event], Page[Booking], Page[User]... instead of writing a near-identical
// EventPage, BookingPage, UserPage. `any` is the constraint — it means "T can be
// any type at all" (it's an alias for interface{}).
//
// This is a textbook GOOD use of generics: a CONTAINER whose logic is identical
// regardless of what's inside. The bad use is reaching for type parameters when
// an interface would do — if you need behaviour from T, take an interface; use
// generics when you need to preserve the concrete type.
//
// Note the envelope is an OBJECT, not a bare array. Returning `[...]` at the top
// level leaves nowhere to put total/limit/offset, and adding them later becomes
// a breaking change for every client. Wrap from day one.
type Page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`  // total matching rows, ignoring limit/offset
	Limit  int `json:"limit"`  // page size actually applied (may be clamped)
	Offset int `json:"offset"` // rows skipped

	// HasMore saves the client doing offset+limit < total arithmetic, and lets
	// us change the underlying strategy (e.g. to cursors) without breaking them.
	HasMore bool `json:"has_more"`
}

// EventPage is Page[Event] under a concrete, non-generic name.
//
// WHY THIS EXISTS: OpenAPI has no concept of generics — every schema must be a
// concrete named type — and swaggo cannot resolve an instantiated generic like
// `Page[Event]` in an annotation. So the Swagger annotations reference
// models.EventPage instead.
//
// `=` makes this a type ALIAS, not a new type: EventPage and Page[Event] are
// literally the same type to the compiler. No conversion, no duplication, no
// runtime cost — purely a second name so the documentation tooling has
// something it can point at.
//
// A small, honest tax for using generics in an API that must also produce an
// OpenAPI spec. Worth knowing before you reach for generics on a public type.
type EventPage = Page[Event]

// Pagination limits. Exported so handlers and stores agree on one definition.
const (
	DefaultLimit = 20  // sensible page size when the client doesn't ask
	MaxLimit     = 100 // hard ceiling
)

// NormalizePagination clamps client input into a safe range.
//
// WHY A CEILING IS NOT OPTIONAL: without one, `?limit=10000000` lets any caller
// ask the database for the entire table in a single request — accidental or
// deliberate denial of service. Clamping (rather than erroring) is friendlier:
// the response reports the limit actually used, so the client can see it.
func NormalizePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
