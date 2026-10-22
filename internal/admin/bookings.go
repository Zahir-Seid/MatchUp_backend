package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/booking"
	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
)

// BookingSummary is one row of the admin booking list. It is deliberately
// flatter than the customer's view: the list is scanned, filtered and searched,
// so it carries the identifying and financial columns and leaves the games,
// participants and payments to the detail endpoint.
type BookingSummary struct {
	ID            uuid.UUID `json:"id"`
	ReferenceCode string    `json:"reference_code"`
	BookingType   string    `json:"booking_type"`
	Status        string    `json:"status"`
	SpotCount     int32     `json:"spot_count"`
	WeekCount     int32     `json:"week_count"`
	Total         string    `json:"total"`
	TotalSantim   int64     `json:"total_santim"`
	UserID        uuid.UUID `json:"user_id"`
	UserName      string    `json:"user_name"`
	UserEmail     string    `json:"user_email"`
	// PaymentCount tells an admin at a glance whether the customer has
	// submitted anything at all, which is the difference between chasing them
	// and chasing the bank.
	PaymentCount  int64      `json:"payment_count"`
	HoldExpiresAt *time.Time `json:"hold_expires_at,omitempty"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt   *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
} //	@name	BookingSummary

// BookingFilter narrows the admin booking list.
type BookingFilter struct {
	Status *string
	// Search matches the reference code, the customer's email or their name.
	Search *string
	Limit  int32
	Offset int32
}

// ListBookings returns bookings newest first (PRD FR27).
func (s *Service) ListBookings(ctx context.Context, f BookingFilter) (Page[BookingSummary], error) {
	rows, err := s.store.ListBookingsForAdmin(ctx, db.ListBookingsForAdminParams{
		Limit:  f.Limit,
		Offset: f.Offset,
		Status: f.Status,
		Search: f.Search,
	})
	if err != nil {
		return Page[BookingSummary]{}, fmt.Errorf("list bookings for admin: %w", err)
	}
	total, err := s.store.CountBookingsForAdmin(ctx, db.CountBookingsForAdminParams{
		Status: f.Status,
		Search: f.Search,
	})
	if err != nil {
		return Page[BookingSummary]{}, fmt.Errorf("count bookings for admin: %w", err)
	}

	items := make([]BookingSummary, 0, len(rows))
	for _, row := range rows {
		b := row.Booking
		items = append(items, BookingSummary{
			ID:            b.ID,
			ReferenceCode: b.ReferenceCode,
			BookingType:   b.BookingType,
			Status:        b.Status,
			SpotCount:     b.SpotCount,
			WeekCount:     b.WeekCount,
			Total:         money.Santim(b.TotalSantim).Format(),
			TotalSantim:   b.TotalSantim,
			UserID:        b.UserID,
			UserName:      row.UserName,
			UserEmail:     row.UserEmail,
			PaymentCount:  row.PaymentCount,
			HoldExpiresAt: b.HoldExpiresAt,
			ConfirmedAt:   b.ConfirmedAt,
			CancelledAt:   b.CancelledAt,
			CreatedAt:     b.CreatedAt,
		})
	}
	return newPage(items, total, f.Limit, f.Offset), nil
}

// BookingDetail is one booking in full, as the dashboard shows it. It differs
// from the customer's own view in what it is allowed to reveal: the raw
// verifier envelope and the resolution notes are here, because deciding a
// disputed payment is impossible without them.
type BookingDetail struct {
	BookingSummary
	UserPhone      *string              `json:"user_phone,omitempty"`
	PricePerPlayer string               `json:"price_per_player"`
	Subtotal       string               `json:"subtotal"`
	Discount       string               `json:"discount"`
	Games          []BookedGame         `json:"games"`
	Participants   []BookingParticipant `json:"participants"`
	Payments       []AdminPaymentView   `json:"payments"`
} //	@name	BookingDetail

// BookedGame is one covered session of a booking.
type BookedGame struct {
	WeekIndex int32            `json:"week_index"`
	Game      catalog.GameView `json:"game"`
} //	@name	BookedGame

// BookingParticipant is one named player on a booking (PRD FR10).
type BookingParticipant struct {
	SlotIndex int32  `json:"slot_index"`
	FullName  string `json:"full_name"`
} //	@name	BookingParticipant

// AdminPaymentView is a payment with the internal detail an admin may see.
type AdminPaymentView struct {
	ID                   uuid.UUID `json:"id"`
	Method               string    `json:"method"`
	TransactionReference string    `json:"transaction_reference"`
	AccountSuffix        *string   `json:"account_suffix,omitempty"`
	Status               string    `json:"status"`
	ExpectedAmount       string    `json:"expected_amount"`
	VerifiedAmount       *string   `json:"verified_amount,omitempty"`
	// IdempotencyKey is included because a duplicate submission investigation
	// starts here: two payments with the same key are one retried request.
	IdempotencyKey   string          `json:"idempotency_key"`
	VerifierResult   json.RawMessage `json:"verifier_result" swaggertype:"object"`
	ResolvedBy       *uuid.UUID      `json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	ResolutionReason *string         `json:"resolution_reason,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
} //	@name	AdminPaymentView

// GetBooking assembles the full admin view of one booking.
func (s *Service) GetBooking(ctx context.Context, id uuid.UUID) (*BookingDetail, error) {
	row, err := s.store.GetBookingWithUser(ctx, id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get booking with user: %w", err)
	}
	b := row.Booking

	detail := &BookingDetail{
		BookingSummary: BookingSummary{
			ID:            b.ID,
			ReferenceCode: b.ReferenceCode,
			BookingType:   b.BookingType,
			Status:        b.Status,
			SpotCount:     b.SpotCount,
			WeekCount:     b.WeekCount,
			Total:         money.Santim(b.TotalSantim).Format(),
			TotalSantim:   b.TotalSantim,
			UserID:        b.UserID,
			UserName:      row.UserName,
			UserEmail:     row.UserEmail,
			HoldExpiresAt: b.HoldExpiresAt,
			ConfirmedAt:   b.ConfirmedAt,
			CancelledAt:   b.CancelledAt,
			CreatedAt:     b.CreatedAt,
		},
		UserPhone:      row.UserPhone,
		PricePerPlayer: money.Santim(b.PricePerPlayerSantim).Format(),
		Subtotal:       money.Santim(b.SubtotalSantim).Format(),
		Discount:       money.Santim(b.DiscountSantim).Format(),
	}

	gameRows, err := s.store.ListBookingGamesForBookings(ctx, []uuid.UUID{b.ID})
	if err != nil {
		return nil, fmt.Errorf("list booking games: %w", err)
	}
	fieldIDs := make([]uuid.UUID, 0, len(gameRows))
	for _, g := range gameRows {
		fieldIDs = append(fieldIDs, g.Field.ID)
	}
	galleries, err := s.fieldImages(ctx, fieldIDs)
	if err != nil {
		return nil, err
	}
	detail.Games = make([]BookedGame, 0, len(gameRows))
	for _, g := range gameRows {
		detail.Games = append(detail.Games, BookedGame{
			WeekIndex: g.WeekIndex,
			Game:      catalog.ToGameView(g.Game, g.Field, galleries[g.Field.ID]),
		})
	}

	participants, err := s.store.ListBookingParticipants(ctx, b.ID)
	if err != nil {
		return nil, fmt.Errorf("list booking participants: %w", err)
	}
	detail.Participants = make([]BookingParticipant, 0, len(participants))
	for _, p := range participants {
		detail.Participants = append(detail.Participants, BookingParticipant{
			SlotIndex: p.SlotIndex,
			FullName:  p.FullName,
		})
	}

	payments, err := s.store.ListPaymentsForBooking(ctx, b.ID)
	if err != nil {
		return nil, fmt.Errorf("list booking payments: %w", err)
	}
	detail.Payments = make([]AdminPaymentView, 0, len(payments))
	detail.PaymentCount = int64(len(payments))
	for _, p := range payments {
		view := AdminPaymentView{
			ID:                   p.ID,
			Method:               p.Method,
			TransactionReference: p.TransactionReference,
			AccountSuffix:        p.AccountSuffix,
			Status:               p.Status,
			ExpectedAmount:       money.Santim(p.ExpectedAmountSantim).Format(),
			IdempotencyKey:       p.IdempotencyKey,
			VerifierResult:       rawOrEmpty(p.RawResult),
			ResolvedBy:           p.ResolvedBy,
			ResolvedAt:           p.ResolvedAt,
			ResolutionReason:     p.ResolutionReason,
			CreatedAt:            p.CreatedAt,
		}
		if p.VerifiedAmountSantim != nil {
			formatted := money.Santim(*p.VerifiedAmountSantim).Format()
			view.VerifiedAmount = &formatted
		}
		detail.Payments = append(detail.Payments, view)
	}

	return detail, nil
}

// Cancelled reports the outcome of an admin cancellation.
type Cancelled struct {
	BookingID     uuid.UUID `json:"booking_id"`
	ReferenceCode string    `json:"reference_code"`
	Status        string    `json:"status"`
	// GamesCredited is how many games had spots returned. A booking on a game
	// that was itself cancelled credits nothing, which is correct: that game no
	// longer sells anything.
	GamesCredited int64 `json:"games_credited"`
	WasPaid       bool  `json:"was_paid"`
} //	@name	CancelledBooking

// CancelBooking cancels a booking on the customer's behalf and releases its
// spots (PRD FR27).
//
// Refunds are not attempted. The PRD leaves refund policy to the business and
// no money ever moves through this platform, so a booking that was already paid
// for is flagged in the response and in the customer's notification, and the
// money is settled by a person off-platform.
func (s *Service) CancelBooking(ctx context.Context, actor, bookingID uuid.UUID, reason string) (*Cancelled, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, httpx.ErrValidation.WithField("reason", "required")
	}

	var out Cancelled
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetBooking(ctx, bookingID)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get booking: %w", err)
		}

		rows, err := q.CancelBooking(ctx, bookingID)
		if err != nil {
			return fmt.Errorf("cancel booking: %w", err)
		}
		if len(rows) == 0 {
			// The guard rejected it: already cancelled, or expired.
			return httpx.NewError(http.StatusConflict, httpx.CodeBookingNotCancelable,
				"That booking is already closed.")
		}

		out = Cancelled{
			BookingID:     rows[0].ID,
			ReferenceCode: rows[0].ReferenceCode,
			Status:        booking.StatusCancelled,
			GamesCredited: rows[0].GamesCredited,
			WasPaid:       before.ConfirmedAt != nil,
		}

		if err := s.notify(ctx, q, rows[0].UserID, notification.TypeBookingCancelled, map[string]any{
			"booking_id":     rows[0].ID.String(),
			"reference_code": rows[0].ReferenceCode,
			"reason":         reason,
			"was_paid":       boolText(out.WasPaid),
		}); err != nil {
			return err
		}

		// The release may have reopened a sold-out game for the people
		// watching it; their alerts commit with the cancellation.
		if err := booking.NotifySpotsAvailable(ctx, q, s.notifier, rows[0].GameIds); err != nil {
			return err
		}

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionBookingCancelled,
			TargetType: audit.TargetBooking,
			TargetID:   rows[0].ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"reference_code":  rows[0].ReferenceCode,
				"previous_status": before.Status,
				"spot_count":      before.SpotCount,
				"week_count":      before.WeekCount,
				"total_santim":    before.TotalSantim,
				"games_credited":  rows[0].GamesCredited,
				"was_paid":        out.WasPaid,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// boolText renders a boolean for a notification payload, whose values are read
// back as strings by the templates.
func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---- HTTP ----

// listBookings returns bookings newest first (PRD FR27).
//
//	@Summary		List bookings
//	@Description	Every booking, newest first, searchable by reference code, customer name or email and filterable by status.
//	@Tags			bookings
//	@Security		BearerAuth
//	@Produce		json
//	@Param			status	query		string	false	"Filter to one status"	Enums(pending_payment, verifying, pending_review, confirmed, expired, cancelled)
//	@Param			search	query		string	false	"Match the reference code, customer name or email (2-120 characters)"
//	@Param			limit	query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset	query		int		false	"Rows to skip (default 0)"
//	@Success		200		{object}	admin.Page[admin.BookingSummary]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings [get]
func (h *Handler) listBookings(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := parsePagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v := httpx.NewValidator()
	filter := BookingFilter{Limit: limit, Offset: offset}

	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		status := v.OneOf("status", raw,
			booking.StatusPendingPayment, booking.StatusVerifying, booking.StatusPendingReview,
			booking.StatusConfirmed, booking.StatusExpired, booking.StatusCancelled)
		filter.Status = &status
	}
	if search := optionalText(q.Get("search")); search != nil {
		// A one-character search matches most of the table; it is a mistyped
		// query rather than a filter, and it scans every row to say so.
		v.Length("search", *search, 2, 120)
		filter.Search = search
	}
	if err := v.Err(); err != nil {
		return err
	}

	page, err := h.svc.ListBookings(r.Context(), filter)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, page)
	return nil
}

// getBooking returns one booking in full.
//
//	@Summary		Get a booking
//	@Description	The booking with its games, participants and every payment attempt — including the raw verifier result and resolution notes, which is what deciding a disputed payment needs.
//	@Tags			bookings
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		string	true	"Booking id (UUID)"
//	@Success		200	{object}	admin.BookingDetail
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/bookings/{id} [get]
func (h *Handler) getBooking(w http.ResponseWriter, r *http.Request) error {
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	detail, err := h.svc.GetBooking(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, detail)
	return nil
}

// cancelBookingRequest carries the reason an admin is cancelling.
type cancelBookingRequest struct {
	Reason string `json:"reason"`
} //	@name	CancelBookingRequest

// cancelBooking cancels a booking on the customer's behalf (PRD FR27).
//
//	@Summary		Cancel a booking
//	@Description	Cancels the booking, releases its held spots and tells the customer, with the reason passed through verbatim. No refund is attempted: a booking that was already paid is flagged in the response so the money can be settled off-platform.
//	@Tags			bookings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Booking id (UUID)"
//	@Param			body	body		admin.cancelBookingRequest	true	"Cancellation reason"
//	@Success		200		{object}	admin.Cancelled
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"booking_not_cancelable: the booking is already closed"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings/{id}/cancel [post]
func (h *Handler) cancelBooking(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req cancelBookingRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	out, err := h.svc.CancelBooking(r.Context(), principal.UserID, id, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}
