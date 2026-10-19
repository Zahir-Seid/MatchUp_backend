//go:build integration

// The "notify me" watchlist: watching a sold-out game, being told when a spot
// frees up, and the watch being spent by the alert.
//
// These tests care about the join between two halves of the package: the sweep
// makes spots available, and the person who asked to hear about it is told in
// the same transaction. Fake either side and the thing worth testing -- that
// the release and the alert commit together -- disappears.
package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

// TestWatchRefusals covers what cannot be watched: a game that still has
// spots, a game that started, a game that was cancelled, and one that does not
// exist.
func TestWatchRefusals(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	ctx := context.Background()

	field := testsupport.Field(t, store)
	user := testsupport.User(t, store)

	open := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID})
	if err := svc.Watch(ctx, user.ID, open.ID); err == nil || apiCode(err) != httpx.CodeGameNotFull {
		t.Fatalf("watch on a game with spots: err = %v, want code %q", err, httpx.CodeGameNotFull)
	}

	started := testsupport.Game(t, store, testsupport.GameSpec{
		FieldID:  field.ID,
		StartsAt: time.Now().UTC().Add(-time.Hour),
	})
	if err := svc.Watch(ctx, user.ID, started.ID); err == nil || apiCode(err) != httpx.CodeGameUnavailable {
		t.Fatalf("watch on a started game: err = %v, want code %q", err, httpx.CodeGameUnavailable)
	}

	cancelled := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID})
	if _, err := store.CancelGame(ctx, cancelled.ID); err != nil {
		t.Fatalf("cancel game: %v", err)
	}
	if err := svc.Watch(ctx, user.ID, cancelled.ID); err == nil || apiCode(err) != httpx.CodeGameUnavailable {
		t.Fatalf("watch on a cancelled game: err = %v, want code %q", err, httpx.CodeGameUnavailable)
	}

	if err := svc.Watch(ctx, user.ID, uuid.New()); err == nil || apiCode(err) != httpx.CodeNotFound {
		t.Fatalf("watch on a game that does not exist: err = %v, want code %q", err, httpx.CodeNotFound)
	}
}

// TestWatchAlertOnExpiredHold is the whole loop end to end: a game sells out, a
// second user asks to be told when it reopens, the unpaid hold lapses, and the
// alert arrives -- exactly once, clearing the watch, from the transaction that
// released the spots.
func TestWatchAlertOnExpiredHold(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	sweeper := newSweeper(store)
	ctx := context.Background()

	const capacity = 2

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: capacity})
	buyer := testsupport.User(t, store)
	watcher := testsupport.User(t, store)

	// Sell the game out: the whole capacity goes to one unpaid hold.
	view, err := svc.Create(ctx, booking.CreateInput{
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

	// Watching is idempotent: two taps from a flaky network are one watch.
	if err := svc.Watch(ctx, watcher.ID, game.ID); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := svc.Watch(ctx, watcher.ID, game.ID); err != nil {
		t.Fatalf("watch twice: %v", err)
	}
	if watching, err := svc.Watching(ctx, watcher.ID, game.ID); err != nil || !watching {
		t.Fatalf("watching before the release = %v (err %v), want true", watching, err)
	}

	// The hold lapses and the sweep releases it.
	testsupport.BackdateHold(t, store, view.ID)
	expired, err := sweeper.ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire once: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d bookings, want 1", expired)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity {
		t.Fatalf("spots_remaining = %d after the release, want %d", left, capacity)
	}

	if got := countNotifications(t, store, watcher.ID, notification.TypeGameSpotAvailable); got != 1 {
		t.Errorf("watcher got %d spot-available alerts, want 1", got)
	}
	// The alert is the whole point of the watch, and the watch is spent by it:
	// a second release must not alert again.
	if watching, err := svc.Watching(ctx, watcher.ID, game.ID); err != nil || watching {
		t.Errorf("watching after the release = %v (err %v), want false", watching, err)
	}
	// The buyer is told their hold lapsed, and is not told about spots they
	// alone were holding.
	if got := countNotifications(t, store, buyer.ID, notification.TypeHoldExpired); got != 1 {
		t.Errorf("buyer got %d hold-expired notices, want 1", got)
	}
	if got := countNotifications(t, store, buyer.ID, notification.TypeGameSpotAvailable); got != 0 {
		t.Errorf("buyer got %d spot-available alerts, want 0", got)
	}

	// The payload carries what the app needs to deep-link the alert.
	var fieldName, gameID string
	if err := store.Pool().QueryRow(ctx,
		`SELECT payload->>'field_name', payload->>'game_id' FROM notifications
		 WHERE user_id = $1 AND type = $2`,
		watcher.ID, notification.TypeGameSpotAvailable).Scan(&fieldName, &gameID); err != nil {
		t.Fatalf("read alert payload: %v", err)
	}
	if fieldName != field.Name || gameID != game.ID.String() {
		t.Errorf("payload field_name = %q game_id = %q, want %q and %q",
			fieldName, gameID, field.Name, game.ID)
	}
}

// TestUnwatchIsIdempotent: stopping the alerts twice is not an error, and the
// watch is gone either way.
func TestUnwatchIsIdempotent(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	ctx := context.Background()

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: 1})
	user := testsupport.User(t, store)

	// Sold out directly: the booking path that fills a game is covered by the
	// tests above.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE games SET spots_remaining = 0 WHERE id = $1`, game.ID); err != nil {
		t.Fatalf("sell out game: %v", err)
	}

	if err := svc.Watch(ctx, user.ID, game.ID); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := svc.Unwatch(ctx, user.ID, game.ID); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	if err := svc.Unwatch(ctx, user.ID, game.ID); err != nil {
		t.Fatalf("unwatch twice: %v", err)
	}
	if watching, err := svc.Watching(ctx, user.ID, game.ID); err != nil || watching {
		t.Errorf("watching after unwatch = %v (err %v), want false", watching, err)
	}
}
