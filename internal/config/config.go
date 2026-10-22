// Package config loads and validates process configuration from the
// environment. Every binary (serve, admin, worker) calls Load at startup and
// fails fast: a missing secret is a boot failure, never a runtime surprise on
// the first request that needs it.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env identifies the deployment environment. Some safety checks are relaxed
// only in EnvLocal.
type Env string

const (
	EnvLocal      Env = "local"
	EnvStaging    Env = "staging"
	EnvProduction Env = "production"
)

// Log output formats.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the full configuration surface of the system. Each binary reads
// the subset it needs; Load validates the subset requested via the Need flags.
type Config struct {
	AppEnv Env

	// HTTP
	Port      int
	AdminPort int
	// PublicAPIBaseURL is the origin clients reach the public API at. Field
	// image URLs are absolute so a mobile client does not have to resolve them
	// against the admin API it fetched the field from.
	PublicAPIBaseURL string
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	ShutdownTimeout  time.Duration

	// Database
	DatabaseURL   string
	DBMaxConns    int32
	DBMinConns    int32
	DBMaxConnLife time.Duration
	DBMaxConnIdle time.Duration

	// Auth
	JWTAccessSecret       string
	JWTRefreshSecret      string
	AdminJWTAccessSecret  string
	AdminJWTRefreshSecret string
	// AdminCORSOrigins allowlists the dashboard's own origins. The admin API
	// is the only browser-facing surface here, and the list is exact: a
	// wildcard would let any site read a credentialed admin response.
	AdminCORSOrigins    []string
	AccessTokenTTL      time.Duration
	RefreshTokenTTL     time.Duration
	PasswordResetTTL    time.Duration
	EmailVerifyTTL      time.Duration
	GoogleOAuthClientID string

	// Rate limiting
	RateLimitRPS   float64
	RateLimitBurst int

	// Booking policy
	HoldDuration     time.Duration
	MonthlyWeekCount int

	// Payment verification
	MerchantCBEAccount       string
	MerchantTelebirrAccount  string
	CBETimeout               time.Duration
	TelebirrTimeout          time.Duration
	VerifyMaxAttempts        int
	AllowInsecureVerifierTLS bool

	// Notification delivery
	ResendAPIKey          string
	ResendFromAddress     string
	FCMServiceAccountJSON string
	// Email copy. The two URLs are the base links a single-use token is
	// appended to, normally app deep links; left empty, the mail carries the
	// code alone rather than a link that goes nowhere.
	AppName          string
	SupportEmail     string
	PasswordResetURL string
	EmailVerifyURL   string

	// Observability
	LogLevel string
	// LogFormat is "json" or "text". Left unset it resolves to text locally
	// and json everywhere else, because a person reads one and a log
	// aggregator reads the other.
	LogFormat string

	// Worker cadence
	VerifyPollInterval   time.Duration
	ExpirySweepInterval  time.Duration
	ReconcileInterval    time.Duration
	NotifyPollInterval   time.Duration
	ImageCleanupInterval time.Duration
	VerifyBatchSize      int
	NotifyBatchSize      int
	StuckJobThreshold    time.Duration
	ExpiryWarningWindow  time.Duration
}

// Need selects which configuration groups must be present and valid. A binary
// that never signs a token should not fail to boot over a missing signing key.
type Need struct {
	HTTP  bool
	DB    bool
	Auth  bool
	Admin bool
	// Accounts requires the merchant receiving accounts. Every binary needs
	// them: the worker checks receipts against them, and the public and admin
	// APIs show them to payers and operators.
	Accounts bool
	Notify   bool
}

// NeedServe, NeedAdmin and NeedWorker are the three service call sites. NeedCLI
// is the operator tool: it only touches accounts, so it requires nothing but a
// database.
var (
	NeedServe  = Need{HTTP: true, DB: true, Auth: true, Accounts: true}
	NeedAdmin  = Need{HTTP: true, DB: true, Admin: true, Accounts: true}
	NeedWorker = Need{DB: true, Accounts: true, Notify: true}
	NeedCLI    = Need{DB: true}
)

// Load reads configuration from the environment and validates the groups named
// by need. The returned error lists every problem found, not just the first,
// so a misconfigured deployment can be fixed in one pass.
func Load(need Need) (*Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	port := envInt("PORT", 8080, fail)
	c := &Config{
		AppEnv: Env(strings.ToLower(envDefault("APP_ENV", string(EnvLocal)))),

		Port:             port,
		AdminPort:        envInt("ADMIN_PORT", 8081, fail),
		PublicAPIBaseURL: envDefault("PUBLIC_API_BASE_URL", fmt.Sprintf("http://localhost:%d", port)),
		ReadTimeout:      envDuration("HTTP_READ_TIMEOUT", 15*time.Second, fail),
		WriteTimeout:     envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second, fail),
		ShutdownTimeout:  envDuration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second, fail),

		DatabaseURL:   os.Getenv("DATABASE_URL"),
		DBMaxConns:    int32(envInt("DB_MAX_CONNS", 10, fail)),
		DBMinConns:    int32(envInt("DB_MIN_CONNS", 2, fail)),
		DBMaxConnLife: envDuration("DB_MAX_CONN_LIFETIME", time.Hour, fail),
		DBMaxConnIdle: envDuration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute, fail),

		JWTAccessSecret:       os.Getenv("JWT_ACCESS_SECRET"),
		JWTRefreshSecret:      os.Getenv("JWT_REFRESH_SECRET"),
		AdminJWTAccessSecret:  os.Getenv("ADMIN_JWT_ACCESS_SECRET"),
		AdminJWTRefreshSecret: os.Getenv("ADMIN_JWT_REFRESH_SECRET"),
		AccessTokenTTL:        envDuration("ACCESS_TOKEN_TTL", 15*time.Minute, fail),
		RefreshTokenTTL:       envDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour, fail),
		PasswordResetTTL:      envDuration("PASSWORD_RESET_TTL", time.Hour, fail),
		EmailVerifyTTL:        envDuration("EMAIL_VERIFY_TTL", 24*time.Hour, fail),
		GoogleOAuthClientID:   os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		AdminCORSOrigins:      envList("ADMIN_CORS_ORIGINS"),

		RateLimitRPS:   envFloat("RATE_LIMIT_RPS", 10, fail),
		RateLimitBurst: envInt("RATE_LIMIT_BURST", 20, fail),

		// PRD FR12: five minutes, matched by the ~15-20s sweep cadence
		// in FR13.
		HoldDuration:     envDuration("BOOKING_HOLD_DURATION", 5*time.Minute, fail),
		MonthlyWeekCount: envInt("MONTHLY_WEEK_COUNT", 4, fail),

		MerchantCBEAccount:       os.Getenv("MERCHANT_CBE_ACCOUNT"),
		MerchantTelebirrAccount:  os.Getenv("MERCHANT_TELEBIRR_ACCOUNT"),
		CBETimeout:               envDuration("CBE_VERIFY_TIMEOUT", 20*time.Second, fail),
		TelebirrTimeout:          envDuration("TELEBIRR_VERIFY_TIMEOUT", 20*time.Second, fail),
		VerifyMaxAttempts:        envInt("VERIFY_MAX_ATTEMPTS", 5, fail),
		AllowInsecureVerifierTLS: envBool("ALLOW_INSECURE_VERIFIER_TLS", false, fail),

		ResendAPIKey:          os.Getenv("RESEND_API_KEY"),
		ResendFromAddress:     envDefault("RESEND_FROM_ADDRESS", "MatchUp <no-reply@matchup.et>"),
		FCMServiceAccountJSON: os.Getenv("FCM_SERVICE_ACCOUNT_JSON"),
		AppName:               envDefault("APP_NAME", "MatchUp"),
		SupportEmail:          os.Getenv("SUPPORT_EMAIL"),
		PasswordResetURL:      os.Getenv("PASSWORD_RESET_URL"),
		EmailVerifyURL:        os.Getenv("EMAIL_VERIFY_URL"),

		LogLevel:  strings.ToLower(envDefault("LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))),

		VerifyPollInterval:   envDuration("VERIFY_POLL_INTERVAL", 2*time.Second, fail),
		ExpirySweepInterval:  envDuration("EXPIRY_SWEEP_INTERVAL", 15*time.Second, fail),
		ReconcileInterval:    envDuration("RECONCILE_INTERVAL", 5*time.Minute, fail),
		NotifyPollInterval:   envDuration("NOTIFY_POLL_INTERVAL", 5*time.Second, fail),
		ImageCleanupInterval: envDuration("IMAGE_CLEANUP_INTERVAL", time.Hour, fail),
		VerifyBatchSize:      envInt("VERIFY_BATCH_SIZE", 10, fail),
		NotifyBatchSize:      envInt("NOTIFY_BATCH_SIZE", 25, fail),
		StuckJobThreshold:    envDuration("STUCK_JOB_THRESHOLD", 5*time.Minute, fail),
		ExpiryWarningWindow:  envDuration("EXPIRY_WARNING_WINDOW", 2*time.Minute, fail),
	}

	switch c.AppEnv {
	case EnvLocal, EnvStaging, EnvProduction:
	default:
		fail("APP_ENV must be one of local, staging, production (got %q)", c.AppEnv)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "warning", "error":
	default:
		fail("LOG_LEVEL must be one of debug, info, warn, error (got %q)", c.LogLevel)
	}
	switch c.LogFormat {
	case LogFormatJSON, LogFormatText:
	case "":
		if c.AppEnv == EnvLocal {
			c.LogFormat = LogFormatText
		} else {
			c.LogFormat = LogFormatJSON
		}
	default:
		fail("LOG_FORMAT must be json or text (got %q)", c.LogFormat)
	}

	// The booking policy is validated for every binary rather than per group,
	// because more than one process reads each of these out of the same
	// environment and they only work if they agree. The API stamps the hold and
	// the worker sweeps it; the booking service prices a monthly plan and the
	// admin service refuses to create a slot too short to sell one; the API
	// stamps a job's retry budget and the worker spends it.
	if c.HoldDuration < time.Minute {
		fail("BOOKING_HOLD_DURATION must be at least 1 minute")
	}
	// A warning that fires at or after the hold has lapsed is worse than no
	// warning: the spots are already gone by the time it arrives.
	if c.ExpiryWarningWindow >= c.HoldDuration {
		fail("EXPIRY_WARNING_WINDOW (%s) must be shorter than BOOKING_HOLD_DURATION (%s)",
			c.ExpiryWarningWindow, c.HoldDuration)
	}
	if c.MonthlyWeekCount < 2 {
		fail("MONTHLY_WEEK_COUNT must be at least 2")
	}
	if c.VerifyMaxAttempts < 1 {
		fail("VERIFY_MAX_ATTEMPTS must be at least 1")
	}

	// The verifier libraries expose a TLS-skip escape hatch for CBE's
	// certificate chain. It must never be reachable in a deployed
	// environment: enabling it there would let anyone with network position
	// forge a payment receipt.
	if c.AllowInsecureVerifierTLS && c.AppEnv != EnvLocal {
		fail("ALLOW_INSECURE_VERIFIER_TLS may only be set when APP_ENV=local")
	}

	if need.DB {
		if c.DatabaseURL == "" {
			fail("DATABASE_URL is required")
		}
		if c.DBMinConns > c.DBMaxConns {
			fail("DB_MIN_CONNS (%d) cannot exceed DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns)
		}
	}

	if need.HTTP {
		if c.Port < 1 || c.Port > 65535 {
			fail("PORT must be between 1 and 65535 (got %d)", c.Port)
		}
		if c.RateLimitRPS <= 0 {
			fail("RATE_LIMIT_RPS must be positive")
		}
		if c.RateLimitBurst < 1 {
			fail("RATE_LIMIT_BURST must be at least 1")
		}
		if !validPublicBaseURL(c.PublicAPIBaseURL) {
			fail("PUBLIC_API_BASE_URL must be an absolute http or https origin (got %q)", c.PublicAPIBaseURL)
		}
	}

	if need.Auth {
		requireSecret("JWT_ACCESS_SECRET", c.JWTAccessSecret, fail)
		requireSecret("JWT_REFRESH_SECRET", c.JWTRefreshSecret, fail)
		if c.JWTAccessSecret != "" && c.JWTAccessSecret == c.JWTRefreshSecret {
			fail("JWT_ACCESS_SECRET and JWT_REFRESH_SECRET must differ")
		}
		if c.GoogleOAuthClientID == "" {
			fail("GOOGLE_OAUTH_CLIENT_ID is required")
		}
	}

	if need.Admin {
		requireSecret("ADMIN_JWT_ACCESS_SECRET", c.AdminJWTAccessSecret, fail)
		requireSecret("ADMIN_JWT_REFRESH_SECRET", c.AdminJWTRefreshSecret, fail)
		if c.AdminJWTAccessSecret != "" && c.AdminJWTAccessSecret == c.AdminJWTRefreshSecret {
			fail("ADMIN_JWT_ACCESS_SECRET and ADMIN_JWT_REFRESH_SECRET must differ")
		}
		// The admin API is a separate trust domain. Sharing a signing key
		// with the public API would let a player's access token be replayed
		// against admin endpoints if a role check were ever missed.
		if c.AdminJWTAccessSecret != "" && c.AdminJWTAccessSecret == c.JWTAccessSecret {
			fail("ADMIN_JWT_ACCESS_SECRET must differ from JWT_ACCESS_SECRET")
		}
		// The admin binary listens on its own port, so PORT's range check
		// says nothing about the one it actually binds.
		if c.AdminPort < 1 || c.AdminPort > 65535 {
			fail("ADMIN_PORT must be between 1 and 65535 (got %d)", c.AdminPort)
		}
		// Without an allowlist the dashboard cannot call the API from a
		// browser at all. Local development is exempt because the dashboard is
		// usually served from a dev server on a port that changes.
		if len(c.AdminCORSOrigins) == 0 && c.AppEnv != EnvLocal {
			fail("ADMIN_CORS_ORIGINS is required outside local (comma-separated dashboard origins)")
		}
	}

	if need.Accounts {
		if c.MerchantCBEAccount == "" {
			fail("MERCHANT_CBE_ACCOUNT is required (shown to payers and checked on receipts)")
		}
		if c.MerchantTelebirrAccount == "" {
			fail("MERCHANT_TELEBIRR_ACCOUNT is required (shown to payers and checked on receipts)")
		}
	}

	if need.Notify && c.AppEnv != EnvLocal {
		if c.ResendAPIKey == "" {
			fail("RESEND_API_KEY is required outside local")
		}
		if c.FCMServiceAccountJSON == "" {
			fail("FCM_SERVICE_ACCOUNT_JSON is required outside local")
		}
		// A reset mail with no link asks the user to copy a token by hand. It
		// works, but it is not what should ship to real customers.
		if c.PasswordResetURL == "" {
			fail("PASSWORD_RESET_URL is required outside local")
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return c, nil
}

// IsLocal reports whether this process is running in the local development
// environment.
func (c *Config) IsLocal() bool { return c.AppEnv == EnvLocal }

// requireSecret rejects absent and obviously weak signing keys. A short key
// makes offline brute force of the HMAC practical.
func requireSecret(name, value string, fail func(string, ...any)) {
	switch {
	case value == "":
		fail("%s is required", name)
	case len(value) < 32:
		fail("%s must be at least 32 characters (got %d)", name, len(value))
	}
}

func validPublicBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") &&
		u.User == nil && (u.Path == "" || u.Path == "/") &&
		u.RawQuery == "" && u.Fragment == ""
}

func envDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envList reads a comma-separated list, dropping empty entries. Absent and
// empty both yield nil, so callers test length rather than nil-ness.
func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envInt(key string, def int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer (got %q)", key, raw)
		return def
	}
	return v
}

func envFloat(key string, def float64, fail func(string, ...any)) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fail("%s must be a number (got %q)", key, raw)
		return def
	}
	return v
}

func envBool(key string, def bool, fail func(string, ...any)) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		fail("%s must be a boolean (got %q)", key, raw)
		return def
	}
	return v
}

func envDuration(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s must be a duration such as 30s or 15m (got %q)", key, raw)
		return def
	}
	if v <= 0 {
		fail("%s must be positive (got %q)", key, raw)
		return def
	}
	return v
}

// ErrNotConfigured is returned by optional integrations that were left
// unconfigured, so callers can degrade instead of crashing.
var ErrNotConfigured = errors.New("integration not configured")
