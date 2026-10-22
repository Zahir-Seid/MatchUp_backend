package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type stubVerifier struct {
	principal Principal
	err       error
}

func (s stubVerifier) VerifyAccessToken(context.Context, string) (Principal, error) {
	return s.principal, s.err
}

// logEntry runs one request through the same order the routers use and returns
// the decoded access-log line.
func logEntry(t *testing.T, h http.Handler, req *http.Request) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()

	RequestID(Logging(base)(h)).ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	return entry
}

// TestLoggingReportsAuthenticatedUser covers the reason the internal header
// exists: RequireAuth attaches the principal to a context the outer logger
// cannot see, so without the header user_id is always missing.
func TestLoggingReportsAuthenticatedUser(t *testing.T) {
	userID := uuid.New()
	handler := RequireAuth(stubVerifier{principal: Principal{UserID: userID, Role: "player"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.RemoteAddr = "192.0.2.10:4321"

	entry := logEntry(t, handler, req)

	if got := entry["user_id"]; got != userID.String() {
		t.Errorf("user_id = %v, want %s", got, userID)
	}
	if got := entry["real_ip"]; got != "192.0.2.10" {
		t.Errorf("real_ip = %v, want 192.0.2.10", got)
	}
	if got := entry["status"]; got != float64(http.StatusNoContent) {
		t.Errorf("status = %v, want %d", got, http.StatusNoContent)
	}
	if got, _ := entry["request_id"].(string); got == "" {
		t.Error("request_id missing")
	}
}

// TestLoggingIgnoresSpoofedUserHeader requires a client-supplied header to be
// discarded: it is an internal carrier, not authentication.
func TestLoggingIgnoresSpoofedUserHeader(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/fields", nil)
	req.Header.Set(headerAuthUserID, uuid.NewString())

	entry := logEntry(t, handler, req)

	if got, ok := entry["user_id"]; ok {
		t.Errorf("unauthenticated request logged user_id = %v", got)
	}
}

// TestLoggingLevelFollowsStatus keeps the level contract: operators alert on
// ERROR for server faults, WARN for rejected requests, INFO for served ones.
func TestLoggingLevelFollowsStatus(t *testing.T) {
	cases := []struct {
		status int
		level  string
		msg    string
	}{
		{http.StatusOK, "INFO", "request"},
		{http.StatusUnauthorized, "WARN", "request rejected"},
		{http.StatusInternalServerError, "ERROR", "request failed"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var buf bytes.Buffer
			base := slog.New(slog.NewJSONHandler(&buf, nil))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			rec := httptest.NewRecorder()
			RequestID(Logging(base)(handler)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			var entry struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
			}
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("decode log line %q: %v", buf.String(), err)
			}
			if entry.Level != tc.level || entry.Msg != tc.msg {
				t.Errorf("logged %s %q, want %s %q", entry.Level, entry.Msg, tc.level, tc.msg)
			}
		})
	}
}
