// Package handlers is the HTTP layer: it translates requests into store
// calls and store results into responses. It knows about Echo and status
// codes; it does NOT know how data is stored (it only sees storage.Store).
// That separation is the whole point of the layout.
package handlers

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"eventreg/internal/models"
	"eventreg/internal/storage"
)

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

func (h *EventHandler) health(c echo.Context) error {
	return c.String(http.StatusOK, "ok")
}

func (h *EventHandler) listEvents(c echo.Context) error {
	events, err := h.Store.List()
	if err != nil {
		return err // DB failure -> central handler -> 500 (no leak)
	}
	return c.JSON(http.StatusOK, events)
}

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

	var body struct {
		Seats int `json:"seats"`
	}
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
		_ = c.JSON(s, map[string]string{"error": m})
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
	_ = c.JSON(status, map[string]string{"error": msg})
}

func intParam(c echo.Context, name string) (int, error) {
	var id int
	err := echo.PathParamsBinder(c).Int(name, &id).BindError()
	return id, err
}
