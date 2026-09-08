//go:build integration

// Submission tests.
//
// Submitting a payment is the one write in the system that a customer will retry
// by hand -- they tap the button again when the network drops, and they paste the
// same receipt twice when they are not sure it went through. So these tests are
// mostly about what must *not* happen twice: two payments for one tap, two
// bookings paid for with one bank transaction, and a payment attached to a
// reservation that no longer exists (PRD FR16, FR17, Fix 2 Part B).
package payment_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

const testHold = 5 * time.Minute

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newBookingService(store *db.Store) *booking.Service {
	return booking.NewService(booking.Options{
		Store:        store,
		Logger:       quiet(),
		HoldDuration: testHold,
		WeekCount:    4,
	})
}

func newPaymentService(store *db.Store) *payment.Service {
	return payment.NewService(payment.Options{Store: store, Logger: quiet()})
}

func newSweeper(store *db.Store) *booking.Sweeper {
	return booking.NewSweeper(booking.SweepOptions{Store: store, Logger: quiet()})
}

func apiCode(err error) httpx.Code { return httpx.AsError(err).Code }

func countRows(t *testing.T, store *db.Store, table string) int64 {
	t.Helper()
	var n int64
	// table is a literal from the caller, never anything a request supplied.
	if err := store.Pool().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func bookingStatus(t *testing.T, store *db.Store, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT status FROM bookings WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read booking status: %v", err)
	}
	return status
}

// heldBooking is the state every test here starts from: a booking holding spots
// and waiting to be paid for.
func heldBooking(t *testing.T, store *db.Store, spots int32) (db.User, *booking.View) {
	t.Helper()
	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: 10})
	user := testsupport.User(t, store)
	view, err := newBookingService(store).Create(context.Background(), booking.CreateInput{
		UserID:      user.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   spots,
	})
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	return user, view
}

// TestSubmitReplaysTheSameIdempotencyKey covers the retried tap: the same key
// returns the original payment rather than queueing a second bank lookup.
func TestSubmitReplaysTheSameIdempotencyKey(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	user, view := heldBooking(t, store, 3)
	key := uuid.NewString()
	in := payment.SubmitInput{
		BookingID:      view.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      "CFB9QK2LM4",
		IdempotencyKey: key,
	}

	first, replayed, err := svc.Submit(ctx, in)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if replayed {
		t.Error("first submit reported a replay")
	}
	// FR16: the amount checked against the bank is read from the booking, and
	// there is no field on the request that could have influenced it.
	if first.ExpectedAmountSantim != view.TotalSantim {
		t.Errorf("expected_amount_santim = %d, want the booking total %d",
			first.ExpectedAmountSantim, view.TotalSantim)
	}
	if got := bookingStatus(t, store, view.ID); got != booking.StatusVerifying {
		t.Errorf("booking status = %q, want %q", got, booking.StatusVerifying)
	}

	second, replayed, err := svc.Submit(ctx, in)
	if err != nil {
		t.Fatalf("replayed submit: %v", err)
	}
	if !replayed {
		t.Error("replayed submit did not report a replay")
	}
	if second.ID != first.ID {
		t.Errorf("replay returned payment %s, want the original %s", second.ID, first.ID)
	}
	if n := countRows(t, store, "payments"); n != 1 {
		t.Errorf("payments rows = %d, want 1", n)
	}
	// The second lookup would cost the customer nothing but would ask the bank
	// about the same reference twice, and each answer can move the booking.
	if n := countRows(t, store, "payment_verification_jobs"); n != 1 {
		t.Errorf("payment_verification_jobs rows = %d, want 1", n)
	}
}

// TestSubmitRejectsAKeyReusedForAnotherBooking: silently returning the other
// booking's payment would hide a client bug that loses somebody's money.
func TestSubmitRejectsAKeyReusedForAnotherBooking(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	user, first := heldBooking(t, store, 2)
	key := uuid.NewString()
	if _, _, err := svc.Submit(ctx, payment.SubmitInput{
		BookingID:      first.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      "CFB1ZZ8QW2",
		IdempotencyKey: key,
	}); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// A second booking for the same user, so only the key is reused.
	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: 8})
	second, err := newBookingService(store).Create(ctx, booking.CreateInput{
		UserID:      user.ID,
		GameID:      game.ID,
		BookingType: booking.TypeSingle,
		SpotCount:   1,
	})
	if err != nil {
		t.Fatalf("create second booking: %v", err)
	}

	_, _, err = svc.Submit(ctx, payment.SubmitInput{
		BookingID:      second.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      "CFB4YY1PL7",
		IdempotencyKey: key,
	})
	if err == nil {
		t.Fatal("reusing an idempotency key across bookings succeeded")
	}
	if code := apiCode(err); code != httpx.CodeIdempotencyConflict {
		t.Errorf("error code = %s (%v), want %s", code, err, httpx.CodeIdempotencyConflict)
	}
	if n := countRows(t, store, "payments"); n != 1 {
		t.Errorf("payments rows = %d, want 1", n)
	}
	if got := bookingStatus(t, store, second.ID); got != booking.StatusPendingPayment {
		t.Errorf("second booking status = %q, want it still %q", got, booking.StatusPendingPayment)
	}
}

// TestSubmitRejectsAReusedTransactionReference is FR17: one bank transaction
// pays for exactly one booking, system-wide, no matter who submits it.
func TestSubmitRejectsAReusedTransactionReference(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	const reference = "CFB7MM3TR9"

	firstUser, first := heldBooking(t, store, 2)
	if _, _, err := svc.Submit(ctx, payment.SubmitInput{
		BookingID:      first.ID,
		UserID:         firstUser.ID,
		Method:         payment.MethodTelebirr,
		Reference:      reference,
		IdempotencyKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// A different person, a different booking, the same receipt.
	secondUser, second := heldBooking(t, store, 1)
	_, _, err := svc.Submit(ctx, payment.SubmitInput{
		BookingID:      second.ID,
		UserID:         secondUser.ID,
		Method:         payment.MethodTelebirr,
		Reference:      reference,
		IdempotencyKey: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("a reused transaction reference was accepted for a second booking")
	}
	if code := apiCode(err); code != httpx.CodePaymentReferenceReused {
		t.Errorf("error code = %s (%v), want %s", code, err, httpx.CodePaymentReferenceReused)
	}
	if n := countRows(t, store, "payments"); n != 1 {
		t.Errorf("payments rows = %d, want 1", n)
	}
	// The claim moved the second booking to 'verifying' inside the transaction
	// that then rolled back, so the row must be untouched -- otherwise the
	// booking would sit in 'verifying' with no payment and no job, invisible to
	// both the sweep and the worker.
	if got := bookingStatus(t, store, second.ID); got != booking.StatusPendingPayment {
		t.Errorf("second booking status = %q, want %q", got, booking.StatusPendingPayment)
	}
	if n := countRows(t, store, "payment_verification_jobs"); n != 1 {
		t.Errorf("payment_verification_jobs rows = %d, want 1", n)
	}
}

// TestSubmitRefusesAnExpiredBooking is Fix 2 Part B: once the sweep has released
// the spots, the payment must not attach to the dead reservation. Taking it would
// leave the customer paid-up for a booking whose spots are already resold.
func TestSubmitRefusesAnExpiredBooking(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	user, view := heldBooking(t, store, 2)
	testsupport.BackdateHold(t, store, view.ID)
	expired, err := newSweeper(store).ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire booking: %v", err)
	}
	if expired != 1 {
		t.Fatalf("sweep expired %d bookings, want 1", expired)
	}

	_, _, err = svc.Submit(ctx, payment.SubmitInput{
		BookingID:      view.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      "CFB2NN6VB1",
		IdempotencyKey: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("payment was accepted for an expired booking")
	}
	// The wording matters as much as the code here: the payer needs to be told
	// to book again, not to wait.
	if code := apiCode(err); code != httpx.CodeHoldExpired {
		t.Errorf("error code = %s (%v), want %s", code, err, httpx.CodeHoldExpired)
	}
	if n := countRows(t, store, "payments"); n != 0 {
		t.Errorf("payments rows = %d, want 0", n)
	}
	if n := countRows(t, store, "payment_verification_jobs"); n != 0 {
		t.Errorf("payment_verification_jobs rows = %d, want 0", n)
	}
}

// TestSubmitBeatsTheSweepInsideTheGraceWindow documents how the two halves of
// Fix 2 fit together: submission is gated on the booking's status, not on the
// clock, so a payer whose receipt arrives in the seconds between the hold lapsing
// and the sweep noticing is accepted -- and the sweep then leaves that booking
// alone because a verification job is live for it. Refusing here would turn a
// fifteen-second scheduling detail into a rejected payment that has already left
// somebody's account.
func TestSubmitBeatsTheSweepInsideTheGraceWindow(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	user, view := heldBooking(t, store, 3)
	// Lapsed, but the sweep has not run yet.
	testsupport.BackdateHold(t, store, view.ID)

	if _, replayed, err := svc.Submit(ctx, payment.SubmitInput{
		BookingID:      view.ID,
		UserID:         user.ID,
		Method:         payment.MethodTelebirr,
		Reference:      "CFB5RR2KD8",
		IdempotencyKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("submit inside the grace window: %v", err)
	} else if replayed {
		t.Error("first submit reported a replay")
	}
	if got := bookingStatus(t, store, view.ID); got != booking.StatusVerifying {
		t.Fatalf("booking status = %q, want %q", got, booking.StatusVerifying)
	}

	expired, err := newSweeper(store).ExpireOnce(ctx)
	if err != nil {
		t.Fatalf("expire once: %v", err)
	}
	if expired != 0 {
		t.Errorf("sweep expired %d bookings whose payment was already in flight, want 0", expired)
	}
	if got := bookingStatus(t, store, view.ID); got != booking.StatusVerifying {
		t.Errorf("booking status = %q after the sweep, want %q", got, booking.StatusVerifying)
	}
}

// TestSubmitStoresTheSuffixFromThePastedReceipt: a legacy CBE receipt URL carries
// both the reference and the account suffix, and the embedded one wins over
// whatever was typed into the form -- it came from the bank, the field came from a
// person. The stored suffix is what the verifier hands to CBE's legacy receipt
// service, so a wrong one turns a good payment into a "not found" for a human to
// sort out.
func TestSubmitStoresTheSuffixFromThePastedReceipt(t *testing.T) {
	store := testsupport.Store(t)
	svc := newPaymentService(store)
	ctx := context.Background()

	const receipt = "https://apps.cbe.com.et:100/?id=FT25062PP5ZB12345678"
	submission, err := payment.Canonicalize(payment.MethodCBE, receipt, "99999999")
	if err != nil {
		t.Fatalf("canonicalize receipt URL: %v", err)
	}
	// The URL is not what gets stored: a payer who pastes the link and one who
	// types the bare code must collide on the anti-replay constraint (FR17).
	if submission.Reference != "FT25062PP5ZB" {
		t.Errorf("reference = %q, want %q", submission.Reference, "FT25062PP5ZB")
	}
	if submission.AccountSuffix != "12345678" {
		t.Fatalf("account suffix = %q, want the one embedded in the receipt", submission.AccountSuffix)
	}

	user, view := heldBooking(t, store, 2)
	created, _, err := svc.Submit(ctx, payment.SubmitInput{
		BookingID:      view.ID,
		UserID:         user.ID,
		Method:         payment.MethodCBE,
		Reference:      submission.Reference,
		AccountSuffix:  submission.AccountSuffix,
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("submit CBE payment: %v", err)
	}
	if created.AccountSuffix == nil {
		t.Fatal("stored account_suffix is null, so the legacy receipt lookup has nothing to send")
	}
	if *created.AccountSuffix != "12345678" {
		t.Errorf("stored account_suffix = %q, want %q", *created.AccountSuffix, "12345678")
	}
	if created.TransactionReference != "FT25062PP5ZB" {
		t.Errorf("stored reference = %q, want %q", created.TransactionReference, "FT25062PP5ZB")
	}
}
