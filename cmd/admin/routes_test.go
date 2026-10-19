package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/config"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/openapi"
)

// TestRoutesMatchSpec is the drift gate between this binary and its committed
// OpenAPI document. It walks the router the server actually runs and compares
// it with internal/openapi/admin.json in both directions: an operation the
// document promises but no route serves, and a route nobody documented, are
// both failures. Regenerate the document with `make docs`.
func TestRoutesMatchSpec(t *testing.T) {
	r := newTestRouter(t)
	want := specOperations(t, openapi.AdminSpec())
	have := routerOperations(t, r, "/admin/v1")

	for op := range want {
		if !have[op] {
			t.Errorf("documented but not routed: %s", op)
		}
	}
	for op := range have {
		if !want[op] {
			t.Errorf("routed but not documented: %s", op)
		}
	}
}

// TestDocsServing checks that the binary serves both halves of the generated
// contract: the JSON document at /openapi.json, and the Swagger UI at /docs/
// rendering the same bytes.
func TestDocsServing(t *testing.T) {
	r := newTestRouter(t)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d", rec.Code)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("GET /openapi.json: openapi version %q, want 3.x", doc.OpenAPI)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/" {
		t.Errorf("GET /docs: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /docs/: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/swagger_spec", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /docs/swagger_spec: status %d", rec.Code)
	} else if !bytes.Equal(rec.Body.Bytes(), openapi.AdminSpec()) {
		t.Errorf("GET /docs/swagger_spec: body does not match the embedded document")
	}
}

// newTestRouter builds the router the binary serves with dependencies stubbed.
// Routing is what is under test; no handler body ever runs.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	limiter := httpx.NewRateLimiter(1, 1)
	t.Cleanup(limiter.Close)
	noLimit := func(next http.Handler) http.Handler { return next }

	return newRouter(routerDeps{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:           &config.Config{WriteTimeout: 30 * time.Second},
		pinger:        pingerFunc(func(context.Context) error { return nil }),
		authH:         auth.NewHandler(nil, noLimit),
		adminH:        admin.NewHandler(nil),
		globalLimiter: limiter,
	})
}

// pingerFunc adapts a function to httpx.Pinger.
type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// specOperations lists every operation in an OpenAPI document as
// "METHOD /path", the way the router reports its routes.
func specOperations(t *testing.T, spec []byte) map[string]bool {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("parse embedded OpenAPI document: %v", err)
	}
	ops := map[string]bool{}
	for path, item := range doc.Paths {
		for key := range item {
			if method := strings.ToUpper(key); httpMethods[method] {
				ops[method+" "+path] = true
			}
		}
	}
	return ops
}

// httpMethods filters path-level fields such as "parameters" out of a path
// item's keys.
var httpMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// routerOperations walks the real route table, keyed the same way as the
// document: basePath stripped, operational endpoints dropped.
func routerOperations(t *testing.T, r http.Handler, basePath string) map[string]bool {
	t.Helper()
	ops := map[string]bool{}
	err := chi.Walk(r.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route, ok := documentedRoute(route, basePath); ok {
			ops[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	return ops
}

// documentedRoute maps a router path onto the document's, or reports that the
// route is intentionally undocumented: the probes, the document itself and the
// UI exist for operability and discovery, not as API surface.
func documentedRoute(route, basePath string) (string, bool) {
	switch route {
	case "/healthz", "/readyz", "/openapi.json", "/docs", "/docs/*":
		return "", false
	}
	// A handler registered at a subrouter's root is reported as "/fields/"
	// while it serves "/fields": the trailing slash is chi's, not the URL's.
	route = strings.TrimSuffix(route, "/")
	return strings.TrimPrefix(route, basePath), true
}
