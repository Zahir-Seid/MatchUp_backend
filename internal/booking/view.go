package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// View is a booking as the app renders it (PRD FR20).
type View struct {
	ID            uuid.UUID `json:"id"`
	ReferenceCode string    `json:"reference_code"`
	BookingType   string    `json:"booking_type"`
	Status        string    `json:"status"`
	SpotCount     int32     `json:"spot_count"`
	WeekCount     int32     `json:"week_count"`

	PricePerPlayer string `json:"price_per_player"`
	Subtotal       string `json:"subtotal"`
	Discount       string `json:"discount"`
	Total          string `json:"total"`
	TotalSantim    int64  `json:"total_santim"`

	// HoldExpiresAt and HoldSecondsRemaining drive the countdown on the payment
	// screen (PRD FR12). Both are absent once the hold no longer applies.
	HoldExpiresAt        *time.Time `json:"hold_expires_at,omitempty"`
	HoldSecondsRemaining *int64     `json:"hold_seconds_remaining,omitempty"`

	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`

	Games        []BookedGame  `json:"games"`
	Participants []Participant `json:"participants,omitempty"`
	Payments     []PaymentView `json:"payments,omitempty"`
}

// BookedGame is one covered session inside a booking.
type BookedGame struct {
	WeekIndex int32            `json:"week_index"`
	Game      catalog.GameView `json:"game"`
}

// PaymentView is a submitted payment as the owner may see it. The raw verifier
// response and the internal resolution notes are deliberately absent.
type PaymentView struct {
	ID                   uuid.UUID  `json:"id"`
	Method               string     `json:"method"`
	TransactionReference string     `json:"transaction_reference"`
	Status               string     `json:"status"`
	ExpectedAmount       string     `json:"expected_amount"`
	VerifiedAmount       *string    `json:"verified_amount,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	ResolvedAt           *time.Time `json:"resolved_at,omitempty"`
}

// Get returns one of the caller's own bookings in full.
func (s *Service) Get(ctx context.Context, userID, bookingID uuid.UUID) (*View, error) {
	booking, err := s.store.GetBookingForUser(ctx, db.GetBookingForUserParams{
		ID:     bookingID,
		UserID: userID,
	})
	if err != nil {
		if db.IsNoRows(err) {
			// Not "forbidden": whether the id exists at all is not the
			// caller's business.
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get booking: %w", err)
	}
	return s.viewFor(ctx, booking)
}

// List returns the caller's bookings, newest first (PRD FR20).
func (s *Service) List(ctx context.Context, userID uuid.UUID, status *string, limit, offset int32) ([]View, error) {
	bookings, err := s.store.ListBookingsForUser(ctx, db.ListBookingsForUserParams{
		UserID: userID,
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	if len(bookings) == 0 {
		return []View{}, nil
	}

	ids := make([]uuid.UUID, 0, len(bookings))
	for _, b := range bookings {
		ids = append(ids, b.ID)
	}
	// One batched lookup instead of a query per booking.
	gameRows, err := s.store.ListBookingGamesForBookings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list booking games: %w", err)
	}
	byBooking := make(map[uuid.UUID][]BookedGame, len(bookings))
	for _, row := range gameRows {
		byBooking[row.BookingID] = append(byBooking[row.BookingID], BookedGame{
			WeekIndex: row.WeekIndex,
			Game:      catalog.ToGameView(row.Game, row.Field),
		})
	}

	views := make([]View, 0, len(bookings))
	for _, b := range bookings {
		view := toView(b, s.now())
		view.Games = byBooking[b.ID]
		if view.Games == nil {
			view.Games = []BookedGame{}
		}
		views = append(views, view)
	}
	return views, nil
}

// viewFor assembles the full detail view for one booking.
func (s *Service) viewFor(ctx context.Context, booking db.Booking) (*View, error) {
	view := toView(booking, s.now())

	gameRows, err := s.store.ListBookingGamesForBookings(ctx, []uuid.UUID{booking.ID})
	if err != nil {
		return nil, fmt.Errorf("list booking games: %w", err)
	}
	view.Games = make([]BookedGame, 0, len(gameRows))
	for _, row := range gameRows {
		view.Games = append(view.Games, BookedGame{
			WeekIndex: row.WeekIndex,
			Game:      catalog.ToGameView(row.Game, row.Field),
		})
	}

	participants, err := s.store.ListBookingParticipants(ctx, booking.ID)
	if err != nil {
		return nil, fmt.Errorf("list booking participants: %w", err)
	}
	view.Participants = make([]Participant, 0, len(participants))
	for _, p := range participants {
		view.Participants = append(view.Participants, Participant{
			SlotIndex: p.SlotIndex,
			FullName:  p.FullName,
		})
	}

	payments, err := s.store.ListPaymentsForBooking(ctx, booking.ID)
	if err != nil {
		return nil, fmt.Errorf("list booking payments: %w", err)
	}
	view.Payments = make([]PaymentView, 0, len(payments))
	for _, p := range payments {
		view.Payments = append(view.Payments, toPaymentView(p))
	}

	return &view, nil
}

// toView projects the booking row itself, deriving the countdown from now.
func toView(b db.Booking, now time.Time) View {
	view := View{
		ID:             b.ID,
		ReferenceCode:  b.ReferenceCode,
		BookingType:    b.BookingType,
		Status:         b.Status,
		SpotCount:      b.SpotCount,
		WeekCount:      b.WeekCount,
		PricePerPlayer: money.Santim(b.PricePerPlayerSantim).Format(),
		Subtotal:       money.Santim(b.SubtotalSantim).Format(),
		Discount:       money.Santim(b.DiscountSantim).Format(),
		Total:          money.Santim(b.TotalSantim).Format(),
		TotalSantim:    b.TotalSantim,
		ConfirmedAt:    b.ConfirmedAt,
		CancelledAt:    b.CancelledAt,
		CreatedAt:      b.CreatedAt,
		HoldExpiresAt:  b.HoldExpiresAt,
	}
	// The countdown is only meaningful while the hold is still running. An
	// already-lapsed hold reports zero rather than a negative number, since the
	// sweep may not have reached the row yet.
	if b.Status == StatusPendingPayment && b.HoldExpiresAt != nil {
		seconds := int64(b.HoldExpiresAt.Sub(now).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		view.HoldSecondsRemaining = &seconds
	}
	return view
}

func toPaymentView(p db.Payment) PaymentView {
	view := PaymentView{
		ID:                   p.ID,
		Method:               p.Method,
		TransactionReference: p.TransactionReference,
		Status:               p.Status,
		ExpectedAmount:       money.Santim(p.ExpectedAmountSantim).Format(),
		CreatedAt:            p.CreatedAt,
		ResolvedAt:           p.ResolvedAt,
	}
	if p.VerifiedAmountSantim != nil {
		formatted := money.Santim(*p.VerifiedAmountSantim).Format()
		view.VerifiedAmount = &formatted
	}
	return view
}
