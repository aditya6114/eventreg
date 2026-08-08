// Command notifier is the notification microservice.
//
// It is a SEPARATE PROCESS from the API, reachable only over gRPC. That's the
// point: this is the project's first genuine service-to-service boundary, and
// everything interesting about it comes from the boundary being real.
//
// In production this would push email/SMS/web-push. Here it logs and counts,
// because the delivery mechanism isn't what the lesson is about — the CONTRACT,
// the DEADLINES, and the FAILURE BEHAVIOUR are.
//
//	go run ./cmd/notifier          (listens on :50051)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	notificationv1 "eventreg/gen/notification/v1"
	"eventreg/internal/config"
	"eventreg/internal/logging"
)

// server implements the generated NotificationServiceServer interface.
//
// UnimplementedNotificationServiceServer must be embedded — and that embedding
// is a deliberate FORWARD-COMPATIBILITY mechanism, not boilerplate. When
// someone adds a new rpc to the .proto, this struct automatically inherits a
// stub returning codes.Unimplemented instead of failing to compile. Old servers
// keep building and running against new contracts; they just don't answer the
// new call. That's what lets a large system roll out a proto change gradually.
type server struct {
	notificationv1.UnimplementedNotificationServiceServer

	log *slog.Logger

	// Deduplication by idempotency key. In-memory here (see the note in the
	// proto): a real service would use Redis so it survives restarts and works
	// across replicas — exactly the reasoning from lesson 18.
	mu    sync.Mutex
	seen  map[string]string // idempotency key -> notification id
	count int64
}

func (s *server) SendNotification(
	ctx context.Context,
	req *notificationv1.SendNotificationRequest,
) (*notificationv1.SendNotificationResponse, error) {
	// ============== VALIDATION AND gRPC STATUS CODES =====================
	//
	// gRPC has its OWN status codes, not HTTP's. The mapping is intuitive:
	//
	//	 OK                  ~ 200      InvalidArgument    ~ 400
	//	 NotFound            ~ 404      PermissionDenied   ~ 403
	//	 Unauthenticated     ~ 401      AlreadyExists      ~ 409
	//	 ResourceExhausted   ~ 429      Internal           ~ 500
	//	 Unavailable         ~ 503      DeadlineExceeded   ~ 504
	//
	// The two that behave specially in every gRPC library:
	//	 Unavailable      -> transient; clients RETRY this automatically
	//	 DeadlineExceeded -> the caller's deadline passed (see the client)
	//
	// That retry behaviour is why choosing the right code matters: return
	// Unavailable for something permanent and clients will hammer you.
	if req.GetEmail() == "" {
		// InvalidArgument = the caller's fault, retrying won't help.
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if req.GetType() == notificationv1.NotificationType_NOTIFICATION_TYPE_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "notification type is required")
	}

	// RESPECT THE CALLER'S DEADLINE. The client attached one, and gRPC
	// propagated it across the network into this ctx automatically. Checking it
	// before doing work means we don't spend effort on a response nobody is
	// waiting for any more.
	if err := ctx.Err(); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded before processing")
	}

	s.mu.Lock()
	if key := req.GetIdempotencyKey(); key != "" {
		if existing, ok := s.seen[key]; ok {
			s.mu.Unlock()
			// A RETRY, not an error. gRPC retries transient failures
			// automatically, so a duplicate is expected traffic — treat it as
			// success and say so, rather than sending a second email.
			s.log.Info("notification deduplicated", "key", key, "notification_id", existing)
			return &notificationv1.SendNotificationResponse{
				NotificationId: existing,
				Delivered:      true,
				Deduplicated:   true,
			}, nil
		}
	}
	s.count++
	id := fmt.Sprintf("ntf-%d", s.count)
	if key := req.GetIdempotencyKey(); key != "" {
		s.seen[key] = id
	}
	s.mu.Unlock()

	// Where a real implementation would call SES/Twilio/APNs.
	s.log.Info("notification sent",
		"id", id,
		"to", req.GetEmail(),
		"user_id", req.GetUserId(),
		"type", req.GetType().String(),
		"event", req.GetEventName(),
		"seats", req.GetSeats(),
	)

	return &notificationv1.SendNotificationResponse{
		NotificationId: id,
		Delivered:      true,
	}, nil
}

func (s *server) Health(
	ctx context.Context,
	_ *notificationv1.HealthRequest,
) (*notificationv1.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &notificationv1.HealthResponse{Healthy: true, SentCount: s.count}, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat)

	addr := ":" + getEnv("GRPC_PORT", "50051")
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	srv := &server{log: logger, seen: map[string]string{}}

	grpcServer := grpc.NewServer(
		// Server-side keepalive/timeout knobs would go here in production.
		grpc.ConnectionTimeout(10 * time.Second),
	)
	notificationv1.RegisterNotificationServiceServer(grpcServer, srv)

	// REFLECTION lets tools like grpcurl discover the service's methods at
	// runtime without holding a copy of the .proto — the closest gRPC gets to
	// "just curl it". Genuinely useful in development; usually disabled in
	// production because it advertises your entire API surface (same reasoning
	// as the dev-only Swagger UI in lesson 15).
	if !cfg.IsProduction() {
		reflection.Register(grpcServer)
		logger.Info("grpc reflection enabled (development only)")
	}

	// Graceful shutdown, same shape as the HTTP server in lesson 14.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("notification service listening", "addr", addr)
		serveErr <- grpcServer.Serve(lis)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}
	stop()

	// GracefulStop stops accepting new RPCs and waits for in-flight ones —
	// the gRPC equivalent of http.Server.Shutdown.
	done := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(done) }()
	select {
	case <-done:
		logger.Info("all in-flight RPCs completed")
	case <-time.After(cfg.ShutdownTimeout):
		// Same reasoning as lesson 14: a deadline, or one stuck RPC blocks
		// shutdown until the orchestrator SIGKILLs us anyway.
		logger.Warn("graceful stop timed out; forcing")
		grpcServer.Stop()
	}
	logger.Info("shutdown complete")
	return nil
}

func getEnv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
