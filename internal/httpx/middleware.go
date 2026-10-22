package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// headerAuthUserID carries the authenticated user from RequireAuth back out to
// Logging. Context values only flow inward, so the outer logger cannot read the
// principal the inner middleware attached; this internal header is the return
// path. Logging deletes any inbound value first, so a client cannot spoof it,
// and RequireAuth sets it only after a token verifies.
const headerAuthUserID = "X-Auth-UserID"

// RequestID assigns each request a correlation ID, preferring a client-supplied
// X-Request-ID so a mobile client can tie its own logs to server logs, and
// echoes it back on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" || len(id) > 64 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// sensitivePathFragments marks routes whose request and response bodies must
// never be logged. Bodies are not logged anywhere in this middleware; the list
// additionally suppresses query strings, which is where a password reset token
// can appear.
var sensitivePathFragments = []string{"/auth/", "/payments"}

// Logging emits one structured line per request with the correlation ID, the
// authenticated user when present, and timing. Authorization headers, cookies
// and bodies are never recorded, and query strings are dropped on auth and
// payment routes.
func Logging(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := RequestIDFrom(r.Context())
			// Stripped before anything downstream runs; RequireAuth sets it
			// again only after a token verifies, so an inbound value is never
			// trusted.
			r.Header.Del(headerAuthUserID)

			log := base.With(
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			ctx := WithLogger(r.Context(), log)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r.WithContext(ctx))

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start).Round(time.Millisecond)),
				slog.String("real_ip", ClientIP(r)),
			}
			if q := r.URL.RawQuery; q != "" && !isSensitivePath(r.URL.Path) {
				attrs = append(attrs, slog.String("query", q))
			}
			// RequireAuth/OptionalAuth write this header on the shared request
			// header map, which is how the identity crosses back out of the
			// inner context.
			if userID := r.Header.Get(headerAuthUserID); userID != "" {
				attrs = append(attrs, slog.String("user_id", userID))
			}

			switch {
			case status >= 500:
				log.Error("request failed", attrs...)
			case status >= 400:
				log.Warn("request rejected", attrs...)
			default:
				log.Info("request", attrs...)
			}
		})
	}
}

func isSensitivePath(path string) bool {
	for _, fragment := range sensitivePathFragments {
		if strings.Contains(path, fragment) {
			return true
		}
	}
	return false
}

// Recover turns a panic into a 500 with the standard envelope and logs the
// stack, so one bad request cannot take the process down.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// A client that hung up mid-write is not a server fault.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				LoggerFrom(r.Context()).Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())))
				Fail(w, r, ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets the response headers relevant to a JSON API consumed by
// a mobile app and an admin SPA.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds handler execution. The handler observes cancellation through
// the request context, which propagates into pgx and the verifier clients.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP resolves the caller's address. Railway terminates TLS and forwards
// the original address in X-Forwarded-For; the left-most entry is used, since
// the platform appends rather than replaces.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found || first != "" {
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimiter is a fixed-rate token bucket per client key, with idle buckets
// evicted so a long-running process does not accumulate one entry per address
// seen.
type RateLimiter struct {
	rps   rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*bucket
	stop    chan struct{}
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter starts a limiter allowing rps sustained requests per key with
// the given burst allowance. Call Close to stop the eviction goroutine.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	go rl.evictLoop()
	return rl
}

// Close stops background eviction.
func (rl *RateLimiter) Close() { close(rl.stop) }

func (rl *RateLimiter) evictLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			rl.mu.Lock()
			for key, b := range rl.buckets {
				if b.lastSeen.Before(cutoff) {
					delete(rl.buckets, key)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *RateLimiter) allow(key string) bool {
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(rl.rps, rl.burst)}
		rl.buckets[key] = b
	}
	b.lastSeen = time.Now()
	rl.mu.Unlock()
	return b.limiter.Allow()
}

// Middleware limits by client IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(ClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			Fail(w, r, ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ByUser limits by authenticated user, falling back to IP for anonymous
// callers. Mount it after authentication on endpoints where the expensive work
// is per-account rather than per-connection, such as payment submission.
func (rl *RateLimiter) ByUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "ip:" + ClientIP(r)
		if p, ok := PrincipalFrom(r.Context()); ok {
			key = "user:" + p.UserID.String()
		}
		if !rl.allow(key) {
			w.Header().Set("Retry-After", "1")
			Fail(w, r, ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}
