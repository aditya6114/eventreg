// Package notify is the API's client for the notification service.
//
// ==================== WHY gRPC FOR SERVICE-TO-SERVICE ======================
//
//  1. TYPED CONTRACTS. The .proto generates both the client and the server, so a
//     signature mismatch is a COMPILE error rather than a runtime 500. With REST
//     between services, the contract is a document someone maintains by hand and
//     the compiler cannot check.
//
//  2. HTTP/2. One TCP connection carries many concurrent requests
//     (multiplexing), so there's no head-of-line blocking and no connection
//     churn. Headers are compressed (HPACK) instead of resent in full. Streaming
//     is native rather than bolted on with SSE or WebSockets.
//
//  3. BINARY PROTOBUF. Field NAMES never go on the wire — only numbers — so
//     payloads are typically 3-10x smaller than the equivalent JSON, and
//     encode/decode is faster because there's no text parsing.
//
// ============================ gRPC vs REST =================================
//
//	                    REST/JSON                gRPC
//	Audience            public, browsers         internal services
//	Contract            OpenAPI (hand-written)   .proto (generates the code)
//	Payload             text, verbose            binary, compact
//	Browser support     native                   needs grpc-web + a proxy
//	Debugging           curl                     grpcurl / needs tooling
//	Streaming           awkward (SSE/WS)         first-class
//
// THE RULE: REST at the EDGE where clients are browsers, third parties, and
// curl; gRPC BETWEEN services you control, where performance and type safety
// beat human-readability.
//
// This project is exactly that shape: an Echo REST API facing the world, gRPC
// behind it — which is why the two coexist rather than one replacing the other.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	notificationv1 "eventreg/gen/notification/v1"
)

// Client wraps the generated gRPC client with this project's failure policy.
type Client struct {
	conn    *grpc.ClientConn
	rpc     notificationv1.NotificationServiceClient
	log     *slog.Logger
	timeout time.Duration
}

// New dials the notification service.
//
// grpc.NewClient does NOT connect eagerly — it sets up a "channel" that
// connects lazily and RECONNECTS automatically with exponential backoff. So a
// notifier that is down at API startup, or restarts later, needs no special
// handling: the next call simply succeeds once it's back. That's very different
// from an http.Client, and it's why there's no retry loop here.
func New(target string, timeout time.Duration, log *slog.Logger) (*Client, error) {
	conn, err := grpc.NewClient(target,
		// INSECURE = plaintext, no TLS. Acceptable ONLY because this traffic
		// never leaves the private container network — the same trade-off as
		// terminating TLS at nginx (lesson 19). Across a public network or
		// under zero-trust you'd use mTLS here.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn:    conn,
		rpc:     notificationv1.NewNotificationServiceClient(conn),
		log:     log,
		timeout: timeout,
	}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Notification is the caller-facing payload — deliberately NOT the generated
// protobuf type.
//
// Keeping the proto types out of handlers means the HTTP layer never imports
// generated code, and swapping gRPC for a queue later touches only this file.
// It's the same reasoning as the Store interface: depend on your own types,
// not on someone else's transport.
type Notification struct {
	UserID         int
	Email          string
	Type           Type
	EventID        int
	EventName      string
	Seats          int
	IdempotencyKey string
}

type Type int

const (
	TypeBookingConfirmed Type = iota
	TypeWaitlisted
	TypePromotedFromWaitlist
)

func (t Type) proto() notificationv1.NotificationType {
	switch t {
	case TypeBookingConfirmed:
		return notificationv1.NotificationType_NOTIFICATION_TYPE_BOOKING_CONFIRMED
	case TypeWaitlisted:
		return notificationv1.NotificationType_NOTIFICATION_TYPE_WAITLISTED
	case TypePromotedFromWaitlist:
		return notificationv1.NotificationType_NOTIFICATION_TYPE_PROMOTED_FROM_WAITLIST
	}
	return notificationv1.NotificationType_NOTIFICATION_TYPE_UNSPECIFIED
}

// ==================== GRACEFUL DEGRADATION (interview) =====================
//
// THE QUESTION: "what happens when the notification service is down?"
//
// THE ANSWER THIS CODE GIVES: the booking still succeeds. A notification is a
// SIDE EFFECT of booking, not part of it. Failing a user's seat reservation
// because an email couldn't be sent would be absurd — and during a launch
// stampede, when the notifier is most likely to be struggling, it would take
// down the one thing that must not fail.
//
// This is where a naive design fails badly: if the API called the notifier
// SYNCHRONOUSLY and waited, then a slow notifier makes every booking slow, and
// a dead notifier makes every booking fail. One non-critical service takes down
// the critical path. Adding a timeout limits the damage but doesn't remove it.
//
// So Send is FIRE-AND-FORGET: it returns immediately and does the RPC in the
// background.
func (c *Client) Send(n Notification) {
	// ================== THE CONTEXT LIFETIME TRAP ======================
	//
	// context.Background(), NOT the incoming request's context.
	//
	// This is a bug people hit constantly. The request context is CANCELLED the
	// moment the HTTP response is written — so background work started with it
	// dies instantly and silently. You'd see notifications "randomly" not
	// arriving, with no error anywhere, because cancellation isn't a failure.
	//
	// Background work needs its OWN lifetime. It still gets a TIMEOUT, so it
	// can't hang forever, but that timeout is independent of the request.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()

		// The deadline travels ACROSS THE NETWORK. gRPC serializes it into the
		// grpc-timeout header, and the server sees it on its own ctx. That's
		// the thing plain HTTP has no standard mechanism for, and the reason
		// context finally earns its keep here (lesson 14 was cancellation; this
		// is propagation).
		//
		// Without a deadline, a hung notifier leaks a goroutine per booking —
		// which under load is a slow, invisible memory leak.
		resp, err := c.rpc.SendNotification(ctx, &notificationv1.SendNotificationRequest{
			UserId:         int64(n.UserID),
			Email:          n.Email,
			Type:           n.Type.proto(),
			EventId:        int64(n.EventID),
			EventName:      n.EventName,
			Seats:          int32(n.Seats),
			IdempotencyKey: n.IdempotencyKey,
		})
		if err != nil {
			// LOG, DON'T PROPAGATE. There is nobody to return an error to — the
			// user's response was sent long ago. Logging at WARN (not ERROR)
			// because a missed notification is degraded service, not a broken
			// booking, and alerting should reflect that difference.
			//
			// status.Code extracts the gRPC code so the log distinguishes
			// "notifier is down" from "we sent a malformed request", which need
			// completely different responses from an operator.
			code := status.Code(err)
			c.log.Warn("notification failed (booking unaffected)",
				"code", code.String(),
				"user_id", n.UserID,
				"event_id", n.EventID,
				"error", err,
			)
			// A production system would enqueue for retry here (an outbox
			// table, or a real queue) so the notification isn't lost — which is
			// precisely the Kafka work the checklist deliberately excludes.
			// Worth naming as the known gap rather than pretending it's solved.
			_ = codes.Unavailable
			return
		}
		c.log.Debug("notification queued",
			"id", resp.GetNotificationId(),
			"deduplicated", resp.GetDeduplicated())
	}()
}

// Healthy reports whether the notification service is reachable, for the API's
// own health endpoint. Synchronous with a short deadline — the caller IS
// waiting for this one.
func (c *Client) Healthy(ctx context.Context) (bool, int64) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := c.rpc.Health(ctx, &notificationv1.HealthRequest{})
	if err != nil {
		return false, 0
	}
	return resp.GetHealthy(), resp.GetSentCount()
}

// Key builds an idempotency key that's stable for a given (user, event, type),
// so a retried RPC can't produce a duplicate notification.
func Key(userID, eventID int, t Type) string {
	return fmt.Sprintf("u%d-e%d-t%d", userID, eventID, int(t))
}
