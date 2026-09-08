//go:build integration

package booking_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

const (
	// testHold is long enough that no hold lapses while a test runs. The expiry
	// tests reach the lapsed state by backdating the row, never by waiting.
	testHold = 5 * time.Minute
	// testWeeks matches the default MONTHLY_WEEK_COUNT, so a monthly booking
	// here covers the same four occurrences it covers in production.
	testWeeks = 4
)

// newBookingService builds the service under test with logging discarded: these
// tests assert on state and on error codes, and a passing run should be silent.
func newBookingService(store *db.Store) *booking.Service {
	return booking.NewService(booking.Options{
		Store:        store,
		Logger:       slog.New(slog.DiscardHandler),
		HoldDuration: testHold,
		WeekCount:    testWeeks,
	})
}

// apiCode is the stable code the failure would have shown the client. Asserting
// on it rather than on the message keeps these tests from breaking over
// rewording.
func apiCode(err error) httpx.Code { return httpx.AsError(err).Code }

// countRows counts a table, to prove a rejected attempt left nothing behind --
// something an assertion on the surviving booking cannot show.
func countRows(t *testing.T, store *db.Store, table string) int64 {
	t.Helper()
	var n int64
	// table is a literal from the caller, never anything a request supplied.
	if err := store.Pool().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// bookingStatus reads one booking's status straight from the row.
func bookingStatus(t *testing.T, store *db.Store, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT status FROM bookings WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read booking status: %v", err)
	}
	return status
}

// waitForRowLockContention blocks until some backend is waiting on a lock.
//
// It is how the rollback test knows the booking transaction has actually reached
// the row its fixture is holding, rather than guessing with a sleep: the
// interleaving the test exists to exercise becomes guaranteed instead of likely.
func waitForRowLockContention(t *testing.T, store *db.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend blocked on a lock within 5s: the booking never reached the contended row")
}
