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

	"eventreg/internal/holds"
	"eventreg/internal/models"
	"eventreg/internal/notify"
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
	// Holds is OPTIONAL — nil when Redis isn't configured. Every use is
	// nil-guarded so the API keeps working without Redis, same principle as
	// the cache: an optimization, never a dependency.
	Holds *holds.Manager

	// Notifier is OPTIONAL for the same reason, and here it matters most: a
	// booking must succeed whether or not anyone can be told about it.
	Notifier *notify.Client

	// Users lets us look up an email to notify. Optional alongside Notifier.
	Users storage.UserStore
}

// New builds the handler with its dependency.
func New(s storage.Store) *EventHandler {
	return &EventHandler{Store: s}
}

// WithHolds enables the seat-hold endpoints. Returning the handler allows
// chaining, and keeps New() unchanged for callers that don't need holds.
func (h *EventHandler) WithHolds(m *holds.Manager) *EventHandler {
	h.Holds = m
	return h
}

// WithNotifier enables outbound notifications. Needs the user store too, to
// resolve an email address from the id in the JWT.
func (h *EventHandler) WithNotifier(n *notify.Client, users storage.UserStore) *EventHandler {
	h.Notifier = n
	h.Users = users
	return h
}

// notifyBooking fires notifications for a booking outcome.
//
// Every call site is best-effort by construction: notify.Send returns
// immediately and never reports failure, so there is no way for a notification
// problem to affect the booking. That's the graceful-degradation guarantee
// enforced by the SHAPE of the API rather than by remembering to ignore errors.
func (h *EventHandler) notifyBooking(userID int, ev models.Event, res models.BookingResult) {
	if h.Notifier == nil || h.Users == nil {
		return
	}
	user, err := h.Users.GetUserByID(userID)
	if err != nil {
		return // can't notify someone we can't identify; not worth failing over
	}

	var (
		t     notify.Type
		seats int
	)
	switch res.Outcome {
	case models.OutcomeBooked:
		t, seats = notify.TypeBookingConfirmed, res.Booking.Seats
	case models.OutcomeWaitlisted:
		t, seats = notify.TypeWaitlisted, res.Waitlist.Seats
	default:
		// An idempotent replay: they were already told the first time. Sending
		// again would mean an email per retry.
		return
	}

	h.Notifier.Send(notify.Notification{
		UserID:         userID,
		Email:          user.Email,
		Type:           t,
		EventID:        ev.ID,
		EventName:      ev.Name,
		Seats:          seats,
		IdempotencyKey: notify.Key(userID, ev.ID, t),
	})
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
	e.GET("/ready", h.ready)
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

	// Seat holds (lesson 18). Registered only when Redis is available —
	// advertising an endpoint that can't work would be worse than omitting it.
	if h.Holds != nil {
		e.POST("/events/:id/hold", h.createHold, protect)
		e.DELETE("/events/:id/hold", h.releaseHold, protect)
	}
}

// @Summary		Hold seats temporarily (checkout window)
// @Description	Requires authentication. Reserves seats for a few minutes so the user can
// @Description	complete checkout without someone else taking them.
// @Description
// @Description	The hold **expires by itself** — if the user abandons checkout, the seats
// @Description	return to the pool with no cleanup job involved.
// @Description
// @Description	Holding again replaces the previous hold rather than stacking on it.
// @Tags			bookings
// @Accept			json
// @Produce		json
// @Security		BearerAuth
// @Param			id		path		int						true	"event id"
// @Param			body	body		handlers.BookRequest	true	"seats to hold"
// @Success		201		{object}	holds.Hold
// @Failure		401		{object}	handlers.ErrorResponse
// @Failure		404		{object}	handlers.ErrorResponse	"no such event"
// @Failure		409		{object}	handlers.ErrorResponse	"not enough seats available"
// @Failure		422		{object}	handlers.ErrorResponse	"seats <= 0"
// @Router			/events/{id}/hold [post]
func (h *EventHandler) createHold(c echo.Context) error {
	id, err := intParam(c, "id")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "id must be a number")
	}
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	var body BookRequest
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if body.Seats <= 0 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "seats must be > 0")
	}

	// CAPACITY COMES FROM POSTGRES. Redis has no idea what an event is — the
	// application joins the two, exactly as in cache-aside. This read is also
	// what makes a 404 possible for an event that doesn't exist.
	event, err := h.Store.Get(id)
	if err != nil {
		return err // -> 404
	}

	hold, err := h.Holds.Acquire(c.Request().Context(), id, userID, body.Seats, event.Seats)
	if err != nil {
		return err // ErrInsufficientSeats -> 409, mapped centrally
	}
	return c.JSON(http.StatusCreated, hold)
}

// @Summary		Release a seat hold
// @Description	Requires authentication. Frees a hold early (user cancelled checkout).
// @Description	Releasing a hold that already expired is a successful no-op.
// @Tags			bookings
// @Produce		json
// @Security		BearerAuth
// @Param			id	path	int	true	"event id"
// @Success		204	"released"
// @Failure		401	{object}	handlers.ErrorResponse
// @Router			/events/{id}/hold [delete]
func (h *EventHandler) releaseHold(c echo.Context) error {
	id, err := intParam(c, "id")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "id must be a number")
	}
	userID, ok := UserIDFrom(c)
	if !ok {
		return models.ErrUnauthorized
	}
	if err := h.Holds.Release(c.Request().Context(), id, userID); err != nil {
		return err
	}
	// 204 No Content: it worked, and there's nothing meaningful to return.
	return c.NoContent(http.StatusNoContent)
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

//	@Summary		Readiness check
//	@Description	Reports whether this instance can SERVE TRAFFIC — i.e. its dependencies
//	@Description	are reachable. Returns 503 when the database is unavailable.
//	@Tags			meta
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Failure		503	{object}	handlers.ErrorResponse
//	@Router			/ready [get]
//
// ============== LIVENESS vs READINESS (the interview distinction) ==========
//
//	/health  LIVENESS  — "is this process alive?"  Failing it gets the pod
//	                     KILLED AND RESTARTED. So it must check almost nothing:
//	                     if it checked the database, a brief DB outage would
//	                     restart every pod in the fleet, which fixes nothing and
//	                     turns a small problem into an outage.
//
//	/ready   READINESS — "can this pod serve traffic RIGHT NOW?" Failing it
//	                     removes the pod from the Service's endpoints — no
//	                     restart, just no traffic. It recovers by itself when
//	                     the dependency returns.
//
// The rule: LIVENESS should check only the process itself; READINESS checks
// the dependencies you need to answer a request. Getting this backwards —
// checking the DB in liveness — is one of the most common Kubernetes mistakes,
// and it converts a degraded dependency into a crash-loop.
func (h *EventHandler) ready(c echo.Context) error {
	// A real dependency check: the cheapest query that proves the database is
	// answering. If this fails we are up but useless, which is exactly the
	// state readiness exists to describe.
	if _, err := h.Store.List(1, 0); err != nil {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "storage unavailable"})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
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

// @Summary		Get one event
// @Tags			events
// @Produce		json
// @Param			id	path		int	true	"event id"
// @Success		200	{object}	models.Event
// @Failure		400	{object}	handlers.ErrorResponse	"id is not a number"
// @Failure		404	{object}	handlers.ErrorResponse	"no such event"
// @Router			/events/{id} [get]
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

// @Summary		Create an event
// @Description	Requires authentication. `id` in the body is ignored — the database assigns it.
// @Tags			events
// @Accept			json
// @Produce		json
// @Security		BearerAuth
// @Param			event	body		models.Event	true	"name and seats"
// @Success		201		{object}	models.Event
// @Failure		400		{object}	handlers.ErrorResponse	"malformed JSON"
// @Failure		401		{object}	handlers.ErrorResponse	"missing or invalid token"
// @Failure		422		{object}	handlers.ErrorResponse	"name empty or seats <= 0"
// @Router			/events [post]
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

// @Summary		Book seats (auto-waitlists if full)
// @Description	Requires authentication. The booking is attributed to the token's user.
// @Description
// @Description	**Idempotent**: repeating the same request returns the original booking
// @Description	with `outcome: already_booked` and does not consume more seats.
// @Description
// @Description	Status codes carry the outcome:
// @Description	- **201** `booked` — a new booking exists
// @Description	- **202** `waitlisted` — event was full, you were queued
// @Description	- **200** `already_booked` / `already_waitlisted` — replay, nothing changed
// @Tags			bookings
// @Accept			json
// @Produce		json
// @Security		BearerAuth
// @Param			id		path		int						true	"event id"
// @Param			body	body		handlers.BookRequest	true	"seats to book"
// @Success		201		{object}	models.BookingResult	"booked"
// @Success		202		{object}	models.BookingResult	"waitlisted"
// @Success		200		{object}	models.BookingResult	"idempotent replay"
// @Failure		401		{object}	handlers.ErrorResponse
// @Failure		404		{object}	handlers.ErrorResponse	"no such event"
// @Failure		422		{object}	handlers.ErrorResponse	"seats <= 0"
// @Router			/events/{id}/book [post]
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

	// The hold has done its job: the seats are now really gone from Postgres,
	// so leaving the hold in place would double-count them and make the event
	// look emptier than it is. Best-effort — if this fails the hold simply
	// expires on its own in a few minutes, which is the whole point of a TTL.
	if h.Holds != nil && res.Outcome == models.OutcomeBooked {
		if rErr := h.Holds.Release(c.Request().Context(), id, userID); rErr != nil {
			c.Logger().Warnf("hold release failed (will expire via TTL): %v", rErr)
		}
	}

	// Tell the user what happened. Fire-and-forget — see notifyBooking.
	if ev, gErr := h.Store.Get(id); gErr == nil {
		h.notifyBooking(userID, ev, res)
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
//
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

	// THE NOTIFICATION THAT ACTUALLY MATTERS.
	//
	// Everyone promoted off the waitlist by this cancellation just got a seat —
	// and they are NOT looking at the page. Without a notification they'd never
	// find out. This is the concrete reason lesson 12's waitlist needed a
	// notification service at all, and why the two lessons belong together.
	if h.Notifier != nil && h.Users != nil && len(res.Promoted) > 0 {
		if ev, gErr := h.Store.Get(id); gErr == nil {
			for _, b := range res.Promoted {
				user, uErr := h.Users.GetUserByID(b.UserID)
				if uErr != nil {
					continue
				}
				h.Notifier.Send(notify.Notification{
					UserID:    b.UserID,
					Email:     user.Email,
					Type:      notify.TypePromotedFromWaitlist,
					EventID:   ev.ID,
					EventName: ev.Name,
					Seats:     b.Seats,
					IdempotencyKey: notify.Key(b.UserID, ev.ID,
						notify.TypePromotedFromWaitlist),
				})
			}
		}
	}

	return c.JSON(http.StatusOK, res)
}

// myWaitlist shows what the user is queued for, and their position in each line.
//
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
//
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
//
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

	// A hold that can't fit is a CONFLICT with current state — the same
	// reasoning as a sold-out event, so the same 409.
	if errors.Is(err, holds.ErrInsufficientSeats) {
		_ = c.JSON(http.StatusConflict, ErrorResponse{Error: err.Error()})
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
