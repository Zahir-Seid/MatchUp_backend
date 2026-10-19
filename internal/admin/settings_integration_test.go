//go:build integration

// The settings loop: a dashboard edit changes what the pricing path quotes,
// without touching bookings that already hold their figures.
//
// It lives with the admin package because the edit is the admin API's, but the
// assertion that matters is the public booking service reading the same row.
// The two halves meet at the database, which is exactly what a unit test of
// either side would fake.
package admin_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func TestBookingSettingsDrivePricing(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	adminSvc := admin.NewService(admin.Options{
		Store:  store,
		Audit:  audit.NewRecorder(store, log),
		Logger: log,
	})
	bookingSvc := booking.NewService(booking.Options{
		Store:        store,
		Logger:       log,
		HoldDuration: 5 * time.Minute,
		WeekCount:    4,
	})

	// The harness truncates every table, including the settings row the
	// migration seeds, so recreate that row the way the migration does. The
	// rest of the suite runs against the same truncated state, which also
	// exercises the booking service's built-in fallback.
	if _, err := store.Pool().Exec(ctx, `INSERT INTO booking_settings (id) VALUES (true)`); err != nil {
		t.Fatalf("seed booking settings: %v", err)
	}

	field := testsupport.Field(t, store)
	_, games := testsupport.Series(t, store, testsupport.GameSpec{FieldID: field.ID, PriceSantim: 15000}, 4)
	anchor := games[0]

	// The seeded rate is the half-session discount the code used to hardcode:
	// four weeks at 150 ETB for one spot is 60000 santim, and 12.5% is 7500.
	q, err := bookingSvc.Quote(ctx, anchor.ID, booking.TypeMonthly4W, 1)
	if err != nil {
		t.Fatalf("quote with the seeded settings: %v", err)
	}
	if q.SubtotalSantim != 60000 || q.DiscountSantim != 7500 {
		t.Fatalf("seeded quote subtotal = %d discount = %d, want 60000 and 7500",
			q.SubtotalSantim, q.DiscountSantim)
	}

	actor := testsupport.User(t, store)
	rate := int32(2500)
	updated, err := adminSvc.UpdateBookingSettings(ctx, actor.ID,
		admin.BookingSettingsPatch{MonthlyDiscountBPS: &rate}, "integration test")
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if updated.MonthlyDiscountBPS != 2500 {
		t.Errorf("updated bps = %d, want 2500", updated.MonthlyDiscountBPS)
	}
	if updated.UpdatedBy == nil || *updated.UpdatedBy != actor.ID {
		t.Errorf("updated_by = %v, want the acting admin", updated.UpdatedBy)
	}

	// A fresh quote sees the edit without a restart.
	q, err = bookingSvc.Quote(ctx, anchor.ID, booking.TypeMonthly4W, 1)
	if err != nil {
		t.Fatalf("quote after the edit: %v", err)
	}
	if q.DiscountSantim != 15000 {
		t.Errorf("discount after the edit = %d, want 15000 (25%% of 60000)", q.DiscountSantim)
	}
	if q.DiscountLabel == "" {
		t.Error("a discounted quote should carry the configured label")
	}

	// Book at the 25% rate, then turn the discount off: the next quote
	// changes and the existing booking does not.
	user := testsupport.User(t, store)
	view, err := bookingSvc.Create(ctx, booking.CreateInput{
		UserID:      user.ID,
		GameID:      anchor.ID,
		BookingType: booking.TypeMonthly4W,
		SpotCount:   1,
	})
	if err != nil {
		t.Fatalf("create monthly booking: %v", err)
	}

	zero := int32(0)
	if _, err := adminSvc.UpdateBookingSettings(ctx, actor.ID,
		admin.BookingSettingsPatch{MonthlyDiscountBPS: &zero}, "promo over"); err != nil {
		t.Fatalf("disable the discount: %v", err)
	}

	q, err = bookingSvc.Quote(ctx, anchor.ID, booking.TypeMonthly4W, 1)
	if err != nil {
		t.Fatalf("quote with the discount disabled: %v", err)
	}
	if q.DiscountSantim != 0 || q.DiscountLabel != "" {
		t.Errorf("disabled quote discount = %d label = %q, want none", q.DiscountSantim, q.DiscountLabel)
	}

	var snapshot int64
	if err := store.Pool().QueryRow(ctx,
		`SELECT discount_santim FROM bookings WHERE id = $1`, view.ID).Scan(&snapshot); err != nil {
		t.Fatalf("read the booking snapshot: %v", err)
	}
	if snapshot != 15000 {
		t.Errorf("existing booking discount = %d, want its 15000 snapshot", snapshot)
	}

	// Both edits are on the record.
	var entries int64
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = $1`,
		audit.ActionBookingSettingsUpdated).Scan(&entries); err != nil {
		t.Fatalf("count audit entries: %v", err)
	}
	if entries != 2 {
		t.Errorf("audit entries = %d, want 2", entries)
	}
}
