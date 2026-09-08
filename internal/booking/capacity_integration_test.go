//go:build integration

// Capacity tests.
//
// These need a real Postgres because the invariant under test lives in SQL:
// fn_reserve_game_spots is what decides which of two simultaneous bookings gets
// the last spot, and a test against a fake store would only prove the fake
// behaves. Both tests here assert the same two things -- one winner, and no
// trace of the loser -- because a partial reservation is the failure that would
// quietly sell a spot twice.
package booking_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

// TestCreateCannotOversellTheLastSpot fires eight bookings at a game with one
// spot, simultaneously, and requires exactly one to succeed (PRD FR14).
func TestCreateCannotOversellTheLastSpot(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	ctx := context.Background()

	field := testsupport.Field(t, store)
	game := testsupport.Game(t, store, testsupport.GameSpec{FieldID: field.ID, Capacity: 1})

	const contenders = 8
	// Users are created up front: testsupport calls t.Fatalf, which may not be
	// called from a goroutine.
	users := make([]db.User, contenders)
	for i := range users {
		users[i] = testsupport.User(t, store)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	views := make([]*booking.View, contenders)
	errs := make([]error, contenders)

	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			views[i], errs[i] = svc.Create(ctx, booking.CreateInput{
				UserID:      users[i].ID,
				GameID:      game.ID,
				BookingType: booking.TypeSingle,
				SpotCount:   1,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := range contenders {
		if errs[i] != nil {
			// Every loser must be told it was capacity, not a fault: the app
			// shows "this game just filled up" on that code alone.
			if code := apiCode(errs[i]); code != httpx.CodeInsufficientCapacity {
				t.Errorf("contender %d failed with %s (%v), want %s",
					i, code, errs[i], httpx.CodeInsufficientCapacity)
			}
			continue
		}
		winners++
		if views[i].Status != booking.StatusPendingPayment {
			t.Errorf("winning booking status = %q, want %q", views[i].Status, booking.StatusPendingPayment)
		}
		if views[i].HoldExpiresAt == nil {
			t.Error("winning booking has no hold expiry, so nothing would ever release its spot")
		}
	}

	if winners != 1 {
		t.Fatalf("%d of %d bookings succeeded for a single spot, want exactly 1", winners, contenders)
	}
	if left := testsupport.SpotsRemaining(t, store, game.ID); left != 0 {
		t.Errorf("spots_remaining = %d after the race, want 0", left)
	}
	if n := countRows(t, store, "bookings"); n != 1 {
		t.Errorf("bookings rows = %d, want 1", n)
	}
	if n := countRows(t, store, "booking_games"); n != 1 {
		t.Errorf("booking_games rows = %d, want 1", n)
	}
}

// TestCreateMonthlyRollsBackWhenAWeekIsShort proves Fix 1's atomicity: a monthly
// booking that cannot get every week gets none of them.
//
// The interleaving is arranged rather than raced, because the failure has to
// happen *inside* the transaction to be interesting. A fixture transaction takes
// the last spots of week 3 and holds them uncommitted, so the booking's cheap
// up-front check still sees them free and the refusal lands after weeks 1 and 2
// are already reserved -- exactly the window where a non-transactional
// implementation would strand two weeks of spots.
func TestCreateMonthlyRollsBackWhenAWeekIsShort(t *testing.T) {
	store := testsupport.Store(t)
	svc := newBookingService(store)
	ctx := context.Background()

	const capacity = 4
	const wanted = 2
	// shortWeek is 1-based, matching booking_games.week_index and the week
	// number the error reports to the customer.
	const shortWeek = 3

	field := testsupport.Field(t, store)
	_, games := testsupport.Series(t, store,
		testsupport.GameSpec{FieldID: field.ID, Capacity: capacity}, testWeeks)
	user := testsupport.User(t, store)

	// Committed: week 3 is down to exactly the two spots the booking will want.
	// Taken through the same guarded function the booking path uses, so the
	// fixture cannot put the row in a state the application could not reach.
	took, err := store.ReserveGameSpots(ctx, db.ReserveGameSpotsParams{
		GameID: games[shortWeek-1].ID,
		Amount: capacity - wanted,
	})
	if err != nil {
		t.Fatalf("fixture reserve on week %d: %v", shortWeek, err)
	}
	if !took {
		t.Fatalf("fixture could not reserve %d spots on week %d", capacity-wanted, shortWeek)
	}

	// Uncommitted: the last two spots of week 3, held on their own connection.
	holder, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below; this is the failure path
	var held bool
	if err := holder.QueryRow(ctx,
		`SELECT fn_reserve_game_spots($1, $2::int)`, games[shortWeek-1].ID, int32(wanted)).Scan(&held); err != nil {
		t.Fatalf("holder reserve: %v", err)
	}
	if !held {
		t.Fatal("holder could not take the last spots of the short week")
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Create(ctx, booking.CreateInput{
			UserID:      user.ID,
			GameID:      games[0].ID,
			BookingType: booking.TypeMonthly4W,
			SpotCount:   wanted,
		})
		done <- err
	}()

	// The booking is now blocked on week 3's row. Releasing the fixture makes
	// the conditional update re-evaluate against the committed value and match
	// nothing, which is the refusal the rest of the test is about.
	waitForRowLockContention(t, store)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder transaction: %v", err)
	}

	err = <-done
	if err == nil {
		t.Fatal("monthly booking succeeded although one of its weeks was full")
	}
	apiErr := httpx.AsError(err)
	if apiErr.Code != httpx.CodeInsufficientCapacity {
		t.Errorf("error code = %s (%v), want %s", apiErr.Code, err, httpx.CodeInsufficientCapacity)
	}
	// Naming the week is the point of the message: "the monthly plan failed" is
	// not something a customer can act on, "week 3 is full" is.
	if got := apiErr.Fields["week_index"]; got != "3" {
		t.Errorf("week_index field = %q, want %q", got, "3")
	}

	// Weeks 1 and 2 were reserved before the refusal and must have been handed
	// back; week 3 keeps what the fixture took.
	want := []int32{capacity, capacity, 0, capacity}
	for i, game := range games {
		if left := testsupport.SpotsRemaining(t, store, game.ID); left != want[i] {
			t.Errorf("week %d spots_remaining = %d, want %d", i+1, left, want[i])
		}
	}
	if n := countRows(t, store, "bookings"); n != 0 {
		t.Errorf("bookings rows = %d, want 0: a half-reserved monthly booking was recorded", n)
	}
	if n := countRows(t, store, "booking_games"); n != 0 {
		t.Errorf("booking_games rows = %d, want 0", n)
	}
}
