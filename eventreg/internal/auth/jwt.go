package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ---------------------------------------------------------------------------
// WHAT A JWT ACTUALLY IS
//
// A JSON Web Token is three base64url-encoded parts joined by dots:
//
//     header.payload.signature
//     eyJhbGciOi...  .  eyJ1c2VyX2lk...  .  4pE3Fs9x...
//
//   header    — which signing algorithm was used (e.g. HS256)
//   payload   — the CLAIMS: who the user is, when it expires
//   signature — HMAC(header + payload, secret)
//
// THE #1 MISCONCEPTION, AND A COMMON INTERVIEW TRAP:
// A JWT is SIGNED, NOT ENCRYPTED. Anyone holding the token can base64-decode the
// payload and read every claim — paste one into jwt.io and see for yourself.
// So NEVER put secrets in a JWT (no passwords, no card numbers, no PII you
// wouldn't hand to the browser).
//
// What the signature guarantees is INTEGRITY, not secrecy: a client can read the
// claims but cannot change them, because altering the payload invalidates the
// signature, and forging a valid signature requires the server's secret.
//
// WHY JWTs FOR THIS PROJECT — the load-balancing link (checklist line 84):
// Server-side sessions store state ("session abc123 = user 42") in the server's
// memory. With two API replicas behind Nginx, a request that lands on replica B
// knows nothing about a session created on replica A — you'd need sticky
// sessions or a shared session store. A JWT is SELF-CONTAINED: it carries the
// user id and its own proof of validity, so ANY replica can verify it with just
// the shared secret and no lookup. That is what makes the API STATELESS, which
// is what makes it horizontally scalable.
//
// THE TRADE-OFF (be ready for this follow-up): you cannot easily revoke a JWT.
// A stolen token stays valid until it expires, because there is no server-side
// record to delete. Mitigations: short expiry (we use 24h; production APIs often
// use minutes plus a refresh token), or a denylist of revoked IDs — which
// reintroduces the shared state JWTs were meant to avoid. Everything is a
// trade-off; knowing this one is what separates "I used a JWT library" from
// "I understand JWTs".
// ---------------------------------------------------------------------------

// Claims is our payload. We embed jwt.RegisteredClaims to get the standard
// fields (exp, iat, sub, iss...) and add our own UserID on top.
//
// Struct embedding — Go's composition-over-inheritance: RegisteredClaims'
// fields and methods are promoted, so Claims satisfies the jwt.Claims interface
// without us implementing anything.
type Claims struct {
	UserID int `json:"user_id"`
	jwt.RegisteredClaims
}

// ErrInvalidToken covers every "this token isn't usable" case: malformed,
// expired, wrong signature. The handler maps it to 401 and — importantly —
// tells the client nothing about WHICH of those it was.
var ErrInvalidToken = errors.New("invalid or expired token")

// IssueToken creates a signed JWT for a user.
func IssueToken(secret []byte, userID int, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			// EXPIRY IS NOT OPTIONAL. A token without exp is valid forever, so
			// one leak is a permanent compromise. This is the single most
			// important claim to set.
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   fmt.Sprintf("%d", userID),
		},
	}

	// HS256 = HMAC with SHA-256: SYMMETRIC, one shared secret both signs and
	// verifies. Fine when the same service does both (our case). If a third
	// party needed to VERIFY tokens without being able to MINT them, you'd use
	// an asymmetric algorithm (RS256) so they only get the public key.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

// ParseToken verifies the signature and expiry, returning the claims.
func ParseToken(secret []byte, tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{},
		func(t *jwt.Token) (any, error) {
			// THE `alg: none` ATTACK — why this callback exists.
			//
			// The token itself states which algorithm to use. A naive parser
			// trusts that field, so an attacker sends a token with the header
			// changed to `"alg": "none"` and NO signature — and the parser
			// happily accepts it. Historically this broke many JWT libraries.
			//
			// We defend by REFUSING to proceed unless the algorithm is the HMAC
			// family we actually issued. Never let the untrusted input choose
			// how it gets verified.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return secret, nil
		},
		// Belt and braces: restrict accepted algorithms at the parser level too.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		// Wrap so callers can errors.Is(err, ErrInvalidToken) (lesson 3) while
		// the underlying detail stays available for server-side logging.
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(*Claims) // type assertion (lesson 3)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
