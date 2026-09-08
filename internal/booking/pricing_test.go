package booking

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// game builds a priced game for the pricing tests.
func game(priceSantim int64, remaining int32) db.Game {
	return db.Game{
		ID:                   uuid.New(),
		StartsAt:             time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC),
		PricePerPlayerSantim: priceSantim,
		Capacity:             10,
		SpotsRemaining:       remaining,
		Status:               "scheduled",
	}
}

func repeat(g db.Game, n int) []db.Game {
	games := make([]db.Game, 0, n)
	for range n {
		copy := g
		copy.ID = uuid.New()
		games = append(games, copy)
	}
	return games
}

func TestBuildQuoteSingle(t *testing.T) {
	// 150 ETB per player, two spots, one session: no discount.
	q, err := buildQuote(TypeSingle, 2, []db.Game{game(15000, 10)})
	if err != nil {
		t.Fatalf("buildQuote: %v", err)
	}
	if q.SubtotalSantim != 30000 {
		t.Errorf("subtotal = %d, want 30000", q.SubtotalSantim)
	}
	if q.DiscountSantim != 0 {
		t.Errorf("discount = %d, want 0", q.DiscountSantim)
	}
	if q.TotalSantim != 30000 {
		t.Errorf("total = %d, want 30000", q.TotalSantim)
	}
	if q.WeekCount != 1 {
		t.Errorf("week count = %d, want 1", q.WeekCount)
	}
	if len(q.Weeks) != 1 || q.Weeks[0].WeekIndex != 1 {
		t.Errorf("weeks = %+v, want one entry indexed 1", q.Weeks)
	}
	if q.DiscountLabel != "" {
		t.Errorf("discount label = %q, want empty for a single booking", q.DiscountLabel)
	}
}

func TestBuildQuoteMonthlyMatchesSpecFormula(t *testing.T) {
	// The relayed spec: total = (4 x price x spots) - (0.5 x price x spots).
	const price = 15000
	for _, spots := range []int32{1, 2, 3, 7} {
		q, err := buildQuote(TypeMonthly4W, spots, repeat(game(price, 10), 4))
		if err != nil {
			t.Fatalf("buildQuote(%d spots): %v", spots, err)
		}
		wantSubtotal := int64(4) * price * int64(spots)
		wantDiscount := price * int64(spots) / 2
		if q.SubtotalSantim != wantSubtotal {
			t.Errorf("spots=%d subtotal = %d, want %d", spots, q.SubtotalSantim, wantSubtotal)
		}
		if q.DiscountSantim != wantDiscount {
			t.Errorf("spots=%d discount = %d, want %d", spots, q.DiscountSantim, wantDiscount)
		}
		if q.TotalSantim != wantSubtotal-wantDiscount {
			t.Errorf("spots=%d total = %d, want %d", spots, q.TotalSantim, wantSubtotal-wantDiscount)
		}
	}
}

func TestBuildQuoteMonthlyWeekIndexesAreSequential(t *testing.T) {
	q, err := buildQuote(TypeMonthly4W, 1, repeat(game(12000, 10), 4))
	if err != nil {
		t.Fatalf("buildQuote: %v", err)
	}
	if len(q.Weeks) != 4 {
		t.Fatalf("weeks = %d, want 4", len(q.Weeks))
	}
	for i, week := range q.Weeks {
		if week.WeekIndex != int32(i+1) {
			t.Errorf("weeks[%d].WeekIndex = %d, want %d", i, week.WeekIndex, i+1)
		}
	}
	if q.DiscountLabel == "" {
		t.Error("monthly quote should explain the discount")
	}
}

func TestBuildQuoteMonthlyWithUnevenPrices(t *testing.T) {
	// Prices are normally uniform across a series, but the schema allows an
	// admin to edit one week. The subtotal must follow the actual prices, and
	// the discount is half the average week.
	games := []db.Game{game(10000, 10), game(10000, 10), game(20000, 10), game(20000, 10)}
	q, err := buildQuote(TypeMonthly4W, 1, games)
	if err != nil {
		t.Fatalf("buildQuote: %v", err)
	}
	if q.SubtotalSantim != 60000 {
		t.Errorf("subtotal = %d, want 60000", q.SubtotalSantim)
	}
	if q.DiscountSantim != 7500 {
		t.Errorf("discount = %d, want 7500 (half of the 15000 average week)", q.DiscountSantim)
	}
	// The stored per-player snapshot is the price of the game the user picked.
	if q.PricePerPlayerSantim != 10000 {
		t.Errorf("price per player = %d, want 10000", q.PricePerPlayerSantim)
	}
}

func TestBuildQuoteRejectsBadInput(t *testing.T) {
	if _, err := buildQuote(TypeSingle, 1, nil); err == nil {
		t.Error("expected an error with no games")
	}
	if _, err := buildQuote(TypeSingle, 0, []db.Game{game(15000, 10)}); err == nil {
		t.Error("expected an error with zero spots")
	}
	// A free game would make the monthly discount equal the subtotal, which the
	// bookings CHECK constraint forbids.
	if _, err := buildQuote(TypeMonthly4W, 1, repeat(game(0, 10), 4)); err == nil {
		t.Error("expected an error when the discount is not less than the subtotal")
	}
}

func TestValidateParticipants(t *testing.T) {
	tests := []struct {
		name         string
		participants []Participant
		spotCount    int32
		wantErr      bool
	}{
		{"no names is valid", nil, 3, false},
		{"sparse names are valid", []Participant{{SlotIndex: 1, FullName: "A"}, {SlotIndex: 4, FullName: "D"}}, 4, false},
		{"all named", []Participant{{1, "A"}, {2, "B"}}, 2, false},
		{"more names than spots", []Participant{{1, "A"}, {2, "B"}}, 1, true},
		{"slot above range", []Participant{{5, "A"}}, 4, true},
		{"slot below range", []Participant{{0, "A"}}, 4, true},
		{"duplicate slot", []Participant{{1, "A"}, {1, "B"}}, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateParticipants(tt.participants, tt.spotCount)
			if tt.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewReferenceCode(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for range 500 {
		code, err := newReferenceCode()
		if err != nil {
			t.Fatalf("newReferenceCode: %v", err)
		}
		if len(code) != len(referencePrefix)+referenceBodyLength {
			t.Fatalf("code %q has length %d, want %d", code, len(code), len(referencePrefix)+referenceBodyLength)
		}
		if code[:len(referencePrefix)] != referencePrefix {
			t.Fatalf("code %q is missing the %q prefix", code, referencePrefix)
		}
		for _, r := range code[len(referencePrefix):] {
			if !strings.ContainsRune(referenceAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside the alphabet", code, r)
			}
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("duplicate reference code %q within 500 draws", code)
		}
		seen[code] = struct{}{}
	}
}
