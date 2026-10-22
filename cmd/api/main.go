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
	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/openapi"
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

// MatchUp API is the player-facing HTTP API the mobile app talks to.
//
//	@title						MatchUp API
//	@version					1.0.0
//	@description				Game discovery, spot booking, payment submission and the in-app notification feed.
//	@description				Amounts are computed and verified server-side; the client never supplies the amount that is checked.
//	@BasePath					/v1
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Access token from /auth/login (or /auth/refresh), sent as `Bearer <token>`.
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

	// Local runs read .env; production sets the environment directly.
	config.LoadDotEnv()
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
		// Signup verification links outlive reset links: players may not open
		// the mail the same day.
		EmailVerifyTTL: cfg.EmailVerifyTTL,
	})
	mediaSvc := media.NewService(media.Options{
		Store:         store,
		PublicBaseURL: cfg.PublicAPIBaseURL,
		Logger:        log,
	})
	catalogSvc := catalog.NewService(store, mediaSvc)
	bookingSvc := booking.NewService(booking.Options{
		Store:        store,
		Logger:       log,
		Media:        mediaSvc,
		HoldDuration: cfg.HoldDuration,
		WeekCount:    cfg.MonthlyWeekCount,
	})
	paymentSvc := payment.NewService(payment.Options{
		Store:  store,
		Logger: log,
		// Stamped onto every verification job this binary queues; the worker
		// spends the budget.
		MaxAttempts: cfg.VerifyMaxAttempts,
		// Payment instructions show these to the payer; they are the same
		// values the worker checks receipts against (SDD 8.4).
		FallbackAccounts: payment.FallbackAccounts{
			CBE:      cfg.MerchantCBEAccount,
			Telebirr: cfg.MerchantTelebirrAccount,
		},
		MerchantName: cfg.AppName,
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

	r := newRouter(routerDeps{
		log:           log,
		cfg:           cfg,
		pinger:        store,
		verifier:      authSvc,
		authH:         authH,
		catalogH:      catalogH,
		bookingH:      bookingH,
		paymentH:      paymentH,
		notifyH:       notifyH,
		globalLimiter: globalLimiter,
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

// routerDeps is everything newRouter needs. It is a struct rather than a
// parameter list because a second caller builds the router too: the
// route-coverage test walks the real route table, so the committed OpenAPI
// document is checked against what this binary serves rather than a copy of it.
//
// Services are absent except for the verifier: routing needs the handlers and
// nothing else, which is what keeps that test free of a database.
type routerDeps struct {
	log      *slog.Logger
	cfg      *config.Config
	pinger   httpx.Pinger
	verifier httpx.TokenVerifier

	authH    *auth.Handler
	catalogH *catalog.Handler
	bookingH *booking.Handler
	paymentH *payment.Handler
	notifyH  *notification.Handler

	globalLimiter *httpx.RateLimiter
}

// newRouter builds the public API's route table.
func newRouter(d routerDeps) http.Handler {
	r := chi.NewRouter()
	// Order matters. Logging attaches the request-scoped logger that Recover
	// reads, and it emits its line after the inner handler returns -- which,
	// with Recover inside it, includes the request that panicked.
	r.Use(httpx.RequestID)
	r.Use(httpx.Logging(d.log))
	r.Use(httpx.Recover)
	r.Use(httpx.SecurityHeaders)

	// Probes sit outside the rate limiter: an orchestrator polls them every few
	// seconds from a single address, which is the exact shape the limiter
	// exists to reject.
	r.Method(http.MethodGet, "/healthz", httpx.LivenessHandler())
	r.Method(http.MethodGet, "/readyz", httpx.ReadinessHandler(d.pinger, 2*time.Second))

	// The generated contract: the OpenAPI 3 document the handlers annotate
	// themselves with (make docs), served as JSON and as the Swagger UI that
	// renders it. Mounted ahead of the rate-limited group: they answer with
	// static bytes, and a limiter would only get in a caller's way.
	r.Method(http.MethodGet, "/openapi.json", openapi.JSON(openapi.APISpec()))
	// The UI fetches ./swagger_spec relative to the page, so the bare path has
	// to land on the slash-terminated one.
	r.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
	})
	r.Handle("/docs/*", http.StripPrefix("/docs", openapi.UI(openapi.APISpec())))

	r.Group(func(r chi.Router) {
		r.Use(d.globalLimiter.Middleware)
		r.Use(httpx.Timeout(handlerTimeout(d.cfg)))

		r.Route("/v1", func(r chi.Router) {
			d.authH.Routes(r)
			d.catalogH.Routes(r)

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireAuth(d.verifier))
				d.authH.AuthedRoutes(r)
				d.bookingH.Routes(r)
				d.paymentH.Routes(r)
				d.notifyH.Routes(r)
			})
		})
	})

	return r
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
