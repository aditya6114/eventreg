// Package logging sets up structured logging and the HTTP request logger.
//
// WHY NOT fmt.Println / log.Printf (a checklist interview item):
//
// Unstructured:
//	log.Printf("booking failed for user %d on event %d: %v", uid, eid, err)
//	=> "2026/08/06 14:23:01 booking failed for user 7 on event 3: sold out"
//
// Structured:
//	logger.Error("booking failed", "user_id", 7, "event_id", 3, "error", "sold out")
//	=> {"time":"...","level":"ERROR","msg":"booking failed","user_id":7,"event_id":3,"error":"sold out"}
//
// The difference only matters at scale — but it matters enormously there:
//
//  1. QUERYABLE. With JSON you ask your log system `user_id=7 AND level=ERROR`.
//     With free text you write a regex, and it breaks the day someone reworders
//     the message.
//  2. NO PARSING AMBIGUITY. An event named "user 7: failed" can't corrupt a
//     JSON field the way it corrupts a text line.
//  3. LEVELS ARE REAL. Debug logs can be emitted in dev and filtered in prod
//     by configuration, with no code change.
//  4. CONTEXT TRAVELS. logger.With("request_id", id) attaches a field to every
//     subsequent line automatically — no threading it through call sites.
//
// log/slog is the STANDARD LIBRARY answer (Go 1.21+). Before it, everyone
// reached for zap or logrus; now the stdlib covers the common case, and using
// it means no third-party logging dependency.
package logging

import (
	"log/slog"
	"os"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// New builds the application logger.
//
// TEXT in development (a human is reading a terminal), JSON in production
// (a log aggregator is parsing fields). Same code, different handler — this is
// exactly the kind of thing config should decide, not the code.
func New(level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(h)

	// SetDefault redirects the OLD log package (log.Printf, log.Fatal) through
	// slog too. That matters because our dependencies — pgx, Echo, migrate —
	// may still use the standard logger; without this their output would bypass
	// our formatting and levels entirely, leaving a mix of JSON and plain text
	// that no parser can handle.
	slog.SetDefault(logger)

	// STDOUT, NOT A FILE — deliberate, and another 12-factor rule. A container
	// shouldn't manage log files, rotation, or disk space. It writes to stdout;
	// Docker/Kubernetes capture the stream and route it wherever it belongs.
	return logger
}

// RequestLogger replaces echo's middleware.Logger() with one that emits
// structured slog lines.
//
// (middleware.Logger is deprecated in current Echo precisely because it writes
// a fixed text format. RequestLoggerWithConfig hands you the VALUES and lets
// you decide how to record them — which is what we want.)
func RequestLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		// Opt in to the fields we want. Each one is off by default so the
		// middleware does no work you didn't ask for.
		LogStatus:   true,
		LogMethod:   true,
		LogURI:      true,
		LogLatency:  true,
		LogError:    true,
		LogRemoteIP: true,

		// HandleError lets Echo's central error handler run FIRST, so the
		// status we log is the one actually sent (404/409/...), not a
		// misleading 200.
		HandleError: true,

		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			attrs := []any{
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				// Round: nanosecond precision is noise in a log line.
				"latency_ms", v.Latency.Round(time.Millisecond).Milliseconds(),
				"remote_ip", v.RemoteIP,
			}
			if v.Error != nil {
				attrs = append(attrs, "error", v.Error.Error())
			}

			// LEVEL BY OUTCOME — so alerting can key on level alone:
			//   5xx -> ERROR: our fault, someone should look
			//   4xx -> WARN:  the client's fault; interesting in aggregate
			//   2xx -> INFO:  normal traffic
			switch {
			case v.Status >= 500:
				logger.Error("request", attrs...)
			case v.Status >= 400:
				logger.Warn("request", attrs...)
			default:
				logger.Info("request", attrs...)
			}
			return nil
		},
	})
}
