package booking

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
)

// Notifier is the slice of the notification service the sweep needs. Kept as an
// interface so the sweep can run in tests without a notification stack.
type Notifier interface {
	Enqueue(ctx context.Context, q *db.Queries, req notification.Request) (*db.Notification, error)
}

// Sweeper releases the spots held by bookings whose payment window ran out, and
// nudges the ones about to run out. It is driven by the worker on a short
// interval (PRD FR13); nothing on the request path calls it.
//
// Both passes run their state change and their notifications in one
// transaction. That is what makes each of them exactly-once in the face of
// several workers and repeated runs: the claim and the notification commit
// together, so a failed notification insert un-claims the booking and the next
// sweep tries again, rather than leaving a booking marked warned that nobody was
// warned about.
type Sweeper struct {
	store    *db.Store
	log      *slog.Logger
	notifier Notifier

	warnBefore  time.Duration
	warnMaxRows int32
}

// SweepOptions configures the sweeper.
type SweepOptions struct {
	Store  *db.Store
	Logger *slog.Logger
	// Notifier may be nil, in which case spots are still released on time and
	// the customer simply is not told. Releasing the capacity is the part that
	// cannot wait.
	Notifier Notifier
	// WarnBefore is how far ahead of expiry the nudge goes out. Config
	// validation keeps it shorter than the hold itself.
	WarnBefore time.Duration
	// WarnMaxRows bounds one warning pass, so a backlog cannot turn a single
	// sweep into an unbounded batch of sends.
	WarnMaxRows int
}

// NewSweeper builds the expiry sweeper.
func NewSweeper(opts SweepOptions) *Sweeper {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	warn := opts.WarnBefore
	if warn <= 0 {
		warn = 2 * time.Minute
	}
	rows := int32(opts.WarnMaxRows)
	if rows <= 0 {
		rows = 100
	}
	return &Sweeper{
		store:       opts.Store,
		log:         log,
		notifier:    opts.Notifier,
		warnBefore:  warn,
		warnMaxRows: rows,
	}
}

// ExpireOnce releases every booking whose hold has lapsed, returning how many
// were expired.
//
// A booking with a live verification job is left alone by the query itself
// (Fix 2 Part A): the payer submitted in time and the bank is still answering,
// so expiring it would take spots away from somebody who did everything right.
func (s *Sweeper) ExpireOnce(ctx context.Context) (int, error) {
	var expired []db.ExpireHeldBookingsRow

	err := s.store.InTx(ctx, func(q *db.Queries) error {
		rows, err := q.ExpireHeldBookings(ctx)
		if err != nil {
			return fmt.Errorf("expire held bookings: %w", err)
		}
		expired = rows

		for _, row := range rows {
			if s.notifier == nil {
				continue
			}
			if _, err := s.notifier.Enqueue(ctx, q, notification.Request{
				UserID: row.UserID,
				Type:   notification.TypeHoldExpired,
				Payload: map[string]any{
					"booking_id":     row.ID.String(),
					"reference_code": row.ReferenceCode,
				},
			}); err != nil {
				return fmt.Errorf("notify hold expired for booking %s: %w", row.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	for _, row := range expired {
		s.log.Info("booking hold expired",
			slog.String("booking_id", row.ID.String()),
			slog.String("reference_code", row.ReferenceCode),
			slog.Int64("games_credited", row.GamesCredited))
	}
	return len(expired), nil
}

// WarnOnce sends the "finish your payment" nudge for holds about to lapse,
// returning how many were warned. Each booking is warned at most once, enforced
// by the claim in the query rather than by checking for a prior notification.
func (s *Sweeper) WarnOnce(ctx context.Context) (int, error) {
	if s.notifier == nil {
		// Claiming rows here would mark them warned with nothing sent, and the
		// claim is not reversible.
		return 0, nil
	}

	var warned int
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		rows, err := q.ClaimBookingsNearingExpiry(ctx, db.ClaimBookingsNearingExpiryParams{
			WarnBefore: db.Interval(s.warnBefore),
			MaxRows:    s.warnMaxRows,
		})
		if err != nil {
			return fmt.Errorf("claim bookings nearing expiry: %w", err)
		}

		for _, row := range rows {
			payload := map[string]any{
				"booking_id":     row.ID.String(),
				"reference_code": row.ReferenceCode,
				"total":          money.Santim(row.TotalSantim).Format(),
			}
			if row.HoldExpiresAt != nil {
				payload["hold_expires_at"] = row.HoldExpiresAt.Format(timeLayout)
			}
			if _, err := s.notifier.Enqueue(ctx, q, notification.Request{
				UserID:  row.UserID,
				Type:    notification.TypeHoldExpiring,
				Payload: payload,
			}); err != nil {
				return fmt.Errorf("notify hold expiring for booking %s: %w", row.ID, err)
			}
		}
		warned = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return warned, nil
}

// RunOnce performs both passes. The warning runs first: a booking that is about
// to expire should be nudged before the sweep that would expire it, and running
// the other way round in the same tick would occasionally warn about a booking
// that had just been released.
func (s *Sweeper) RunOnce(ctx context.Context) (warned, expired int, err error) {
	warned, warnErr := s.WarnOnce(ctx)
	expired, expireErr := s.ExpireOnce(ctx)

	// The expiry pass is the one that must not be skipped, so a failed warning
	// does not short-circuit it; both errors are reported.
	switch {
	case warnErr != nil && expireErr != nil:
		return warned, expired, fmt.Errorf("%w; %w", warnErr, expireErr)
	case warnErr != nil:
		return warned, expired, warnErr
	case expireErr != nil:
		return warned, expired, expireErr
	}
	return warned, expired, nil
}
