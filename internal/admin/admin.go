// Package admin is the dashboard API: payment review, booking management,
// catalog editing, user administration and reporting.
//
// It is a separate binary on a separate port with its own signing keys, so a
// routing mistake in the public API cannot expose an admin endpoint -- the
// route simply does not exist in that process (SDD 10.2).
//
// Writes here go through the same guarded queries the public API uses. Nothing
// in this package adjusts spots_remaining by hand or moves a booking's status
// with an unguarded UPDATE: the capacity arithmetic and the status transitions
// are conditional statements in SQL, and an admin shortcut around them would be
// the one path in the system where the invariants do not hold.
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// Notifier is the slice of the notification service this package needs, kept as
// an interface so the admin service can be built without a live dispatch stack.
type Notifier interface {
	Enqueue(ctx context.Context, q *db.Queries, req notification.Request) (*db.Notification, error)
}

// Service holds the admin operations. It is safe for concurrent use.
type Service struct {
	store    *db.Store
	audit    *audit.Recorder
	notifier Notifier
	log      *slog.Logger

	// monthlyWeeks mirrors the booking service's week count. A recurring slot
	// shorter than this can never be booked monthly, so game creation refuses
	// to make one; reading the same configured number keeps the two ends of
	// that rule from drifting apart.
	monthlyWeeks int32

	// paymentAccounts are the environment-configured receiving accounts. The
	// payment settings view reports them, and their source, wherever the
	// dashboard has not saved an override.
	paymentAccounts payment.FallbackAccounts
}

// Options configures the admin service.
type Options struct {
	Store *db.Store
	Audit *audit.Recorder
	// Notifier may be nil, in which case admin actions still apply but the
	// affected customer is not told. Only useful in tests.
	Notifier Notifier
	Logger   *slog.Logger
	// MonthlyWeeks is the configured length of a monthly plan. Defaults to 4.
	MonthlyWeeks int
	// PaymentAccounts are the environment-configured receiving accounts, the
	// bootstrap values the dashboard can override.
	PaymentAccounts payment.FallbackAccounts
}

// NewService builds the admin service.
func NewService(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	weeks := int32(opts.MonthlyWeeks)
	if weeks <= 0 {
		weeks = 4
	}
	return &Service{
		store:           opts.Store,
		audit:           opts.Audit,
		notifier:        opts.Notifier,
		log:             log,
		monthlyWeeks:    weeks,
		paymentAccounts: opts.PaymentAccounts,
	}
}

// MonthlyWeeks reports the configured length of a monthly plan.
func (s *Service) MonthlyWeeks() int32 { return s.monthlyWeeks }

// notify queues a customer notification inside the caller's transaction, so the
// message and the change that justifies it commit together.
func (s *Service) notify(ctx context.Context, q *db.Queries, userID uuid.UUID, typ string, payload map[string]any) error {
	if s.notifier == nil {
		return nil
	}
	if _, err := s.notifier.Enqueue(ctx, q, notification.Request{
		UserID:  userID,
		Type:    typ,
		Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

// record writes an audit entry inside the caller's transaction.
func (s *Service) record(ctx context.Context, q *db.Queries, e audit.Entry) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.Record(ctx, q, e)
}

// Page is the envelope every admin list endpoint returns. Total is the count
// before paging, which is what the dashboard needs to draw pagination at all.
type Page[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int32 `json:"limit"`
	Offset int32 `json:"offset"`
} //	@name	Page

func newPage[T any](items []T, total int64, limit, offset int32) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{Items: items, Total: total, Limit: limit, Offset: offset}
}

// window is a reporting time range, half-open: [From, To).
type window struct {
	From time.Time
	To   time.Time
}

// maxReportWindow caps a report's span. The reporting queries scan bookings and
// payments without a covering index for arbitrary ranges, so an unbounded
// "since the beginning of time" request from a dashboard refresh loop is a
// self-inflicted outage.
const maxReportWindow = 400 * 24 * time.Hour

// parseWindow reads the from/to query parameters, defaulting to the last 30
// days. Both bounds are required to be RFC3339 when present: guessing at a
// date format would silently report on the wrong period.
func parseWindow(r *http.Request) (window, error) {
	q := r.URL.Query()
	v := httpx.NewValidator()
	now := time.Now().UTC()
	w := window{From: now.AddDate(0, 0, -30), To: now}

	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			v.Add("from", "must be an RFC3339 timestamp")
		} else {
			w.From = t
		}
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			v.Add("to", "must be an RFC3339 timestamp")
		} else {
			w.To = t
		}
	}
	if err := v.Err(); err != nil {
		return window{}, err
	}
	if !w.To.After(w.From) {
		return window{}, httpx.ErrValidation.WithField("to", "must be after from")
	}
	if w.To.Sub(w.From) > maxReportWindow {
		return window{}, httpx.ErrValidation.WithField("from",
			"the window may span at most 400 days")
	}
	return w, nil
}

// optionalText returns a pointer for a non-empty trimmed string, which is how
// the generated queries express "no filter".
func optionalText(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
