// Package handlers is the HTTP layer: it translates requests into store
// calls and store results into responses. It knows about Echo and status
// codes; it does NOT know how data is stored (it only sees storage.Store).
// That separation is the whole point of the layout.
package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"eventreg/internal/models"
	"eventreg/internal/storage"
)

// ErrorResponse is the ONE shape every error in this API takes.
//
// It replaces the anonymous map[string]string we were building inline. Two
// reasons that's an upgrade: OpenAPI can only document a NAMED type (a map
// would appear as an opaque object), and a declared struct makes the error
// contract greppable and impossible to typo — `{"eror": ...}` was previously
// a one-character bug away.
type ErrorResponse struct {
	Error string `json:"error" example:"event not found"`
}

// BookRequest is the body of POST /events/{id}/book.
//
// Note what is NOT here: no user_id. Identity comes from the verified JWT, so
// there is nothing for a client to forge. The request body should only carry
// what the client is genuinely entitled to decide.
type BookRequest struct {
	Seats int `json:"seats" example:"2"`
}

// EventHandler bundles the dependencies its methods need. Here that's just
// the store. This is DEPENDENCY INJECTION: main() creates the store and
// hands it in, rather than handlers reaching for a global. It's what makes
// the code testable — a test can inject a fake Store.
type EventHandler struct {
	Store storage.Store
}

// New builds the handler with its dependency.
func New(s storage.Store) *EventHandler {
	return &EventHandler{Store: s}
}

// Register wires all routes and the central error handler onto an Echo
// instance. Keeping routing next to the handlers (not in main) means main
// stays a thin wiring file.
// Register wires the event routes. `protect` is the auth middleware; passing it
// in (rather than importing AuthHandler here) keeps this package unaware of how
// authentication works — it only knows "wrap this route in that".
//
// PUBLIC vs PROTECTED is a deliberate product decision, not an accident:
//   - browsing events is public (you must see what's on offer before signing up)
//   - BOOKING requires a login, because a booking belongs to a person
func (h *EventHandler) Register(e *echo.Echo, protect echo.MiddlewareFunc) {
	e.HTTPErrorHandler = errorHandler // central mapping, defined below

	// --- public ---
	e.GET("/health", h.health)
	e.GET("/events", h.listEvents)
	// NOTE: /events/stats must be registered BEFORE /events/:id would be a
	// concern in some routers, because "stats" could match the :id pattern.
	// Echo prioritises static segments over params, so this is safe — but it's
	// a real trap in routers that match in declaration order.
	e.GET("/events/stats", h.eventStats)
	e.GET("/events/:id", h.getEvent)

	// --- protected: extra middleware args apply to THIS route only ---
	e.POST("/events", h.createEvent, protect)
	e.POST("/events/:id/book", h.bookEvent, protect)
	// Same resource, different verb: POST creates the booking, DELETE removes
	// it. That's REST done properly — the URL names the THING, the method says
	// what you're doing to it. (Not /events/:id/cancelBooking — verbs belong in
	// the method, not the path.)
	e.DELETE("/events/:id/book", h.cancelBooking, protect)
	// "my bookings" — /me/* is the REST convention for "the current user",
	// avoiding a user id in the URL that someone would inevitably tamper with.
	e.GET("/me/bookings", h.myBookings, protect)
	e.GET("/me/waitlist", h.myWaitlist, protect)
}

// health godoc
//
//	@Summary		Liveness check
//	@Description	Returns "ok" if the process is up. Used by Docker/Kubernetes probes.
//	@Tags			meta
//	@Produce		plain
//	@Success		200	{string}	string	"ok"
//	@Router			/health [get]
func (h *EventHandler) health(c echo.Context) error {
	return c.String(http.StatusOK, "ok")
}

// listEvents supports ?limit= and ?offset= — e.g. /events?limit=10&offset=20.
//
// Query params, not path params: they're OPTIONAL filters on a collection, not
// part of the resource's identity. /events is the same resource whether you
// asked for 10 of them or 100.
//
//	@Summary		List events (paginated)
//	@Description	Public. Returns a page envelope: {items, total, limit, offset, has_more}.
//	@Description	`limit` defaults to 20 and is clamped to a maximum of 100.
//	@Tags			events
//	@Produce		json
//	@Param			limit	query		int	false	"page size (default 20, max 100)"
//	@Param			offset	query		int	false	"rows to skip (default 0)"
//	@Success		200		{object}	models.EventPage
//	@Failure		500		{object}	handlers.ErrorResponse
//	@Router			/events [get]
func (h *EventHandler) listEvents(c echo.Context) error {
	// Missing params come back as "" and parse to 0, which NormalizePagination
	// turns into the defaults — so /events with no query string just works.
	// We ignore parse errors deliberately: garbage like ?limit=abc falls back to
	// the default rather than 400-ing. For a filter (as opposed to a required
	// field) being forgiving is the friendlier choice.
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))

	page, err := h.Store.List(limit, offset)
	if err != nil {
		return err // DB failure -> central handler -> 500 (no leak)
	}
	return c.JSON(http.StatusOK, page)
}

//	@Summary		Get one event
//	@Tags			events
//	@Produce		json
//	@Param			id	path		int	true	"event id"
//	@Success		200	{object}	models.Event
//	@Failure		400	{object}	handlers.ErrorResponse	"id is not a number"
//	@Failure		404	{object}	handlers.ErrorResponse	"no such event"
//	@Router			/events/{id} [get]
func (h *EventHandler) getEvent(c echo.Context) error {
	id, err := intParam(c, "id")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "id must be a number")
	}
	e, err := h.Store.Get(id)
	if err != nil {
		return err // bubble to central handler -> 404
	}
	return c.JSON(http.StatusOK, e)
}

//	@Summary		Create an event
//	@Description	Requires authentication. `id` in the body is ignored — the database assigns it.
//	@Tags			events
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			event	body		models.Event	true	"name and seats"
//	@Success		201		{object}	models.Event
//	@Failure		400		{object}	handlers.ErrorResponse	"malformed JSON"
//	@Failure		401		{object}	handlers.ErrorResponse	"missing or invalid token"
//	@Failure		422		{object}	handlers.ErrorResponse	"name empty or seats <= 0"
//	@Router			/events [post]
func (h *EventHandler) createEvent(c echo.Context) error {
	var in models.Event
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if in.Name == "" || in.Seats <= 0 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "name required and seats must be > 0")
	}
	created, err := h.Store.Create(in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, created)
}

//	@Summary		Book seats (auto-waitlists if full)
//	@Description	Requires authentication. The booking is attributed to the token's user.
//	@Description
//	@Description	**Idempotent**: repeating the same request returns the original booking
//	@Description	with `outcome: already_booked` and does not consume more seats.
//	@Description
//	@Description	Status codes carry the outcome:
//	@Description	- **201** `booked` — a new booking exists
//	@Description	- **202** `waitlisted` — event was full, you were queued
//	@Description	- **200** `already_booked` / `already_waitlisted` — replay, nothing changed
//	@Tags			bookings
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		int						true	"event id"
//	@Param			body	body		handlers.BookRequest	true	"seats to book"
//	@Success		201		{object}	models.BookingResult	"booked"
//	@Success		202		{object}	models.BookingResult	"waitlisted"
//	@Success		200		{object}	models.BookingResult	"idempotent replay"
//	@Failure		401		{object}	handlers.ErrorResponse
//	@Failure		404		{object}	handlers.ErrorResponse	"no such event"
//	@Failure		422		{object}	handlers.ErrorResponse	"seats <= 0"
//	@Router			/events/{id}/book [post]
func (h *EventHandler) bookEvent(c echo.Context) error {
	id, err := intParam(c, "id")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "id must be a number")
	}
	// WHO is booking comes from the VERIFIED JWT (set by RequireAuth), never
	// from the request body. If the client could send its own user_id, anyone
	// could book on anyone else's behalf — a textbook broken-access-control bug.
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}

	// Named type, not an anonymous struct: OpenAPI can only reference a named
	// schema, and it makes the request contract visible from outside the file.
	var body BookRequest
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if body.Seats <= 0 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "seats must be > 0")
	}

	res, err := h.Store.Book(id, userID, body.Seats)
	if err != nil {
		return err // 404 not found, decided centrally
	}

	// THE STATUS CODE CARRIES THE MEANING. Four outcomes, four honest answers:
	//
	//   201 Created  — a new booking exists
	//   202 Accepted — request accepted, NOT yet fulfilled: you're queued.
	//                  202 is exactly what it's for — "I took this, it's
	//                  pending" — and far more truthful than a 200 that hides
	//                  the fact you have no seat, or a 409 that implies
	//                  failure when we did something useful for you.
	//   200 OK       — you already had this booking/place; nothing changed
	switch res.Outcome {
	case models.OutcomeBooked:
		return c.JSON(http.StatusCreated, res)
	case models.OutcomeWaitlisted:
		return c.JSON(http.StatusAccepted, res)
	default: // already_booked / already_waitlisted — idempotent replays
		return c.JSON(http.StatusOK, res)
	}
}

// cancelBooking releases the user's seats and promotes from the waitlist.
//
// DELETE is the right verb: it removes a resource (this user's booking). The
// promotion is a side effect the response reports, so the client can see that
// their cancellation actually let someone else in.
//	@Summary		Cancel a booking (promotes from the waitlist)
//	@Description	Requires authentication. Frees your seats and immediately promotes the
//	@Description	longest-waiting users who fit — all in one transaction, so the freed seats
//	@Description	cannot be taken by a new arrival before the queue gets them.
//	@Description	The response lists who was promoted.
//	@Tags			bookings
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		int	true	"event id"
//	@Success		200	{object}	models.CancelResult
//	@Failure		401	{object}	handlers.ErrorResponse
//	@Failure		404	{object}	handlers.ErrorResponse	"you have no booking for this event"
//	@Router			/events/{id}/book [delete]
func (h *EventHandler) cancelBooking(c echo.Context) error {
	id, err := intParam(c, "id")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "id must be a number")
	}
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	res, err := h.Store.CancelBooking(id, userID)
	if err != nil {
		return err // 404 if they had no booking
	}
	return c.JSON(http.StatusOK, res)
}

// myWaitlist shows what the user is queued for, and their position in each line.
//	@Summary		My waitlist entries (with queue position)
//	@Description	Requires authentication. `position` is 1-based and computed at read time
//	@Description	from arrival order — it is not stored, so it is never stale.
//	@Tags			me
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{array}		models.WaitlistEntryWithEvent
//	@Failure		401	{object}	handlers.ErrorResponse
//	@Router			/me/waitlist [get]
func (h *EventHandler) myWaitlist(c echo.Context) error {
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	list, err := h.Store.ListUserWaitlist(userID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

// myBookings — the JOIN endpoint. Scoped to the authenticated user, so there's
// no way to read someone else's bookings by changing a URL parameter.
//	@Summary		My bookings
//	@Description	Requires authentication. Scoped to the token's user — there is no id in the
//	@Description	URL to tamper with, so you cannot read anyone else's bookings.
//	@Tags			me
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{array}		models.BookingWithEvent
//	@Failure		401	{object}	handlers.ErrorResponse
//	@Router			/me/bookings [get]
func (h *EventHandler) myBookings(c echo.Context) error {
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	list, err := h.Store.ListUserBookings(userID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

// eventStats — the GROUP BY endpoint: how each event is selling.
//	@Summary		Per-event sales stats
//	@Description	Public. Booking count and seats sold per event, via GROUP BY with a
//	@Description	LEFT JOIN — so events with zero bookings still appear.
//	@Tags			events
//	@Produce		json
//	@Success		200	{array}		models.EventStat
//	@Failure		500	{object}	handlers.ErrorResponse
//	@Router			/events/stats [get]
func (h *EventHandler) eventStats(c echo.Context) error {
	stats, err := h.Store.EventStats()
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, stats)
}

// errorHandler maps domain errors -> HTTP responses in ONE place (lesson 6).
// Because models exports ErrSoldOut and NotFoundError, this cross-package
// mapping just works with errors.Is / errors.As.
func errorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}
	status := http.StatusInternalServerError
	msg := "internal server error"

	// Auth errors first (lesson 10): 401 for bad/missing credentials,
	// 409 for a duplicate email. Kept in auth.go next to the code that
	// produces them, so the mapping lives beside its reasoning.
	if s, m, handled := authStatus(err); handled {
		_ = c.JSON(s, ErrorResponse{Error: m})
		return
	}

	var nf *models.NotFoundError
	switch {
	case errors.Is(err, models.ErrSoldOut):
		status, msg = http.StatusConflict, err.Error()
	case errors.As(err, &nf):
		status, msg = http.StatusNotFound, err.Error()
	default:
		var he *echo.HTTPError
		if errors.As(err, &he) {
			status = he.Code
			if m, ok := he.Message.(string); ok {
				msg = m
			}
		}
	}
	_ = c.JSON(status, ErrorResponse{Error: msg})
}

func intParam(c echo.Context, name string) (int, error) {
	var id int
	err := echo.PathParamsBinder(c).Int(name, &id).BindError()
	return id, err
}
