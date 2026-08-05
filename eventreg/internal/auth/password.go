// Package auth handles credentials: hashing passwords and issuing/verifying
// JWTs. It is deliberately independent of HTTP and of the database, so it can
// be unit-tested on its own and reused anywhere.
package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword turns a plaintext password into a bcrypt hash for storage.
//
// WHY BCRYPT AND NOT SHA-256 — the classic interview question:
//
//  1. SHA/MD5 are FAST, which is exactly wrong here. A GPU computes billions of
//     SHA-256 hashes per second, so a stolen table of SHA'd passwords falls to
//     brute force quickly. bcrypt is deliberately SLOW.
//
//  2. bcrypt has a COST FACTOR (work factor). DefaultCost is 10, meaning 2^10
//     iterations. As hardware gets faster you raise the cost, and bcrypt stays
//     expensive to crack. It is future-proofed by design.
//
//  3. bcrypt SALTS automatically. A salt is random data mixed into each hash, so
//     two users with the same password get different hashes. That defeats
//     rainbow tables (precomputed hash lookups) and stops an attacker learning
//     that two accounts share a password. The salt is stored inside the output
//     string, so there's no separate column to manage.
//
// The cost is real and intentional: hashing takes ~50-100ms. That's negligible
// for a human logging in, and devastating for an attacker trying millions of
// guesses. Never "optimize" it away.
//
// (bcrypt truncates input beyond 72 bytes — a known quirk. For this project
// it's irrelevant; production systems sometimes pre-hash to work around it.)
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword reports whether the plaintext matches the stored hash.
//
// You cannot "decrypt" a bcrypt hash — hashing is one-way. Verification works by
// hashing the supplied password with the SAME salt and cost (both readable from
// the stored hash string) and comparing the results.
//
// CompareHashAndPassword does that comparison in CONSTANT TIME. A naive `==`
// on strings returns early at the first differing byte, so the time it takes
// leaks information about how much of the value was correct — a timing attack.
// Constant-time comparison always takes the same duration regardless of input.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
