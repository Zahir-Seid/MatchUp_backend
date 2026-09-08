package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// Service owns payment submission. Verification itself happens in the worker
// (see verify.go); nothing here calls a bank.
type Service struct {
	store *db.Store
	log   *slog.Logger
	now   func() time.Time
	// maxAttempts is the retry budget stamped onto each verification job.
	maxAttempts int32
}

// Options configures the submission service.
type Options struct {
	Store  *db.Store
	Logger *slog.Logger
	Now    func() time.Time
	// MaxAttempts is how many times a queued verification may be attempted
	// before it fails permanently and the booking parks for human review.
	// Defaults to 5.
	MaxAttempts int
}

// NewService builds the submission service.
func NewService(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	attempts := int32(opts.MaxAttempts)
	if attempts <= 0 {
		attempts = 5
	}
	return &Service{store: opts.Store, log: log, now: now, maxAttempts: attempts}
}

// SubmitInput is one payment submission. There is deliberately no amount field:
// the amount checked against the bank is read from the booking (PRD FR16).
type SubmitInput struct {
	BookingID      uuid.UUID
	UserID         uuid.UUID
	Method         string
	Reference      string
	AccountSuffix  string
	IdempotencyKey string
}

// Submit records a payment against a booking and queues its verification.
//
// The submission is the last step the payer takes: it moves the booking from
// 'pending_payment' to 'verifying', which stops the hold clock, and hands the
// reference to the worker. The response says "we are checking", never "you are
// confirmed", because only the bank can decide that.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*db.Payment, bool, error) {
	in.Reference = strings.TrimSpace(in.Reference)
	in.AccountSuffix = strings.TrimSpace(in.AccountSuffix)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)

	// A replay of a key this user already spent returns the original payment.
	// Checked before the write so the common retry costs one read, and again
	// via the unique constraint below so a concurrent retry cannot slip past.
	existing, err := s.store.GetPaymentByIdempotencyKey(ctx, db.GetPaymentByIdempotencyKeyParams{
		UserID:         in.UserID,
		IdempotencyKey: in.IdempotencyKey,
	})
	switch {
	case err == nil:
		if existing.BookingID != in.BookingID {
			// The same key reused for a different booking is a client bug, and
			// silently returning the other booking's payment would hide it.
			return nil, false, httpx.NewError(http.StatusConflict, httpx.CodeIdempotencyConflict,
				"That idempotency key was already used for a different booking.")
		}
		return &existing, true, nil
	case db.IsNoRows(err):
		// Expected: a first submission.
	default:
		return nil, false, fmt.Errorf("look up idempotency key: %w", err)
	}

	var suffix *string
	if in.AccountSuffix != "" {
		suffix = &in.AccountSuffix
	}

	var payment db.Payment
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		// One statement claims the booking and inserts the payment. Zero rows
		// means the booking was not claimable: expired by the sweep, already
		// under verification, or never this user's (Fix 2 Part B).
		created, err := q.BeginPaymentVerification(ctx, db.BeginPaymentVerificationParams{
			BookingID:            in.BookingID,
			UserID:               in.UserID,
			Method:               in.Method,
			TransactionReference: in.Reference,
			AccountSuffix:        suffix,
			IdempotencyKey:       in.IdempotencyKey,
		})
		if err != nil {
			return err
		}
		if _, err := q.CreateVerificationJob(ctx, db.CreateVerificationJobParams{
			PaymentID:   created.ID,
			MaxAttempts: s.maxAttempts,
		}); err != nil {
			return fmt.Errorf("queue verification job: %w", err)
		}
		payment = created
		return nil
	})
	if err != nil {
		mapped := s.submitError(ctx, in, err)
		// A concurrent retry with the same idempotency key is a replay, not a
		// failure, so it comes back as the original payment.
		var replay *replayed
		if errors.As(mapped, &replay) {
			return &replay.payment, true, nil
		}
		return nil, false, mapped
	}

	s.log.Info("payment submitted",
		slog.String("payment_id", payment.ID.String()),
		slog.String("booking_id", in.BookingID.String()),
		slog.String("method", in.Method))
	return &payment, false, nil
}

// submitError turns the failure modes of the claim-and-insert into answers the
// app can act on.
func (s *Service) submitError(ctx context.Context, in SubmitInput, err error) error {
	switch {
	case db.IsNoRows(err):
		// The INSERT selected from an empty claim. Read the booking to say
		// which of the reasons it was, since "expired" and "already submitted"
		// call for opposite actions from the payer.
		return s.explainUnclaimable(ctx, in)

	case db.IsUniqueViolation(err, "payments_transaction_reference_key"):
		// FR17: one transaction reference is spendable exactly once
		// system-wide. The booking keeps its 'verifying' status from the
		// rolled-back claim only if the transaction committed, which it did
		// not, so nothing to undo.
		return httpx.NewError(http.StatusConflict, httpx.CodePaymentReferenceReused,
			"That transaction reference has already been used for another booking.").WithCause(err)

	case db.IsUniqueViolation(err, "payments_user_idempotency_key"):
		// A concurrent retry with the same key won the race. Return its
		// payment, which is what the caller wanted.
		existing, lookupErr := s.store.GetPaymentByIdempotencyKey(ctx, db.GetPaymentByIdempotencyKeyParams{
			UserID:         in.UserID,
			IdempotencyKey: in.IdempotencyKey,
		})
		if lookupErr != nil {
			return fmt.Errorf("resolve concurrent idempotent submission: %w", lookupErr)
		}
		return &replayed{payment: existing}

	case db.IsCheckViolation(err):
		// The suffix or reference shape failed its CHECK. Validation should
		// have caught it; answering 422 beats a 500.
		return httpx.ErrValidation.WithField("transaction_reference",
			"that does not look like a valid reference").WithCause(err)

	default:
		return fmt.Errorf("begin payment verification: %w", err)
	}
}

// replayed is the internal signal that a concurrent submission with the same
// idempotency key already created the payment. Submit's caller unwraps it.
type replayed struct{ payment db.Payment }

func (r *replayed) Error() string { return "idempotent replay" }

// explainUnclaimable reports why a booking could not accept a payment.
func (s *Service) explainUnclaimable(ctx context.Context, in SubmitInput) error {
	booking, err := s.store.GetBookingForUser(ctx, db.GetBookingForUserParams{
		ID:     in.BookingID,
		UserID: in.UserID,
	})
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		return fmt.Errorf("get booking: %w", err)
	}

	switch booking.Status {
	case "expired":
		return httpx.NewError(http.StatusConflict, httpx.CodeHoldExpired,
			"This reservation expired before the payment was submitted. Please book again.")
	case "verifying":
		return httpx.NewError(http.StatusConflict, httpx.CodeBookingNotPending,
			"A payment for this booking is already being checked.")
	case "pending_review":
		return httpx.NewError(http.StatusConflict, httpx.CodeBookingNotPending,
			"A payment for this booking is with our team for review.")
	case "confirmed":
		return httpx.NewError(http.StatusConflict, httpx.CodeBookingNotPending,
			"This booking is already confirmed. No further payment is needed.")
	case "cancelled":
		return httpx.NewError(http.StatusConflict, httpx.CodeBookingNotPending,
			"This booking was cancelled.")
	default:
		// 'pending_payment' reaching here means the hold lapsed between the
		// claim and this read; the sweep will expire it shortly.
		return httpx.NewError(http.StatusConflict, httpx.CodeHoldExpired,
			"This reservation is no longer accepting payment. Please book again.")
	}
}

// SubmitView is the response to a submission.
type SubmitView struct {
	PaymentID string `json:"payment_id"`
	BookingID string `json:"booking_id"`
	Method    string `json:"method"`
	Reference string `json:"transaction_reference"`
	Status    string `json:"status"`
	// ExpectedAmount is the amount being checked against the bank, echoed so
	// the payer can see what we are looking for if it does not match.
	ExpectedAmount       string `json:"expected_amount"`
	ExpectedAmountSantim int64  `json:"expected_amount_santim"`
	SubmittedAt          string `json:"submitted_at"`
	// Message is what the app shows: verification is asynchronous, so this is
	// never a confirmation.
	Message string `json:"message"`
}

func toSubmitView(p db.Payment) SubmitView {
	return SubmitView{
		PaymentID:            p.ID.String(),
		BookingID:            p.BookingID.String(),
		Method:               p.Method,
		Reference:            p.TransactionReference,
		Status:               p.Status,
		ExpectedAmount:       money.Santim(p.ExpectedAmountSantim).Format(),
		ExpectedAmountSantim: p.ExpectedAmountSantim,
		SubmittedAt:          p.CreatedAt.UTC().Format(time.RFC3339),
		Message:              submitMessage(p.Status),
	}
}

func submitMessage(status string) string {
	switch status {
	case StatusVerified, StatusApproved:
		return "Your payment is confirmed."
	case StatusMismatch, StatusNotFound:
		return "We could not match this payment automatically. Our team is checking it and your spots are still held."
	case StatusRejected:
		return "This payment was not accepted. Please contact us if you believe that is wrong."
	default:
		return "We are checking your payment with the bank. This usually takes a few seconds."
	}
}
