//go:build integration

// Hold-expiry tests, covering Fix 2 Part A.
//
// The sweep is the only thing that returns spots nobody paid for, so these tests
// care about two opposite mistakes: failing to release a lapsed hold, which
// takes a game off sale for good, and releasing a hold whose payer is mid-
// verification, which takes the spots away from somebody who did everything
// right and already sent the money.
package booking_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

// newSweeper builds the sweeper with a real notification service, so the tests
// also cover the part of the design that matters most about it: the notification
// commits in the same transaction as the state change it explains.
func newSweeper(store *db.Store) *booking.Sweeper {
	log := slog.New(slog.DiscardHandler)
	return booking.NewSweeper(booking.SweepOptions{
		Store:       store,
		Logger:      log,
		Notifier:    notification.NewService(store, log, false),
		WarnBefore:  2 * time.Minute,
		WarnMaxRows: 50,
	})
}

// countNotifications counts what a user was told, by type.
func countNotifications(t *testing.T, store *db.Store, userID uuid.UUID, typ string) int64 {
	t.Helper()
	var n int64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = $2`, userID, typ).Scan(&n); err != nil {
		t.Fatalf("count %s notifications: %v", typ, err)
	}
	return n
}

// setHoldExpiry moves a booking's hold relative to now, so a test can reach the
// state a five-minute window would otherwise make it wait for.
func setHoldExpiry(t *testing.T, store *db.Store, bookingID uuid.UUID, in time.Duration) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(),
		`UPDATE bookings SET hold_expires_at = now() + make_interval(secs => $2::double precision) WHERE id = $1`,
		bookingID, in.Seconds()); err != nil {
		t.Fatalf("set hold expiry: %v", err)
	}
}

// reopenLapsedHold puts a booking back into 'pending_payment' with a hold that
// has already run out.
//
// It writes the row directly on purpose. The guard being tested is the sweep's
// "not while a verification job is live" clause, and the application path moves a
// booking to 'verifying' as it queues that job -- so the only way to show the
// clause carries its own weight, rather than being shadowed by the status check
// beside it, is to construct the state it exists for.
func reopenLapsedHold(t *testing.T, store *db.Store, bookingID uuid.UUID) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(),
		`UPDATE bookings
		 SET status = 'pending_payment', hold_expires_at = now() - interval '1 minute'
		 WHERE id = $1`, bookingID); err != nil {
		t.Fatalf("reopen lapsed hold: %v", err)
	}
}

// TestExpireOnceReleasesALapsedHold covers the ordinary case: the payer walked
// away, and the spots go back on sale (PRD FR13).
func TestExpireOnceReleasesALapsedHold(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	sweeper := newSweeper(store)
	ctx := context.Background()

	const capacity = 10
	const spots = 3

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: capacity})
	user := testsupport.User(t, store)

	view, err := svc.Create(ctx, booking.CreateInput{
		UserID:      user.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   spots,
	})
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity-spots {
		t.Fatalf("spots_remaining = %d after booking, want %d", left, capacity-spots)
	}

	testsupport.BackdateHold(t, store, view.ID)

	expired, err := sweeper.ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire once: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d bookings, want 1", expired)
	}
	if got := bookingStatus(t, store, view.ID); got != booking.StatusExpired {
		t.Errorf("booking status = %q, want %q", got, booking.StatusExpired)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity {
		t.Errorf("spots_remaining = %d after expiry, want %d: the game is short %d spots for good",
			left, capacity, capacity-left)
	}
	if n := countNotifications(t, store, user.ID, notification.TypeHoldExpired); n != 1 {
		t.Errorf("hold_expired notifications = %d, want 1", n)
	}

	// The sweep runs every fifteen seconds forever, so re-running it must be
	// inert: a second credit would push spots above capacity.
	again, err := sweeper.ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("second expire: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep expired %d bookings, want 0", again)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity {
		t.Errorf("spots_remaining = %d after a second sweep, want %d", left, capacity)
	}
}

// TestExpireOnceSparesBookingsUnderVerification is Fix 2 Part A: a hold that
// lapsed while the bank was still answering must not be swept.
func TestExpireOnceSparesBookingsUnderVerification(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	sweeper := newSweeper(store)
	paySvc := payment.NewService(payment.Options{Store: store, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()

	const capacity = 10
	const spots = 4

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: capacity})
	user := testsupport.User(t, store)

	view, err := svc.Create(ctx, booking.CreateInput{
		UserID:      user.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   spots,
	})
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}

	// A real submission, so the job under test is queued the way the API queues
	// it rather than inserted by hand.
	submission, err := payment.Canonicalize(payment.MethodTelebirr, "CFB9QK2LM4", "")
	if err != nil {
		t.Fatalf("canonicalize reference: %v", err)
	}
	if _, _, err := paySvc.Submit(ctx, payment.SubmitInput{
		BookingID:      view.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      submission.Reference,
		IdempotencyKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("submit payment: %v", err)
	}

	reopenLapsedHold(t, store, view.ID)

	expired, err := sweeper.ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire once: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expired %d bookings with a live verification job, want 0", expired)
	}
	if got := bookingStatus(t, store, view.ID); got != booking.StatusPendingPayment {
		t.Errorf("booking status = %q, want it untouched at %q", got, booking.StatusPendingPayment)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity-spots {
		t.Errorf("spots_remaining = %d, want %d: spots were taken from a payer whose money was already sent",
			left, capacity-spots)
	}
	if n := countNotifications(t, store, user.ID, notification.TypeHoldExpired); n != 0 {
		t.Errorf("hold_expired notifications = %d, want 0", n)
	}

	// With the job finished, the same lapsed hold is fair game again -- which is
	// what shows the job, not the status, was doing the sparing.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE payment_verification_jobs SET status = 'completed'`); err != nil {
		t.Fatalf("complete verification job: %v", err)
	}
	expired, err = sweeper.ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire after the job finished: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d bookings once no job was live, want 1", expired)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != capacity {
		t.Errorf("spots_remaining = %d after expiry, want %d", left, capacity)
	}
}

// TestWarnOnceNudgesEachHoldOnce covers the other half of the sweep: the
// "finish your payment" nudge, which must go out once however often the sweep
// runs or however many workers run it.
func TestWarnOnceNudgesEachHoldOnce(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	sweeper := newSweeper(store)
	ctx := context.Background()

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: 6})
	user := testsupport.User(t, store)

	view, err := svc.Create(ctx, booking.CreateInput{
		UserID:      user.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   2,
	})
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}

	// Inside the two-minute warning window, still comfortably in the future.
	setHoldExpiry(t, store, view.ID, 30*time.Second)

	warned, err := sweeper.WarnOnce(ctx)
	if err != nil {
		t.Fatalf("warn once: %v", err)
	}
	if warned != 1 {
		t.Fatalf("warned %d bookings, want 1", warned)
	}
	if n := countNotifications(t, store, user.ID, notification.TypeHoldExpiring); n != 1 {
		t.Fatalf("hold_expiring notifications = %d, want 1", n)
	}

	again, err := sweeper.WarnOnce(ctx)
	if err != nil {
		t.Fatalf("second warn: %v", err)
	}
	if again != 0 {
		t.Errorf("second pass warned %d bookings, want 0", again)
	}
	if n := countNotifications(t, store, user.ID, notification.TypeHoldExpiring); n != 1 {
		t.Errorf("hold_expiring notifications = %d after two passes, want 1", n)
	}
	// The nudge must not touch the hold it is warning about.
	if got := bookingStatus(t, store, view.ID); got != booking.StatusPendingPayment {
		t.Errorf("booking status = %q after warning, want %q", got, booking.StatusPendingPayment)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != 4 {
		t.Errorf("spots_remaining = %d after warning, want 4", left)
	}
}
