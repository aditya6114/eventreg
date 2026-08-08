package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"eventreg/internal/auth"
	"eventreg/internal/models"
	"eventreg/internal/storage"
)

// userIDKey is the key under which the authenticated user's id is stored on the
// request context. It's an unexported custom type rather than a bare string so
// no other package can accidentally collide with (or forge) it.
const userIDKey = "auth_user_id"

// AuthHandler owns registration, login, and the JWT middleware.
//
// Note it depends on storage.UserStore — the SMALL interface — not on the full
// Store. It has no access to events, and couldn't book a seat if it tried.
// That's interface segregation doing real work.
type AuthHandler struct {
	Users    storage.UserStore
	Secret   []byte
	TokenTTL time.Duration
}

func NewAuth(users storage.UserStore, secret []byte, ttl time.Duration) *AuthHandler {
	return &AuthHandler{Users: users, Secret: secret, TokenTTL: ttl}
}

// Register wires the auth routes.
//
// `extra` is applied to all three routes — main.go uses it to attach a STRICTER
// rate limiter here than the global one, because /auth/login is a password
// oracle and every attempt costs ~100ms of bcrypt.
//
// WHY VARIADIC MIDDLEWARE RATHER THAN echo.Group: a first attempt used
// `e.Group("/auth", limiter)` in main.go, which compiled, ran, and did
// NOTHING — a group only applies its middleware to routes registered ON THE
// GROUP. These routes are registered on the root instance, so the group was an
// empty shell. Silent no-ops are the worst kind of bug; passing the middleware
// explicitly to the routes that need it can't fail that way.
func (h *AuthHandler) Register(e *echo.Echo, extra ...echo.MiddlewareFunc) {
	e.POST("/auth/register", h.register, extra...)
	e.POST("/auth/login", h.login, extra...)
	// /auth/me is protected — it proves the middleware works and shows how to
	// read the authenticated user back out of the context.
	e.GET("/auth/me", h.me, append([]echo.MiddlewareFunc{h.RequireAuth}, extra...)...)
}

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type TokenResponse struct {
	Token string      `json:"token"`
	User  models.User `json:"user"`
}

//	@Summary		Register a new account
//	@Description	Creates the user and returns a JWT immediately, so a fresh signup does not
//	@Description	have to log in with the credentials it just typed.
//	@Description	Passwords are stored as bcrypt hashes and are never returned.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			credentials	body		handlers.Credentials	true	"email and password (min 8 chars)"
//	@Success		201			{object}	handlers.TokenResponse
//	@Failure		400			{object}	handlers.ErrorResponse	"malformed JSON"
//	@Failure		409			{object}	handlers.ErrorResponse	"email already registered"
//	@Failure		422			{object}	handlers.ErrorResponse	"invalid email or password too short"
//	@Router			/auth/register [post]
func (h *AuthHandler) register(c echo.Context) error {
	var in Credentials
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if !strings.Contains(in.Email, "@") || in.Email == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "a valid email is required")
	}
	// A minimum length is the cheapest real protection against trivially
	// guessable passwords. Modern guidance (NIST) favours length over forced
	// symbol/number rules, which mostly produce "Passw0rd!" and annoyance.
	if len(in.Password) < 8 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "password must be at least 8 characters")
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err // -> 500, details logged server-side, never shown to client
	}

	user, err := h.Users.CreateUser(in.Email, hash)
	if err != nil {
		return err // ErrEmailTaken -> 409 via the central error handler
	}

	token, err := auth.IssueToken(h.Secret, user.ID, h.TokenTTL)
	if err != nil {
		return err
	}
	// 201 Created: registering created a new resource (the user).
	// user serializes WITHOUT the password hash thanks to `json:"-"`.
	return c.JSON(http.StatusCreated, TokenResponse{Token: token, User: user})
}

//	@Summary		Log in and get a JWT
//	@Description	Returns 200 (not 201) — logging in creates no resource.
//	@Description
//	@Description	An unknown email and a wrong password return the SAME 401 with the SAME
//	@Description	message, deliberately: distinguishing them would let an attacker enumerate
//	@Description	which addresses have accounts.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			credentials	body		handlers.Credentials	true	"email and password"
//	@Success		200			{object}	handlers.TokenResponse
//	@Failure		400			{object}	handlers.ErrorResponse	"malformed JSON"
//	@Failure		401			{object}	handlers.ErrorResponse	"invalid email or password"
//	@Router			/auth/login [post]
func (h *AuthHandler) login(c echo.Context) error {
	var in Credentials
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}

	user, err := h.Users.GetUserByEmail(in.Email)
	if err != nil {
		// Note: the store already converted "no such user" into the GENERIC
		// ErrInvalidCredentials, so we can't leak which emails exist even by
		// accident here.
		return err // -> 401
	}

	if !auth.CheckPassword(user.PasswordHash, in.Password) {
		// SAME error, SAME status as an unknown email. Indistinguishable from
		// the outside — that's the point.
		return models.ErrInvalidCredentials // -> 401
	}

	token, err := auth.IssueToken(h.Secret, user.ID, h.TokenTTL)
	if err != nil {
		return err
	}
	// 200 OK, not 201: logging in didn't create a resource.
	return c.JSON(http.StatusOK, TokenResponse{Token: token, User: user})
}

//	@Summary		Who am I
//	@Description	Requires authentication. Returns the account belonging to the token —
//	@Description	the simplest proof that the auth middleware is working.
//	@Tags			auth
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	models.User
//	@Failure		401	{object}	handlers.ErrorResponse
//	@Router			/auth/me [get]
func (h *AuthHandler) me(c echo.Context) error {
	id, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	user, err := h.Users.GetUserByID(id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, user)
}

// RequireAuth is MIDDLEWARE: a function that wraps a handler, running before it
// and able to reject the request outright.
//
// Signature note: Echo middleware is `func(next echo.HandlerFunc) echo.HandlerFunc`
// — it RECEIVES the next handler and RETURNS a replacement. Calling next(c)
// passes control down the chain; returning early without calling it stops the
// request. That "wrap the next thing" shape is the middleware pattern in every
// framework (Express, Django, ASP.NET), not just Echo.
//
// This is the checklist's "auth middleware": one place enforces authentication
// for every protected route, instead of each handler re-checking the token.
func (h *AuthHandler) RequireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// The standard is the Authorization header with the "Bearer" scheme:
		//     Authorization: Bearer eyJhbGciOi...
		header := c.Request().Header.Get("Authorization")
		if header == "" {
			return models.ErrUnauthorized // -> 401
		}
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return models.ErrUnauthorized
		}

		claims, err := auth.ParseToken(h.Secret, parts[1])
		if err != nil {
			// Deliberately does NOT tell the client whether the token was
			// expired, malformed, or badly signed — that's free reconnaissance.
			// The real reason is available server-side in logs.
			return models.ErrUnauthorized
		}

		// Stash the user id so downstream handlers can read it WITHOUT
		// re-parsing the token. The handler never touches JWTs at all — it just
		// asks "who is this?". Middleware did the work once.
		c.Set(userIDKey, claims.UserID)
		return next(c) // continue down the chain
	}
}

// UserIDFrom pulls the authenticated user's id out of the request context.
// Returns ok=false if the route wasn't behind RequireAuth.
func UserIDFrom(c echo.Context) (int, bool) {
	v := c.Get(userIDKey)
	id, ok := v.(int) // type assertion (lesson 3)
	return id, ok
}

// authStatus maps auth-related domain errors to HTTP statuses. Called by the
// central error handler in handlers.go.
//
// 401 vs 403 — THE distinction interviewers probe:
//
//	401 Unauthorized — "I don't know who you are." No/invalid credentials.
//	                   Fix by logging in. (Misnamed; it really means
//	                   *unauthenticated*.)
//	403 Forbidden    — "I know exactly who you are, and you still can't do
//	                   this." Authenticated but lacking permission. Logging in
//	                   again changes nothing.
//
// We only produce 401 so far, because every logged-in user may do everything.
// 403 arrives with roles (e.g. only an organiser may create events).
func authStatus(err error) (int, string, bool) {
	switch {
	case errors.Is(err, models.ErrInvalidCredentials):
		return http.StatusUnauthorized, err.Error(), true
	case errors.Is(err, models.ErrUnauthorized):
		return http.StatusUnauthorized, "authentication required", true
	case errors.Is(err, models.ErrEmailTaken):
		// 409 Conflict: the request is valid but conflicts with existing state
		// — the same reasoning as a sold-out event.
		return http.StatusConflict, err.Error(), true
	}
	return 0, "", false
}
