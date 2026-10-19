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
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// ReviewItem is one payment awaiting a human decision (PRD FR26). It carries
// the whole picture an admin needs to decide without opening three screens:
// what the customer says they paid, what the bank said when we asked, and what
// the booking is worth.
type ReviewItem struct {
	PaymentID            uuid.UUID `json:"payment_id"`
	BookingID            uuid.UUID `json:"booking_id"`
	ReferenceCode        string    `json:"reference_code"`
	BookingType          string    `json:"booking_type"`
	Method               string    `json:"method"`
	TransactionReference string    `json:"transaction_reference"`
	// AccountSuffix is the payer's own account tail, present only for CBE.
	AccountSuffix  *string   `json:"account_suffix,omitempty"`
	Status         string    `json:"status"`
	ExpectedAmount string    `json:"expected_amount"`
	VerifiedAmount *string   `json:"verified_amount,omitempty"`
	BookingTotal   string    `json:"booking_total"`
	SubmittedAt    time.Time `json:"submitted_at"`
	PayerName      string    `json:"payer_name"`
	PayerEmail     string    `json:"payer_email"`
	// VerifierResult is the decision trail written by the worker: the outcome,
	// the payee check, the reasons, and the bank's own response verbatim. This
	// is the evidence the decision rests on, so it is passed through unedited
	// rather than summarized.
	VerifierResult json.RawMessage `json:"verifier_result" swaggertype:"object"`
} //	@name	PaymentReviewItem

// ReviewQueue returns the payments that automatic verification could not settle,
// oldest first: a customer whose money is in limbo has been waiting longest.
func (s *Service) ReviewQueue(ctx context.Context, limit, offset int32) (Page[ReviewItem], error) {
	rows, err := s.store.ListPaymentReviewQueue(ctx, db.ListPaymentReviewQueueParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return Page[ReviewItem]{}, fmt.Errorf("list payment review queue: %w", err)
	}
	total, err := s.store.CountPaymentReviewQueue(ctx)
	if err != nil {
		return Page[ReviewItem]{}, fmt.Errorf("count payment review queue: %w", err)
	}

	items := make([]ReviewItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toReviewItem(row))
	}
	return newPage(items, total, limit, offset), nil
}

func toReviewItem(row db.ListPaymentReviewQueueRow) ReviewItem {
	p := row.Payment
	item := ReviewItem{
		PaymentID:            p.ID,
		BookingID:            p.BookingID,
		ReferenceCode:        row.ReferenceCode,
		BookingType:          row.BookingType,
		Method:               p.Method,
		TransactionReference: p.TransactionReference,
		AccountSuffix:        p.AccountSuffix,
		Status:               p.Status,
		ExpectedAmount:       money.Santim(p.ExpectedAmountSantim).Format(),
		BookingTotal:         money.Santim(row.TotalSantim).Format(),
		SubmittedAt:          p.CreatedAt,
		PayerName:            row.PayerName,
		PayerEmail:           row.PayerEmail,
		VerifierResult:       rawOrEmpty(p.RawResult),
	}
	if p.VerifiedAmountSantim != nil {
		formatted := money.Santim(*p.VerifiedAmountSantim).Format()
		item.VerifiedAmount = &formatted
	}
	return item
}

// rawOrEmpty keeps a jsonb column from serializing as null, which a dashboard
// would have to special-case on every field it reads.
func rawOrEmpty(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}

// ResolveInput is an admin's decision on a queued payment.
type ResolveInput struct {
	PaymentID uuid.UUID
	// Approve confirms the booking; false rejects the payment and releases the
	// spots.
	Approve bool
	// Reason is mandatory either way. An approval overrides an automatic
	// decision that said the money could not be matched, and a rejection is
	// told to the customer verbatim; neither is defensible as a bare click.
	Reason string
}

// Resolved reports what the decision did.
type Resolved struct {
	PaymentID     uuid.UUID `json:"payment_id"`
	BookingID     uuid.UUID `json:"booking_id"`
	ReferenceCode string    `json:"reference_code"`
	PaymentStatus string    `json:"payment_status"`
	BookingStatus string    `json:"booking_status"`
	// SpotsReleased is set on a rejection, reporting how many games had their
	// capacity credited back.
	SpotsReleased int64 `json:"spots_released"`
} //	@name	PaymentResolution

// ResolvePayment applies an approve/reject decision (PRD FR26).
//
// Everything happens in one transaction: the payment outcome, the booking
// transition, the audit entry and the customer's notification. A partial
// commit here is the worst failure in the system -- a booking confirmed with no
// record of who confirmed it, or a customer told their payment was rejected
// while the booking still holds its spots.
func (s *Service) ResolvePayment(ctx context.Context, actor uuid.UUID, in ResolveInput) (*Resolved, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, httpx.ErrValidation.WithField("reason", "required")
	}

	status := payment.StatusRejected
	action := audit.ActionPaymentRejected
	if in.Approve {
		status = payment.StatusApproved
		action = audit.ActionPaymentApproved
	}

	var out Resolved
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		resolved, err := q.ResolvePayment(ctx, db.ResolvePaymentParams{
			ID:               in.PaymentID,
			Status:           status,
			ResolvedBy:       &actor,
			ResolutionReason: &reason,
		})
		if err != nil {
			if db.IsNoRows(err) {
				// Either no such payment, or it is not in the queue: already
				// resolved by another admin, or auto-verified in the meantime.
				// Both are conflicts rather than not-founds, because the
				// dashboard's list is simply stale.
				return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
					"That payment is no longer awaiting review.").WithCause(err)
			}
			return fmt.Errorf("resolve payment: %w", err)
		}

		booking, err := q.GetBooking(ctx, resolved.BookingID)
		if err != nil {
			return fmt.Errorf("get booking for resolved payment: %w", err)
		}
		out = Resolved{
			PaymentID:     resolved.ID,
			BookingID:     booking.ID,
			ReferenceCode: booking.ReferenceCode,
			PaymentStatus: resolved.Status,
			BookingStatus: booking.Status,
		}

		if in.Approve {
			if err := s.approveBooking(ctx, q, resolved, booking, reason, &out); err != nil {
				return err
			}
		} else if err := s.rejectBooking(ctx, q, resolved, booking, reason, &out); err != nil {
			return err
		}

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     action,
			TargetType: audit.TargetPayment,
			TargetID:   resolved.ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"booking_id":              booking.ID.String(),
				"reference_code":          booking.ReferenceCode,
				"method":                  resolved.Method,
				"transaction_reference":   resolved.TransactionReference,
				"expected_amount_santim":  resolved.ExpectedAmountSantim,
				"previous_booking_status": booking.Status,
				"booking_status":          out.BookingStatus,
				"payment_status":          resolved.Status,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// approveBooking confirms the booking behind an approved payment.
func (s *Service) approveBooking(
	ctx context.Context,
	q *db.Queries,
	resolved db.Payment,
	booking db.Booking,
	reason string,
	out *Resolved,
) error {
	confirmed, err := q.SetBookingConfirmed(ctx, booking.ID)
	if err != nil {
		if db.IsNoRows(err) {
			// The booking left 'verifying'/'pending_review' by another route,
			// most plausibly an admin cancelling it. The payment stands as
			// approved -- the money did arrive -- and the refund is a business
			// decision the PRD leaves off-platform, so this is recorded rather
			// than rolled back.
			s.log.Warn("approved a payment for a booking that is no longer reviewable",
				"booking_id", booking.ID.String(),
				"booking_status", booking.Status,
				"payment_id", resolved.ID.String())
			return nil
		}
		return fmt.Errorf("confirm booking: %w", err)
	}
	out.BookingStatus = confirmed.Status

	return s.notify(ctx, q, booking.UserID, notification.TypeBookingConfirmed, map[string]any{
		"booking_id":     confirmed.ID.String(),
		"reference_code": confirmed.ReferenceCode,
		"total":          money.Santim(confirmed.TotalSantim).Format(),
		"reason":         reason,
	})
}

// rejectBooking cancels the booking behind a rejected payment, which is what
// releases the held spots back onto the game.
func (s *Service) rejectBooking(
	ctx context.Context,
	q *db.Queries,
	resolved db.Payment,
	booking db.Booking,
	reason string,
	out *Resolved,
) error {
	rows, err := q.CancelBooking(ctx, booking.ID)
	if err != nil {
		return fmt.Errorf("cancel booking after rejection: %w", err)
	}
	if len(rows) == 0 {
		// Already cancelled or expired. Nothing to release and nothing to tell
		// the customer that they were not told when it happened.
		s.log.Warn("rejected a payment for a booking that was already closed",
			"booking_id", booking.ID.String(),
			"booking_status", booking.Status,
			"payment_id", resolved.ID.String())
		return nil
	}
	out.BookingStatus = "cancelled"
	out.SpotsReleased = rows[0].GamesCredited

	// The admin's reason is passed through to the customer. A rejection with no
	// explanation is worse for them than an internal note being visible.
	return s.notify(ctx, q, booking.UserID, notification.TypePaymentRejected, map[string]any{
		"booking_id":     booking.ID.String(),
		"reference_code": booking.ReferenceCode,
		"reason":         reason,
	})
}

// ---- HTTP ----

// listPaymentReviewQueue returns the payments awaiting a human decision.
//
//	@Summary		List the payment review queue
//	@Description	Payments automatic verification could not settle, oldest first: a customer whose money is in limbo has been waiting longest.
//	@Tags			payments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			limit	query		int	false	"Page size, 1-100 (default 20)"
//	@Param			offset	query		int	false	"Rows to skip (default 0)"
//	@Success		200		{object}	admin.Page[admin.ReviewItem]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: bad paging"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/payments/review-queue [get]
func (h *Handler) listPaymentReviewQueue(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := parsePagination(r)
	if err != nil {
		return err
	}
	page, err := h.svc.ReviewQueue(r.Context(), limit, offset)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, page)
	return nil
}

// resolvePaymentRequest is an admin's decision body.
type resolvePaymentRequest struct {
	// Decision is "approve" or "reject". A named decision beats a boolean in a
	// JSON body: "approved": false is far too easy to send by accident.
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
} //	@name	ResolvePaymentRequest

// resolvePayment approves or rejects one queued payment (PRD FR26).
//
//	@Summary		Resolve a queued payment
//	@Description	Approving confirms the booking; rejecting cancels it and releases the held spots. The reason is mandatory and is passed to the customer verbatim. A payment that left the queue while the screen was open is a conflict.
//	@Tags			payments
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Payment id (UUID)"
//	@Param			body	body		admin.resolvePaymentRequest	true	"Decision"
//	@Success		200		{object}	admin.Resolved
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"conflict: that payment is no longer awaiting review"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/payments/{id}/resolve [post]
func (h *Handler) resolvePayment(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}

	var req resolvePaymentRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	decision := v.OneOf("decision", req.Decision, "approve", "reject")
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	resolved, err := h.svc.ResolvePayment(r.Context(), principal.UserID, ResolveInput{
		PaymentID: id,
		Approve:   decision == "approve",
		Reason:    reason,
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, resolved)
	return nil
}
