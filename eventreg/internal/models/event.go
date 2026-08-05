// Package models holds the core domain types and errors — the "nouns" of
// the system. One folder = one package (the package name is `models`, and
// it matches the folder name by convention).
//
// It lives under internal/ which has a special meaning in Go: code inside
// an internal/ directory can ONLY be imported by code rooted in the parent
// of internal/. So eventreg/internal/models is importable by anything in
// eventreg, but NOT by some other module that depends on eventreg. It's
// the language-enforced way to say "this is a private implementation detail,
// not public API."
//
// Notice there is NO func main() here and no `package main`. Only the entry
// point in cmd/api is `package main`; every other package is a library.
package models

import "fmt"

// Event is exported (capital E) so other packages can use it. Same struct
// tags as before — the JSON contract travels with the type.
type Event struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Seats int    `json:"seats"`
}

// ErrSoldOut is a sentinel error (lesson 3). It's exported so the handlers
// package can compare against it with errors.Is. Package-qualified from
// outside, callers write models.ErrSoldOut.
var ErrSoldOut = fmt.Errorf("event is sold out")

// NotFoundError is a typed error carrying which id was missing (lesson 3),
// so the handler can turn it into a 404 via errors.As.
type NotFoundError struct{ EntityID int }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("event %d not found", e.EntityID)
}
