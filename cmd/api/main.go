// Command api serves the public API the Flutter app talks to.
//
// It is one of three binaries built from this module, and it deliberately does
// less than the system as a whole: nothing here calls a bank, drains a queue or
// exposes an admin endpoint. Verification and delivery belong to the worker, and
// the dashboard's surface is a separate process with its own signing keys
// (SDD 10.2), so a routing mistake in this file cannot reach either.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/config"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// Per-group rate limits, tighter than the global one because these endpoints
// cost more than the request that triggers them. They are fixed rather than
// configurable: the global limit is the knob an operator turns, and these three
// numbers are a property of what the endpoints do.
const (
	// Credential endpoints, keyed by IP because the caller is anonymous. PRD
	// FR5a leans on this in place of a mandatory email-verification gate, so an
	// attacker gets a handful of guesses a minute rather than thousands.
	authRPS, authBurst = 0.5, 10
	// Booking creation, keyed by account. A hold costs nothing to create and
	// parks real capacity for five minutes.
	holdRPS, holdBurst = 0.5, 5
	// Payment submission, keyed by account. Every attempt is an outbound bank
	// lookup and spends a transaction reference that can never be reused.
	paymentRPS, paymentBurst = 0.2, 5
)

func main() {
	if err := run(); err != nil {
		// Written to stderr rather than through slog: a configuration failure
		// happens before the logger exists, and stderr is what a container
		// runtime captures either way.
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// SIGTERM is what an orchestrator sends first, and honouring it is what
	// keeps a rolling deploy from dropping in-flight bookings.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(config.NeedServe)
	if err != nil {
		return err
	}
	log := cfg.NewLogger("api")
	// Packages that were built without an explicit logger fall back to the
	// default one, so it is replaced rather than left as the stdlib's.
	slog.SetDefault(log)

	store, err := db.Open(ctx, db.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLife,
		MaxConnIdleTime: cfg.DBMaxConnIdle,
	})
	if err != nil {
		return err
	}
	defer store.Close()

	tokens := auth.NewTokenIssuer(
		cfg.JWTAccessSecret, cfg.JWTRefreshSecret,
		auth.AudienceApp, cfg.AccessTokenTTL, cfg.RefreshTokenTTL,
	)
	authSvc := auth.NewService(auth.Options{
		Store:    store,
		Tokens:   tokens,
		Google:   auth.NewGoogleVerifier(cfg.GoogleOAuthClientID),
		Logger:   log,
		ResetTTL: cfg.PasswordResetTTL,
	})
	catalogSvc := catalog.NewService(store)
	bookingSvc := booking.NewService(booking.Options{
		Store:        store,
		Logger:       log,
		HoldDuration: cfg.HoldDuration,
		WeekCount:    cfg.MonthlyWeekCount,
	})
	paymentSvc := payment.NewService(payment.Options{
		Store:  store,
		Logger: log,
		// Stamped onto every verification job this binary queues; the worker
		// spends the budget.
		MaxAttempts: cfg.VerifyMaxAttempts,
	})
	// The email channel is gated on a configured sender: without one, queueing
	// email deliveries would only manufacture rows nothing can ever send.
	notifySvc := notification.NewService(store, log, cfg.ResendAPIKey != "")

	globalLimiter := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	defer globalLimiter.Close()
	authLimiter := httpx.NewRateLimiter(authRPS, authBurst)
	defer authLimiter.Close()
	holdLimiter := httpx.NewRateLimiter(holdRPS, holdBurst)
	defer holdLimiter.Close()
	paymentLimiter := httpx.NewRateLimiter(paymentRPS, paymentBurst)
	defer paymentLimiter.Close()

	authH := auth.NewHandler(authSvc, authLimiter.Middleware)
	catalogH := catalog.NewHandler(catalogSvc)
	// The hold and payment limiters key by account, so they are mounted inside
	// the authenticated group by their handlers.
	bookingH := booking.NewHandler(bookingSvc, holdLimiter.ByUser)
	paymentH := payment.NewHandler(paymentSvc, paymentLimiter.ByUser)
	notifyH := notification.NewHandler(notifySvc)

	r := chi.NewRouter()
	// Order matters. Logging attaches the request-scoped logger that Recover
	// reads, and it emits its line after the inner handler returns -- which,
	// with Recover inside it, includes the request that panicked.
	r.Use(httpx.RequestID)
	r.Use(httpx.Logging(log))
	r.Use(httpx.Recover)
	r.Use(httpx.SecurityHeaders)

	// Probes sit outside the rate limiter: an orchestrator polls them every few
	// seconds from a single address, which is the exact shape the limiter
	// exists to reject.
	r.Method(http.MethodGet, "/healthz", httpx.LivenessHandler())
	r.Method(http.MethodGet, "/readyz", httpx.ReadinessHandler(store, 2*time.Second))

	r.Group(func(r chi.Router) {
		r.Use(globalLimiter.Middleware)
		r.Use(httpx.Timeout(handlerTimeout(cfg)))

		r.Route("/v1", func(r chi.Router) {
			authH.Routes(r)
			catalogH.Routes(r)

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireAuth(authSvc))
				authH.AuthedRoutes(r)
				bookingH.Routes(r)
				paymentH.Routes(r)
				notifyH.Routes(r)
			})
		})
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: r,
		// ReadHeaderTimeout is set explicitly as well as ReadTimeout: it is the
		// one that bounds a client which opens a connection and then sends its
		// headers one byte at a time.
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       2 * time.Minute,
		// Routes the server's own errors (a bad TLS handshake, a malformed
		// request line) into the structured log instead of stderr.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return httpx.Serve(ctx, srv, log, cfg.ShutdownTimeout)
}

// handlerTimeout keeps handler cancellation just inside the server's write
// deadline. A handler that outruns WriteTimeout has its response truncated
// mid-body; cancelling a second earlier means the client gets a real error
// envelope instead of a dead connection.
func handlerTimeout(cfg *config.Config) time.Duration {
	if cfg.WriteTimeout > 2*time.Second {
		return cfg.WriteTimeout - time.Second
	}
	return cfg.WriteTimeout
}
