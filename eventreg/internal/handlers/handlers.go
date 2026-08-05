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
func (h *EventHandler) Register(e *echo.Echo) {
	e.HTTPErrorHandler = errorHandler // central mapping, defined below

	e.GET("/health", h.health)
	e.GET("/events", h.listEvents)
	e.GET("/events/:id", h.getEvent)
	e.POST("/events", h.createEvent)
	e.POST("/events/:id/book", h.bookEvent)
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
	var body struct {
		Seats int `json:"seats"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	booked, err := h.Store.Book(id, body.Seats)
	if err != nil {
		return err // 409 or 404, decided centrally
	}
	return c.JSON(http.StatusOK, booked)
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
