package booking

import (
	"fmt"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// Booking types (PRD FR8).
const (
	TypeSingle    = "single"
	TypeMonthly4W = "monthly_4wk"
)

// DiscountPolicy is the monthly-plan discount as the dashboard currently
// configures it. It is read per quote rather than captured at boot, so an admin
// edit applies to the next quote without a restart.
type DiscountPolicy struct {
	// BPS is the discount in basis points of the subtotal (1250 = 12.5%).
	BPS int32
	// Label is the phrase the app shows beside the deduction.
	Label string
}

// DefaultDiscountPolicy is the launch configuration, identical to what the
// pricing code hardcoded before the dashboard owned it: half of one week's
// charge on a four-week plan (12.5% of the subtotal). It is used when the
// settings row is missing or unreadable so pricing keeps working.
var DefaultDiscountPolicy = DiscountPolicy{
	BPS:   1250,
	Label: "Half a session free on the monthly plan",
}

// Quote is the price breakdown shown before confirming (PRD FR11) and the
// snapshot stored on the booking row. Storing the figures rather than
// recomputing them means a later price edit never changes what an existing
// booking owes.
type Quote struct {
	BookingType    string `json:"booking_type"`
	SpotCount      int32  `json:"spot_count"`
	WeekCount      int32  `json:"week_count"`
	PricePerPlayer string `json:"price_per_player"`
	Subtotal       string `json:"subtotal"`
	Discount       string `json:"discount"`
	Total          string `json:"total"`

	PricePerPlayerSantim int64 `json:"price_per_player_santim"`
	SubtotalSantim       int64 `json:"subtotal_santim"`
	DiscountSantim       int64 `json:"discount_santim"`
	TotalSantim          int64 `json:"total_santim"`

	// DiscountLabel explains the deduction in the app's own words.
	DiscountLabel string `json:"discount_label,omitempty"`
	// Weeks lists what the money buys, one entry per covered game.
	Weeks []QuoteWeek `json:"weeks"`
} //	@name	Quote

// QuoteWeek is one covered game inside a quote.
type QuoteWeek struct {
	WeekIndex      int32  `json:"week_index"`
	GameID         string `json:"game_id"`
	StartsAt       string `json:"starts_at"`
	PricePerPlayer string `json:"price_per_player"`
	LineTotal      string `json:"line_total"`
	SpotsRemaining int32  `json:"spots_remaining"`
} //	@name	QuoteWeek

// buildQuote prices a booking across the games it will cover.
//
// Subtotal sums each game's own price rather than multiplying the first game's
// price by the week count: games in a recurring series normally carry identical
// prices, but nothing in the schema guarantees it, and charging for a price the
// customer was not shown would be wrong in either direction.
//
// The monthly discount comes from the dashboard's policy, applied to the
// subtotal in basis points. The launch value is half of one week's charge —
// "half a session off" as the manual MVP offered it — which for a four-week
// plan is 12.5% of the subtotal regardless of how the four prices differ.
func buildQuote(bookingType string, spotCount int32, games []db.Game, policy DiscountPolicy) (*Quote, error) {
	if len(games) == 0 {
		return nil, fmt.Errorf("buildQuote: no games to price")
	}
	if spotCount <= 0 {
		return nil, fmt.Errorf("buildQuote: spot count must be positive, got %d", spotCount)
	}

	weekCount := int32(len(games))
	var subtotal money.Santim
	weeks := make([]QuoteWeek, 0, len(games))

	for i, game := range games {
		line := money.Santim(game.PricePerPlayerSantim).Mul(int(spotCount))
		subtotal += line
		weeks = append(weeks, QuoteWeek{
			WeekIndex:      int32(i + 1),
			GameID:         game.ID.String(),
			StartsAt:       game.StartsAt.Format(timeLayout),
			PricePerPlayer: money.Santim(game.PricePerPlayerSantim).Format(),
			LineTotal:      line.Format(),
			SpotsRemaining: game.SpotsRemaining,
		})
	}

	var discount money.Santim
	var label string
	if bookingType == TypeMonthly4W && policy.BPS > 0 {
		discount = subtotal * money.Santim(policy.BPS) / 10000
		// The label explains a deduction; with none, a label would lie.
		if discount > 0 {
			label = policy.Label
		}
	}

	// The bookings table enforces discount < subtotal; a configuration that
	// produced a free booking would be a pricing bug, not a promotion.
	if discount >= subtotal {
		return nil, fmt.Errorf("buildQuote: discount %s is not less than subtotal %s", discount, subtotal)
	}
	total := subtotal - discount

	// The stored per-player snapshot is the first covered game's price, which
	// is the figure the customer saw on the game they picked.
	perPlayer := money.Santim(games[0].PricePerPlayerSantim)

	return &Quote{
		BookingType:          bookingType,
		SpotCount:            spotCount,
		WeekCount:            weekCount,
		PricePerPlayer:       perPlayer.Format(),
		Subtotal:             subtotal.Format(),
		Discount:             discount.Format(),
		Total:                total.Format(),
		PricePerPlayerSantim: int64(perPlayer),
		SubtotalSantim:       int64(subtotal),
		DiscountSantim:       int64(discount),
		TotalSantim:          int64(total),
		DiscountLabel:        label,
		Weeks:                weeks,
	}, nil
}
