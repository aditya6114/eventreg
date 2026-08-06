package models

import (
	"errors"
	"time"
)

// User is a registered account.
//
// LOOK AT THE PasswordHash TAG. `json:"-"` tells encoding/json to NEVER
// serialize this field. Without it, any handler that returns a User would leak
// the bcrypt hash straight to the client — and a leaked hash is an offline
// brute-force target. One character of struct tag closes that hole for every
// present and future endpoint, which is far safer than remembering to strip it
// by hand at each call site.
//
// Note we store the HASH, never the password. The system is designed so that
// nobody — including us, including anyone who steals a database dump — can read
// a user's password.

type User struct {
	ID           int       `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// ErrInvalidCredentials is returned when either the email doesn't exist OR the
// password is wrong.
//
// DELIBERATELY ONE ERROR FOR BOTH CASES. If login said "no such user" vs "wrong
// password", an attacker could enumerate which email addresses have accounts —
// useful for targeted phishing and credential stuffing. Same message, same
// status code, no information leaked. This is a real security practice, and a
// good thing to mention in an interview.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrEmailTaken is returned when registering an email that already exists.
// (Arguably this leaks the same information as above — a trade-off almost every
// real signup form accepts, because the alternative is a confusing UX.)
var ErrEmailTaken = errors.New("email already registered")

// ErrUnauthorized means "no valid credentials were presented" -> HTTP 401.
var ErrUnauthorized = errors.New("unauthorized")
