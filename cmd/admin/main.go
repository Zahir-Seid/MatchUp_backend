// Command admin serves the dashboard API.
//
// It runs as its own process on its own port with its own signing keys, so an
// admin endpoint does not merely fail a role check on the public API -- the route
// does not exist in that process at all (SDD 10.2). The same isolation is why
// this binary mounts no signup and no Google sign-in: an admin account is made
// by promoting an existing player, which is an audited action taken from inside.
//
// This is also the only browser-facing surface in the system, which is what the
// CORS allowlist is for. It is exact, never a wildcard: a credentialed admin
// response must not be readable by a site the operator has not named.
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

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/config"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/openapi"
)

// Sign-in limits, keyed by IP. Tighter than the public API's: the population
// that legitimately signs in here is a handful of staff, so a burst of ten
// attempts a minute from one address is already generous.
const (
	loginRPS, loginBurst = 0.2, 10
)

// MatchUp Admin API is the dashboard API staff use to run the platform.
//
//	@title						MatchUp Admin API
//	@version					1.0.0
//	@description				Dashboard operations for staff: the payment review queue, bookings, fields and games, users, platform settings, reports and the audit log.
//	@description				Every operation requires an admin access token; a player's token is refused with 403, and tokens minted for the public API are not accepted here.
//	@BasePath					/admin/v1
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Admin access token from /auth/login, sent as `Bearer <token>`.
func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "admin: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(config.NeedAdmin)
	if err != nil {
		return err
	}
	log := cfg.NewLogger("admin")
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

	recorder := audit.NewRecorder(store, log)
	// Admin actions tell the affected customer, so this binary needs the
	// notification service even though it never delivers anything itself.
	notifySvc := notification.NewService(store, log, cfg.ResendAPIKey != "")

	// The admin audience and the admin secrets: a token minted here is not
	// accepted by the public API, and a player's app token is not accepted
	// here, independently of any role check.
	tokens := auth.NewTokenIssuer(
		cfg.AdminJWTAccessSecret, cfg.AdminJWTRefreshSecret,
		auth.AudienceAdmin, cfg.AccessTokenTTL, cfg.RefreshTokenTTL,
	)
	authSvc := auth.NewService(auth.Options{
		Store:  store,
		Tokens: tokens,
		// No Google verifier: the dashboard has no Google sign-in, and a nil
		// verifier makes that a 501 rather than a route to be discovered.
		Logger:   log,
		ResetTTL: cfg.PasswordResetTTL,
		// Correct credentials belonging to a player are refused here, so a
		// missed role check downstream cannot be the only thing standing
		// between a player and an admin-audience token.
		RequireAdminRole: true,
		OnLogin: func(ctx context.Context, user db.User) {
			recorder.RecordDetached(ctx, audit.Entry{
				ActorID:    &user.ID,
				Action:     audit.ActionAdminLogin,
				TargetType: audit.TargetUser,
				TargetID:   user.ID.String(),
				Metadata:   map[string]any{"email": user.Email},
			})
		},
	})

	adminSvc := admin.NewService(admin.Options{
		Store:        store,
		Audit:        recorder,
		Notifier:     notifySvc,
		Logger:       log,
		MonthlyWeeks: cfg.MonthlyWeekCount,
	})

	globalLimiter := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	defer globalLimiter.Close()
	loginLimiter := httpx.NewRateLimiter(loginRPS, loginBurst)
	defer loginLimiter.Close()

	authH := auth.NewHandler(authSvc, loginLimiter.Middleware)
	adminH := admin.NewHandler(adminSvc)

	r := newRouter(routerDeps{
		log:           log,
		cfg:           cfg,
		pinger:        store,
		verifier:      authSvc,
		authH:         authH,
		adminH:        adminH,
		globalLimiter: globalLimiter,
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.AdminPort),
		Handler:           r,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
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

	authH  *auth.Handler
	adminH *admin.Handler

	globalLimiter *httpx.RateLimiter
}

// newRouter builds the dashboard API's route table.
func newRouter(d routerDeps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Logging(d.log))
	r.Use(httpx.Recover)
	r.Use(httpx.SecurityHeaders)
	// Ahead of everything that can reject a request: a preflight carries no
	// credentials, so a 401 on OPTIONS would make the real request unsendable
	// and the failure would read as a server error in the browser console.
	r.Use(httpx.CORS(d.cfg.AdminCORSOrigins))

	r.Method(http.MethodGet, "/healthz", httpx.LivenessHandler())
	r.Method(http.MethodGet, "/readyz", httpx.ReadinessHandler(d.pinger, 2*time.Second))

	// The generated contract: the OpenAPI 3 document the handlers annotate
	// themselves with (make docs), served as JSON and as the Swagger UI that
	// renders it. Mounted ahead of the rate-limited group: they answer with
	// static bytes, and a limiter would only get in a caller's way.
	r.Method(http.MethodGet, "/openapi.json", openapi.JSON(openapi.AdminSpec()))
	// The UI fetches ./swagger_spec relative to the page, so the bare path has
	// to land on the slash-terminated one.
	r.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
	})
	r.Handle("/docs/*", http.StripPrefix("/docs", openapi.UI(openapi.AdminSpec())))

	r.Group(func(r chi.Router) {
		r.Use(d.globalLimiter.Middleware)
		r.Use(httpx.Timeout(handlerTimeout(d.cfg)))

		r.Route("/admin/v1", func(r chi.Router) {
			d.authH.AdminRoutes(r)

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireAuth(d.verifier))
				// Self-service needs a signed-in caller, not an admin one. It
				// is the same set of endpoints an ex-admin still holding a
				// valid token should be able to use to sign themselves out.
				d.authH.AdminAuthedRoutes(r)

				r.Group(func(r chi.Router) {
					// One role check for the whole dashboard surface. A route
					// added inside this group inherits it; a route added
					// outside is visibly outside it.
					r.Use(httpx.RequireRole(auth.RoleAdmin))
					d.adminH.Routes(r)
				})
			})
		})
	})

	return r
}

// handlerTimeout keeps handler cancellation just inside the server's write
// deadline, so a slow report returns an error envelope rather than a truncated
// response body.
func handlerTimeout(cfg *config.Config) time.Duration {
	if cfg.WriteTimeout > 2*time.Second {
		return cfg.WriteTimeout - time.Second
	}
	return cfg.WriteTimeout
}
