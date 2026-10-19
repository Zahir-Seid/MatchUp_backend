//go:build integration

// The watchlist's admin side: two more paths make a spot available on a
// sold-out game -- raising capacity and cancelling a booking -- and the people
// watching must hear about both from the same transaction that did it.
//
// It lives with the admin package because both paths are admin API operations,
// but the assertions that matter are the watcher's alert and the watch being
// spent, which is what booking.NotifySpotsAvailable promises on any release.
package admin_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

// watchFixture is a sold-out game with one watcher on it, plus the booking
// that holds the game's only spots.
type watchFixture struct {
	store    *db.Store
	adminSvc *admin.Service
	bookSvc  *booking.Service
	game     db.Game
	holdID   uuid.UUID
	watcher  uuid.UUID
}

func newWatchFixture(t *testing.T) watchFixture {
	t.Helper()

	store := testsupport.Store(t)
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	adminSvc := admin.NewService(admin.Options{
		Store:    store,
		Audit:    audit.NewRecorder(store, log),
		Notifier: notification.NewService(store, log, false),
		Logger:   log,
	})
	bookSvc := booking.NewService(booking.Options{
		Store:        store,
		Logger:       log,
		HoldDuration: 5 * time.Minute,
		WeekCount:    4,
	})

	const capacity = 2

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: capacity})
	buyer := testsupport.User(t, store)
	watcher := testsupport.User(t, store)

	view, err := bookSvc.Create(ctx, booking.CreateInput{
		UserID:      buyer.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   capacity,
	})
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != 0 {
		t.Fatalf("spots_remaining = %d after selling out, want 0", left)
	}
	if err := bookSvc.Watch(ctx, watcher.ID, game.ID); err != nil {
		t.Fatalf("watch: %v", err)
	}

	return watchFixture{
		store:    store,
		adminSvc: adminSvc,
		bookSvc:  bookSvc,
		game:     game,
		holdID:   view.ID,
		watcher:  watcher.ID,
	}
}

func TestAdjustCapacityAlertsWatchers(t *testing.T) {
	f := newWatchFixture(t)
	ctx := context.Background()
	actor := testsupport.User(t, f.store)

	if _, err := f.adminSvc.AdjustCapacity(ctx, actor.ID, f.game.ID, 2, "integration test"); err != nil {
		t.Fatalf("adjust capacity: %v", err)
	}
	if left := testsupport.SpotsRemaining(t, f.store, f.game.ID); left != 2 {
		t.Fatalf("spots_remaining = %d after the raise, want 2", left)
	}

	if got := countSpotAlerts(t, f.store, f.watcher); got != 1 {
		t.Errorf("watcher got %d spot-available alerts, want 1", got)
	}
	if watching, err := f.bookSvc.Watching(ctx, f.watcher, f.game.ID); err != nil || watching {
		t.Errorf("watching after the raise = %v (err %v), want false", watching, err)
	}
}

func TestCancelBookingAlertsWatchers(t *testing.T) {
	f := newWatchFixture(t)
	ctx := context.Background()
	actor := testsupport.User(t, f.store)

	if _, err := f.adminSvc.CancelBooking(ctx, actor.ID, f.holdID, "integration test"); err != nil {
		t.Fatalf("cancel booking: %v", err)
	}
	if left := testsupport.SpotsRemaining(t, f.store, f.game.ID); left != 2 {
		t.Fatalf("spots_remaining = %d after the cancellation, want 2", left)
	}

	if got := countSpotAlerts(t, f.store, f.watcher); got != 1 {
		t.Errorf("watcher got %d spot-available alerts, want 1", got)
	}
	if watching, err := f.bookSvc.Watching(ctx, f.watcher, f.game.ID); err != nil || watching {
		t.Errorf("watching after the cancellation = %v (err %v), want false", watching, err)
	}
}

// countSpotAlerts counts the "a spot opened up" alerts a user received.
func countSpotAlerts(t *testing.T, store *db.Store, userID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = $2`,
		userID, notification.TypeGameSpotAvailable).Scan(&n); err != nil {
		t.Fatalf("count spot alerts: %v", err)
	}
	return n
}
