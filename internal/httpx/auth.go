package httpx

import (
	"context"
	"net/http"
	"strings"
)

// TokenVerifier validates a bearer access token and resolves the caller. It is
// an interface here so the HTTP layer does not depend on the auth package,
// which depends on this one for its error types.
type TokenVerifier interface {
	VerifyAccessToken(ctx context.Context, token string) (Principal, error)
}

// RequireAuth rejects requests without a valid bearer token and attaches the
// resolved principal to the request context.
func RequireAuth(v TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				Fail(w, r, ErrUnauthorized)
				return
			}
			principal, err := v.VerifyAccessToken(r.Context(), token)
			if err != nil {
				Fail(w, r, err)
				return
			}
			// Set on the shared request header map so Logging, an outer
			// middleware, can report the user; context values only flow inward.
			r.Header.Set(headerAuthUserID, principal.UserID.String())
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// OptionalAuth attaches a principal when a valid token is present and
// otherwise proceeds anonymously. Used for endpoints whose response is richer
// for a signed-in caller but still meaningful without one. An invalid token is
// ignored rather than rejected, so a stale token cannot make browsing fail.
func OptionalAuth(v TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token, ok := bearerToken(r); ok {
				if principal, err := v.VerifyAccessToken(r.Context(), token); err == nil {
					r.Header.Set(headerAuthUserID, principal.UserID.String())
					r = r.WithContext(WithPrincipal(r.Context(), principal))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole enforces a role on top of RequireAuth. Mounting it without
// RequireAuth in front yields 401 rather than allowing the request through.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := PrincipalFrom(r.Context())
			if !ok {
				Fail(w, r, ErrUnauthorized)
				return
			}
			if principal.Role != role {
				Fail(w, r, ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization header, requiring the
// Bearer scheme.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// CORS answers preflight requests and sets the response headers for browser
// clients. The admin dashboard is the only browser consumer; the mobile app is
// unaffected. Origins are matched exactly against the allowlist — no wildcard,
// because credentialed requests must not be readable by arbitrary sites.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.ToLower(o)] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowed[strings.ToLower(origin)]; ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Set("Access-Control-Allow-Credentials", "true")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, Idempotency-Key")
					h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Max-Age", "600")
					h.Add("Vary", "Origin")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// contextWithoutCancel is used where a follow-up write must complete even if
// the client has hung up, such as recording an audit entry for an action that
// already happened.
func contextWithoutCancel(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// DetachedContext exposes contextWithoutCancel to other packages.
func DetachedContext(ctx context.Context) context.Context { return contextWithoutCancel(ctx) }
