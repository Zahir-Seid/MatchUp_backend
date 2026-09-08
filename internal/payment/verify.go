package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
)

// Notifier is the slice of the notification service this package needs. Kept as
// an interface so the verifier can be tested without a live notification stack.
type Notifier interface {
	Enqueue(ctx context.Context, q *db.Queries, req notification.Request) (*db.Notification, error)
}

// VerifyService runs queued verification jobs. It is driven by the worker.
type VerifyService struct {
	store     *db.Store
	log       *slog.Logger
	notifier  Notifier
	verifiers map[string]Verifier

	batchSize   int32
	concurrency int
	staleAfter  time.Duration
	now         func() time.Time
}

// VerifyOptions configures the verification runner.
type VerifyOptions struct {
	Store  *db.Store
	Logger *slog.Logger
	// Notifier may be nil, in which case outcomes are applied without telling
	// the customer. Only useful in tests.
	Notifier Notifier
	// Verifiers are the payment channels, keyed by their Method().
	Verifiers []Verifier
	BatchSize int
	// Concurrency is how many payments in a claimed batch are verified at
	// once. A verification is a slow call to a bank -- tens of seconds in the
	// worst case -- so running the batch strictly in series would make the
	// last payment in it wait for every one before it. Default 4.
	Concurrency int
	StaleAfter  time.Duration
	Now         func() time.Time
}

// NewVerifyService builds the verification runner.
func NewVerifyService(opts VerifyOptions) *VerifyService {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	batch := int32(opts.BatchSize)
	if batch <= 0 {
		batch = 10
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > int(batch) {
		// More slots than work is pointless, and it keeps the semaphore's
		// buffer honest about the real ceiling.
		concurrency = int(batch)
	}
	stale := opts.StaleAfter
	if stale <= 0 {
		stale = 5 * time.Minute
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	verifiers := make(map[string]Verifier, len(opts.Verifiers))
	for _, v := range opts.Verifiers {
		if v != nil {
			verifiers[v.Method()] = v
		}
	}
	return &VerifyService{
		store:       opts.Store,
		log:         log,
		notifier:    opts.Notifier,
		verifiers:   verifiers,
		batchSize:   batch,
		concurrency: concurrency,
		staleAfter:  stale,
		now:         now,
	}
}

// RunOnce claims a batch of pending verification jobs and processes them,
// returning how many were claimed.
func (s *VerifyService) RunOnce(ctx context.Context) (int, error) {
	jobs, err := s.store.ClaimVerificationJobs(ctx, s.batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim verification jobs: %w", err)
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	paymentIDs := make([]uuid.UUID, 0, len(jobs))
	for _, job := range jobs {
		paymentIDs = append(paymentIDs, job.PaymentID)
	}
	contexts, err := s.store.ListVerificationContexts(ctx, paymentIDs)
	if err != nil {
		return len(jobs), fmt.Errorf("list verification contexts: %w", err)
	}
	byPayment := make(map[uuid.UUID]db.ListVerificationContextsRow, len(contexts))
	for _, row := range contexts {
		byPayment[row.PaymentID] = row
	}

	// The batch is verified with bounded concurrency. Each job owns its own
	// transaction and touches only its own payment and booking, so they do not
	// contend; the bound exists to keep one worker from opening an unbounded
	// number of connections to the banks and to the pool.
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup

	for _, job := range jobs {
		row, ok := byPayment[job.PaymentID]
		if !ok {
			// No payment behind the job. Unreachable through the API; nothing
			// to verify, so the job is closed rather than retried forever.
			s.log.Error("verification job has no payment",
				slog.String("job_id", job.ID.String()),
				slog.String("payment_id", job.PaymentID.String()))
			s.complete(ctx, job.ID)
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Shutting down or out of time. Whatever is left stays claimed and
			// the reconciliation pass hands it back.
			wg.Wait()
			return len(jobs), ctx.Err()
		}

		wg.Add(1)
		go func(job db.PaymentVerificationJob, row db.ListVerificationContextsRow) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runJob(ctx, job, row)
		}(job, row)
	}
	wg.Wait()
	return len(jobs), nil
}

// runJob verifies one payment and applies the outcome.
func (s *VerifyService) runJob(ctx context.Context, job db.PaymentVerificationJob, row db.ListVerificationContextsRow) {
	log := s.log.With(
		slog.String("job_id", job.ID.String()),
		slog.String("payment_id", row.PaymentID.String()),
		slog.String("booking_id", row.BookingID.String()),
		slog.String("method", row.Method),
		slog.Int("attempts", int(job.Attempts)),
	)

	verifier, ok := s.verifiers[row.Method]
	if !ok {
		// The method is CHECK-constrained, so this is a deployment gap: the
		// worker was started without that channel's credentials. Retry rather
		// than reject: the payment may be perfectly good.
		s.retry(ctx, job, row, fmt.Errorf("no verifier configured for method %q", row.Method), log)
		return
	}

	suffix := ""
	if row.AccountSuffix != nil {
		suffix = *row.AccountSuffix
	}

	result, err := verifier.Verify(ctx, VerifyRequest{
		Reference:      row.TransactionReference,
		AccountSuffix:  suffix,
		ExpectedAmount: money.Santim(row.ExpectedAmountSantim),
	})
	if err != nil {
		if message, permanent := PermanentMessage(err); permanent {
			// The reference can never verify. Park it for review with the
			// reason recorded; the spots stay held for the human decision.
			log.Info("payment permanently unverifiable", slog.Any("error", err))
			s.apply(ctx, job, row, VerifyResult{
				Outcome:    OutcomeMismatch,
				PayeeCheck: PayeeUnknown,
				Reasons:    []string{message},
			}, log)
			return
		}
		s.retry(ctx, job, row, err, log)
		return
	}

	log.Info("payment verified",
		slog.String("outcome", string(result.Outcome)),
		slog.String("payee_check", string(result.PayeeCheck)))
	s.apply(ctx, job, row, result, log)
}

// apply writes the outcome, moves the booking, and queues the customer
// notification -- all in one transaction, so a confirmed booking always has its
// payment row and its notification (SDD 8.5).
func (s *VerifyService) apply(
	ctx context.Context,
	job db.PaymentVerificationJob,
	row db.ListVerificationContextsRow,
	result VerifyResult,
	log *slog.Logger,
) {
	status := StatusMismatch
	switch result.Outcome {
	case OutcomeVerified:
		status = StatusVerified
	case OutcomeNotFound:
		status = StatusNotFound
	}

	raw, err := marshalRawResult(result, s.now())
	if err != nil {
		// Losing the envelope must not lose the decision.
		log.Error("could not encode verification result", slog.Any("error", err))
		raw = nil
	}

	var verified *int64
	if result.AmountKnown {
		amount := int64(result.Amount)
		verified = &amount
	}

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		// The WHERE status = 'pending' guard makes this idempotent: a worker
		// that died after committing here re-runs the job and changes nothing.
		payment, err := q.SetPaymentOutcome(ctx, db.SetPaymentOutcomeParams{
			ID:                   row.PaymentID,
			Status:               status,
			VerifiedAmountSantim: verified,
			RawResult:            raw,
		})
		if err != nil {
			if db.IsNoRows(err) {
				log.Info("payment outcome already recorded, nothing to apply")
				return nil
			}
			return fmt.Errorf("set payment outcome: %w", err)
		}

		if status == StatusVerified {
			return s.confirmBooking(ctx, q, row, payment)
		}
		return s.parkForReview(ctx, q, row, result)
	})
	if err != nil {
		// The outcome did not stick. Retry the job: the verifier will be asked
		// again, which is safe because verification is a read at the bank.
		s.retry(ctx, job, row, err, log)
		return
	}
	s.complete(ctx, job.ID)
}

// confirmBooking marks the booking paid and tells the customer.
func (s *VerifyService) confirmBooking(
	ctx context.Context,
	q *db.Queries,
	row db.ListVerificationContextsRow,
	payment db.Payment,
) error {
	booking, err := q.SetBookingConfirmed(ctx, row.BookingID)
	if err != nil {
		if db.IsNoRows(err) {
			// The booking left 'verifying' by another route -- an admin
			// cancellation, most plausibly. The payment stands as verified; a
			// human deals with the refund, which the PRD leaves to the
			// business.
			s.log.Warn("verified payment for a booking that is no longer verifying",
				slog.String("booking_id", row.BookingID.String()),
				slog.String("payment_id", payment.ID.String()))
			return nil
		}
		return fmt.Errorf("confirm booking: %w", err)
	}

	return s.notify(ctx, q, row.UserID, notification.TypeBookingConfirmed, map[string]any{
		"booking_id":     booking.ID.String(),
		"reference_code": booking.ReferenceCode,
		"total":          money.Santim(booking.TotalSantim).Format(),
	})
}

// parkForReview moves the booking into the admin queue with the spots still
// held. Nothing is released here: a payment we could not match is not the same
// as a payment that did not happen, and releasing the spots would punish the
// customer for our uncertainty (PRD FR26).
func (s *VerifyService) parkForReview(
	ctx context.Context,
	q *db.Queries,
	row db.ListVerificationContextsRow,
	result VerifyResult,
) error {
	if _, err := q.SetBookingPendingReview(ctx, row.BookingID); err != nil && !db.IsNoRows(err) {
		return fmt.Errorf("park booking for review: %w", err)
	}

	return s.notify(ctx, q, row.UserID, notification.TypePaymentUnderReview, map[string]any{
		"booking_id":     row.BookingID.String(),
		"reference_code": row.ReferenceCode,
		"reason":         strings.Join(result.Reasons, "; "),
	})
}

func (s *VerifyService) notify(ctx context.Context, q *db.Queries, userID uuid.UUID, typ string, payload map[string]any) error {
	if s.notifier == nil {
		return nil
	}
	if _, err := s.notifier.Enqueue(ctx, q, notification.Request{
		UserID:  userID,
		Type:    typ,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("queue %s notification: %w", typ, err)
	}
	return nil
}

// retry puts the job back with backoff. When the attempt budget is spent the
// job fails permanently, and the booking is parked for review rather than left
// in 'verifying' forever.
func (s *VerifyService) retry(
	ctx context.Context,
	job db.PaymentVerificationJob,
	row db.ListVerificationContextsRow,
	cause error,
	log *slog.Logger,
) {
	message := truncate(cause.Error(), 500)
	updated, err := s.store.RescheduleVerificationJob(ctx, db.RescheduleVerificationJobParams{
		ID:        job.ID,
		Backoff:   db.Interval(notification.Backoff(int(job.Attempts))),
		LastError: &message,
	})
	if err != nil {
		log.Error("could not reschedule verification job", slog.Any("error", err))
		return
	}

	if updated.Status != "failed" {
		log.Warn("verification failed, will retry", slog.Any("error", cause))
		return
	}

	log.Error("verification exhausted its attempts, parking for review", slog.Any("error", cause))
	s.parkExhausted(ctx, row, message, log)
}

// parkExhausted records the give-up outcome for a job that ran out of attempts.
// Without this a transport outage would leave the booking in 'verifying', which
// the expiry sweep ignores, so it would hold its spots indefinitely with nobody
// looking at it.
func (s *VerifyService) parkExhausted(ctx context.Context, row db.ListVerificationContextsRow, cause string, log *slog.Logger) {
	result := VerifyResult{
		Outcome:    OutcomeMismatch,
		PayeeCheck: PayeeUnknown,
		Reasons:    []string{"automatic verification could not be completed: " + cause},
	}
	raw, err := marshalRawResult(result, s.now())
	if err != nil {
		raw = nil
	}

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		if _, err := q.SetPaymentOutcome(ctx, db.SetPaymentOutcomeParams{
			ID:        row.PaymentID,
			Status:    StatusMismatch,
			RawResult: raw,
		}); err != nil && !db.IsNoRows(err) {
			return fmt.Errorf("set payment outcome: %w", err)
		}
		return s.parkForReview(ctx, q, row, result)
	})
	if err != nil {
		log.Error("could not park exhausted verification for review", slog.Any("error", err))
	}
}

func (s *VerifyService) complete(ctx context.Context, jobID uuid.UUID) {
	if err := s.store.CompleteVerificationJob(ctx, jobID); err != nil {
		s.log.Error("could not complete verification job",
			slog.String("job_id", jobID.String()), slog.Any("error", err))
	}
}

// ReclaimStuck returns jobs abandoned by a dead worker to the queue. Jobs that
// exhausted their attempts while stuck are parked for review, for the same
// reason parkExhausted exists.
func (s *VerifyService) ReclaimStuck(ctx context.Context) (int, error) {
	jobs, err := s.store.ReclaimStuckVerificationJobs(ctx, db.Interval(s.staleAfter))
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck verification jobs: %w", err)
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	var failed []uuid.UUID
	for _, job := range jobs {
		if job.Status == "failed" {
			failed = append(failed, job.PaymentID)
		}
	}
	s.log.Warn("reclaimed stalled verification jobs",
		slog.Int("reclaimed", len(jobs)), slog.Int("exhausted", len(failed)))

	if len(failed) > 0 {
		contexts, err := s.store.ListVerificationContexts(ctx, failed)
		if err != nil {
			return len(jobs), fmt.Errorf("list contexts for exhausted jobs: %w", err)
		}
		for _, row := range contexts {
			s.parkExhausted(ctx, row, "worker stalled and the job ran out of attempts",
				s.log.With(slog.String("payment_id", row.PaymentID.String())))
		}
	}
	return len(jobs), nil
}

// rawEnvelope is what lands in payments.raw_result: the bank's own answer kept
// verbatim under "provider", wrapped in the decision trail an admin needs to
// understand why the payment was flagged (PRD J7).
type rawEnvelope struct {
	Outcome    VerifyOutcome `json:"outcome"`
	PayeeCheck PayeeVerdict  `json:"payee_check"`
	Reasons    []string      `json:"reasons,omitempty"`
	Amount     *string       `json:"reported_amount,omitempty"`
	CheckedAt  string        `json:"checked_at"`
	Provider   any           `json:"provider,omitempty"`
}

func marshalRawResult(result VerifyResult, at time.Time) ([]byte, error) {
	env := rawEnvelope{
		Outcome:    result.Outcome,
		PayeeCheck: result.PayeeCheck,
		Reasons:    result.Reasons,
		CheckedAt:  at.UTC().Format(time.RFC3339),
		Provider:   result.Raw,
	}
	if result.AmountKnown {
		amount := result.Amount.String()
		env.Amount = &amount
	}

	encoded, err := json.Marshal(env)
	if err != nil {
		// The provider result is the only part that can fail to encode. Drop it
		// and keep our own decision trail, which is the part we control.
		env.Provider = nil
		env.Reasons = append(env.Reasons, "provider result could not be encoded")
		fallback, fallbackErr := json.Marshal(env)
		if fallbackErr != nil {
			return nil, errors.Join(err, fallbackErr)
		}
		return fallback, nil
	}
	return encoded, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
