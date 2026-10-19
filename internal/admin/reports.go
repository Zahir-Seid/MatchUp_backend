package admin

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// Report is the envelope the simpler reporting endpoints return. The window is
// echoed back because the bounds are defaulted server-side: a dashboard that
// sent no from/to still needs to label the period it is drawing.
type Report[T any] struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Rows []T       `json:"rows"`
} //	@name	Report

func newReport[T any](w window, rows []T) Report[T] {
	if rows == nil {
		rows = []T{}
	}
	return Report[T]{From: w.From, To: w.To, Rows: rows}
}

// FieldRevenue is one field's line in the revenue report (PRD FR28).
//
// Margin is absent rather than zero when any covered game is missing its cost,
// because a partially known cost produces a margin that reads as fact and is
// not one. Cost is still reported, so a dashboard that wants to show the
// partial figure can, as long as it says so.
type FieldRevenue struct {
	FieldID       uuid.UUID `json:"field_id"`
	FieldName     string    `json:"field_name"`
	Bookings      int64     `json:"bookings"`
	SpotsSold     int64     `json:"spots_sold"`
	Revenue       string    `json:"revenue"`
	RevenueSantim int64     `json:"revenue_santim"`
	Cost          string    `json:"cost"`
	CostSantim    int64     `json:"cost_santim"`
	Margin        *string   `json:"margin,omitempty"`
	MarginSantim  *int64    `json:"margin_santim,omitempty"`
	CostComplete  bool      `json:"cost_complete"`
} //	@name	FieldRevenue

// RevenueTotals is the headline row. CostComplete is true only when every field
// in the window had complete cost data, so one field with a missing cost
// withholds the platform-wide margin too.
type RevenueTotals struct {
	Bookings      int64   `json:"bookings"`
	SpotsSold     int64   `json:"spots_sold"`
	Revenue       string  `json:"revenue"`
	RevenueSantim int64   `json:"revenue_santim"`
	Cost          string  `json:"cost"`
	CostSantim    int64   `json:"cost_santim"`
	Margin        *string `json:"margin,omitempty"`
	MarginSantim  *int64  `json:"margin_santim,omitempty"`
	CostComplete  bool    `json:"cost_complete"`
} //	@name	RevenueTotals

// RevenueReport is the per-field revenue breakdown with its totals.
type RevenueReport struct {
	From   time.Time      `json:"from"`
	To     time.Time      `json:"to"`
	Fields []FieldRevenue `json:"fields"`
	Totals RevenueTotals  `json:"totals"`
} //	@name	RevenueReport

// Revenue reports confirmed revenue per field over the window. The window is
// read against confirmed_at, not created_at: revenue belongs to the day the
// money was accepted, which is the figure a monthly reconciliation has to
// match.
func (s *Service) Revenue(ctx context.Context, w window) (*RevenueReport, error) {
	rows, err := s.store.RevenueByField(ctx, db.RevenueByFieldParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return nil, fmt.Errorf("revenue by field: %w", err)
	}

	report := RevenueReport{From: w.From, To: w.To, Fields: make([]FieldRevenue, 0, len(rows))}
	totals := RevenueTotals{CostComplete: true}

	for _, row := range rows {
		field := FieldRevenue{
			FieldID:       row.FieldID,
			FieldName:     row.FieldName,
			Bookings:      row.Bookings,
			SpotsSold:     row.SpotsSold,
			Revenue:       money.Santim(row.RevenueSantim).Format(),
			RevenueSantim: row.RevenueSantim,
			Cost:          money.Santim(row.CostSantim).Format(),
			CostSantim:    row.CostSantim,
			CostComplete:  row.CostComplete,
		}
		if row.CostComplete {
			margin := row.RevenueSantim - row.CostSantim
			formatted := money.Santim(margin).Format()
			field.Margin = &formatted
			field.MarginSantim = &margin
		}
		report.Fields = append(report.Fields, field)

		totals.Bookings += row.Bookings
		totals.SpotsSold += row.SpotsSold
		totals.RevenueSantim += row.RevenueSantim
		totals.CostSantim += row.CostSantim
		totals.CostComplete = totals.CostComplete && row.CostComplete
	}

	// An empty window has nothing to be complete about; reporting a total margin
	// of zero as if it were known would be the wrong kind of tidy.
	if len(rows) == 0 {
		totals.CostComplete = false
	}
	totals.Revenue = money.Santim(totals.RevenueSantim).Format()
	totals.Cost = money.Santim(totals.CostSantim).Format()
	if totals.CostComplete {
		margin := totals.RevenueSantim - totals.CostSantim
		formatted := money.Santim(margin).Format()
		totals.Margin = &formatted
		totals.MarginSantim = &margin
	}
	report.Totals = totals
	return &report, nil
}

// DailyVolume is one day of booking activity (PRD FR28).
type DailyVolume struct {
	Day                    string `json:"day"`
	BookingsCreated        int64  `json:"bookings_created"`
	Confirmed              int64  `json:"confirmed"`
	Expired                int64  `json:"expired"`
	Cancelled              int64  `json:"cancelled"`
	PendingReview          int64  `json:"pending_review"`
	ConfirmedRevenue       string `json:"confirmed_revenue"`
	ConfirmedRevenueSantim int64  `json:"confirmed_revenue_santim"`
} //	@name	DailyVolume

// BookingVolume reports bookings created per day, with how they ended up.
//
// The status counts are the bookings' current status, not their status on that
// day, so a booking created on Monday and cancelled on Friday counts as
// cancelled under Monday. That is what makes the row answer "of the bookings
// taken that day, how many stuck".
func (s *Service) BookingVolume(ctx context.Context, w window) (Report[DailyVolume], error) {
	rows, err := s.store.BookingVolumeByDay(ctx, db.BookingVolumeByDayParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return Report[DailyVolume]{}, fmt.Errorf("booking volume by day: %w", err)
	}
	out := make([]DailyVolume, 0, len(rows))
	for _, row := range rows {
		day := ""
		if row.Day.Valid {
			day = row.Day.Time.Format(time.DateOnly)
		}
		out = append(out, DailyVolume{
			Day:                    day,
			BookingsCreated:        row.BookingsCreated,
			Confirmed:              row.Confirmed,
			Expired:                row.Expired,
			Cancelled:              row.Cancelled,
			PendingReview:          row.PendingReview,
			ConfirmedRevenue:       money.Santim(row.ConfirmedRevenueSantim).Format(),
			ConfirmedRevenueSantim: row.ConfirmedRevenueSantim,
		})
	}
	return newReport(w, out), nil
}

// SlotDemand is one weekday/hour slot's demand (PRD FR28).
type SlotDemand struct {
	IsoDayOfWeek int32  `json:"iso_day_of_week"`
	DayName      string `json:"day_name"`
	HourOfDay    int32  `json:"hour_of_day"`
	Bookings     int64  `json:"bookings"`
	SpotsSold    int64  `json:"spots_sold"`
} //	@name	SlotDemand

// isoDayNames indexes ISO-8601 weekdays, where 1 is Monday. Postgres' ISODOW
// and Go's time.Weekday disagree on where the week starts, so the name is
// resolved here rather than by casting the number.
var isoDayNames = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

func isoDayName(d int32) string {
	if d < 1 || d > 7 {
		return ""
	}
	return isoDayNames[d-1]
}

// PopularSlots reports which weekday/hour slots actually sell, busiest first.
// The window is read against the game's start time rather than the booking's
// creation, because the question is which slots to schedule, not when people
// browse.
func (s *Service) PopularSlots(ctx context.Context, w window) (Report[SlotDemand], error) {
	rows, err := s.store.PopularSlots(ctx, db.PopularSlotsParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return Report[SlotDemand]{}, fmt.Errorf("popular slots: %w", err)
	}
	out := make([]SlotDemand, 0, len(rows))
	for _, row := range rows {
		out = append(out, SlotDemand{
			IsoDayOfWeek: row.IsoDayOfWeek,
			DayName:      isoDayName(row.IsoDayOfWeek),
			HourOfDay:    row.HourOfDay,
			Bookings:     row.Bookings,
			SpotsSold:    row.SpotsSold,
		})
	}
	return newReport(w, out), nil
}

// MethodStats is one payment method's outcomes (PRD FR28).
type MethodStats struct {
	Method          string `json:"method"`
	Submissions     int64  `json:"submissions"`
	AutoVerified    int64  `json:"auto_verified"`
	ApprovedByAdmin int64  `json:"approved_by_admin"`
	Rejected        int64  `json:"rejected"`
	AwaitingReview  int64  `json:"awaiting_review"`
	Collected       string `json:"collected"`
	CollectedSantim int64  `json:"collected_santim"`
	// AutomationRate is the share of this method's decided payments that
	// cleared without a human, as a fraction from 0 to 1.
	AutomationRate float64 `json:"automation_rate"`
} //	@name	MethodStats

// PaymentMethods reports how each method performs: how much it collects, and
// how much of the review queue it generates. A method that needs a human on
// every submission is a verifier problem, and this is where it shows.
func (s *Service) PaymentMethods(ctx context.Context, w window) (Report[MethodStats], error) {
	rows, err := s.store.PaymentMethodBreakdown(ctx, db.PaymentMethodBreakdownParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return Report[MethodStats]{}, fmt.Errorf("payment method breakdown: %w", err)
	}
	out := make([]MethodStats, 0, len(rows))
	for _, row := range rows {
		decided := row.AutoVerified + row.ApprovedByAdmin + row.Rejected
		out = append(out, MethodStats{
			Method:          row.Method,
			Submissions:     row.Submissions,
			AutoVerified:    row.AutoVerified,
			ApprovedByAdmin: row.ApprovedByAdmin,
			Rejected:        row.Rejected,
			AwaitingReview:  row.AwaitingReview,
			Collected:       money.Santim(row.CollectedSantim).Format(),
			CollectedSantim: row.CollectedSantim,
			AutomationRate:  rate(row.AutoVerified, decided),
		})
	}
	return newReport(w, out), nil
}

// VerificationReport is the headline operational metric (PRD 15): the share of
// payments that cleared without an admin touching them.
type VerificationReport struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Total        int64     `json:"total"`
	AutoVerified int64     `json:"auto_verified"`
	NeededReview int64     `json:"needed_review"`
	StillPending int64     `json:"still_pending"`
	// AutomationRate excludes payments still awaiting their first verification
	// attempt from the denominator. They have no outcome yet, and counting them
	// as manual work would make the metric dip every time the queue grows
	// rather than when automation actually degrades.
	AutomationRate float64 `json:"automation_rate"`
} //	@name	VerificationReport

// Verification reports the automation rate over the window.
func (s *Service) Verification(ctx context.Context, w window) (*VerificationReport, error) {
	row, err := s.store.VerificationOutcomeSummary(ctx, db.VerificationOutcomeSummaryParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return nil, fmt.Errorf("verification outcome summary: %w", err)
	}
	return &VerificationReport{
		From:           w.From,
		To:             w.To,
		Total:          row.Total,
		AutoVerified:   row.AutoVerified,
		NeededReview:   row.NeededReview,
		StillPending:   row.StillPending,
		AutomationRate: rate(row.AutoVerified, row.Total-row.StillPending),
	}, nil
}

// FieldCancellation is one field's cancellation pressure (PRD FR28).
type FieldCancellation struct {
	FieldID   uuid.UUID `json:"field_id"`
	FieldName string    `json:"field_name"`
	Bookings  int64     `json:"bookings"`
	Cancelled int64     `json:"cancelled"`
	Expired   int64     `json:"expired"`
	Confirmed int64     `json:"confirmed"`
	// AbandonedAfterConfirm counts bookings that were paid for and then
	// cancelled. It is the closest honest proxy for a no-show, because nothing
	// in this system records attendance -- the dashboard must label it as what
	// it is rather than as players failing to turn up.
	AbandonedAfterConfirm int64   `json:"abandoned_after_confirm"`
	SpotsReturned         int64   `json:"spots_returned"`
	Refundable            string  `json:"refundable"`
	RefundableSantim      int64   `json:"refundable_santim"`
	CancellationRate      float64 `json:"cancellation_rate"`
	ExpiryRate            float64 `json:"expiry_rate"`
} //	@name	FieldCancellation

// Cancellations reports cancellation and expiry pressure per field.
func (s *Service) Cancellations(ctx context.Context, w window) (Report[FieldCancellation], error) {
	rows, err := s.store.CancellationRates(ctx, db.CancellationRatesParams{
		FromTime: w.From,
		ToTime:   w.To,
	})
	if err != nil {
		return Report[FieldCancellation]{}, fmt.Errorf("cancellation rates: %w", err)
	}
	out := make([]FieldCancellation, 0, len(rows))
	for _, row := range rows {
		out = append(out, FieldCancellation{
			FieldID:               row.FieldID,
			FieldName:             row.FieldName,
			Bookings:              row.Bookings,
			Cancelled:             row.Cancelled,
			Expired:               row.Expired,
			Confirmed:             row.Confirmed,
			AbandonedAfterConfirm: row.AbandonedAfterConfirm,
			SpotsReturned:         row.SpotsReturned,
			Refundable:            money.Santim(row.RefundableSantim).Format(),
			RefundableSantim:      row.RefundableSantim,
			CancellationRate:      rate(row.Cancelled, row.Bookings),
			ExpiryRate:            rate(row.Expired, row.Bookings),
		})
	}
	return newReport(w, out), nil
}

// rate divides safely and rounds to four decimals. A zero denominator reports
// zero rather than NaN, which would serialize as invalid JSON and take the
// whole report down with it.
func rate(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Round(float64(part)/float64(whole)*10000) / 10000
}

// AuditPage is the audit log listing. There is no total: the log is append-only
// and grows without bound, so counting it on every page view buys a full scan
// for a number nobody acts on. The client pages until a short page comes back.
type AuditPage struct {
	Items  []audit.View `json:"items"`
	Limit  int32        `json:"limit"`
	Offset int32        `json:"offset"`
} //	@name	AuditPage

// AuditLog returns the admin action trail, newest first (PRD FR27).
func (s *Service) AuditLog(ctx context.Context, filter audit.Filter, limit, offset int32) (AuditPage, error) {
	if s.audit == nil {
		return AuditPage{Items: []audit.View{}, Limit: limit, Offset: offset}, nil
	}
	items, err := s.audit.List(ctx, filter, limit, offset)
	if err != nil {
		return AuditPage{}, err
	}
	if items == nil {
		items = []audit.View{}
	}
	return AuditPage{Items: items, Limit: limit, Offset: offset}, nil
}

// ---- HTTP ----

// revenueReport returns confirmed revenue per field (PRD FR28).
//
//	@Summary		Revenue report
//	@Description	Confirmed revenue per field over the window, read against confirmation time: revenue belongs to the day the money was accepted. Margin is omitted rather than zero when any covered game is missing its cost.
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.RevenueReport
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/revenue [get]
func (h *Handler) revenueReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.Revenue(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}

// bookingVolumeReport returns bookings created per day (PRD FR28).
//
//	@Summary		Booking volume report
//	@Description	Bookings created per day with how they ended up. Status counts are the bookings' current status, not their status on that day, which is what makes each row answer "of the bookings taken that day, how many stuck".
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.Report[admin.DailyVolume]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/bookings [get]
func (h *Handler) bookingVolumeReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.BookingVolume(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}

// popularSlotsReport returns which weekday/hour slots sell, busiest first (PRD FR28).
//
//	@Summary		Slot demand report
//	@Description	Which weekday/hour slots actually sell, busiest first. The window is read against the game's start time rather than the booking's creation: the question is which slots to schedule, not when people browse.
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.Report[admin.SlotDemand]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/slots [get]
func (h *Handler) popularSlotsReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.PopularSlots(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}

// paymentMethodsReport returns how each payment method performs (PRD FR28).
//
//	@Summary		Payment methods report
//	@Description	How much each method collects and how much manual review it generates. A method that needs a human on every submission is a verifier problem. Automation rate is the share of that method's decided payments that cleared without a human, 0 to 1.
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.Report[admin.MethodStats]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/payment-methods [get]
func (h *Handler) paymentMethodsReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.PaymentMethods(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}

// verificationReport returns the automation rate over the window (PRD 15).
//
//	@Summary		Verification report
//	@Description	The headline operational metric: how many submissions cleared without an admin touching them. Automation rate excludes payments still awaiting their first attempt from the denominator, so a growing queue does not read as degrading automation.
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.VerificationReport
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/verification [get]
func (h *Handler) verificationReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.Verification(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}

// listAuditLogs returns the admin action trail, newest first (PRD FR27).
//
//	@Summary		List the audit log
//	@Description	The append-only record of admin actions. action and target_type are matched exactly against historical names, so a renamed action stays searchable. There is no total: the client pages until a short page comes back.
//	@Tags			audit
//	@Security		BearerAuth
//	@Produce		json
//	@Param			actor_id	query		string	false	"Only entries made by this admin (UUID)"
//	@Param			action		query		string	false	"Only this action name (2-64 characters)"
//	@Param			target_type	query		string	false	"Only this target type (2-64 characters)"
//	@Param			limit		query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset		query		int		false	"Rows to skip (default 0)"
//	@Success		200			{object}	admin.AuditPage
//	@Failure		401			{object}	httpx.ErrorEnvelope
//	@Failure		403			{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422			{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429			{object}	httpx.ErrorEnvelope
//	@Failure		500			{object}	httpx.ErrorEnvelope
//	@Router			/audit-logs [get]
func (h *Handler) listAuditLogs(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := parsePagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v := httpx.NewValidator()
	var filter audit.Filter

	if raw := strings.TrimSpace(q.Get("actor_id")); raw != "" {
		id := v.UUID("actor_id", raw)
		filter.ActorID = &id
	}
	// action and target_type are matched exactly and are not validated against
	// the known vocabulary: the log is append-only history, and an action name
	// that has since been renamed must still be searchable.
	if action := optionalText(q.Get("action")); action != nil {
		v.Length("action", *action, 2, 64)
		filter.Action = action
	}
	if target := optionalText(q.Get("target_type")); target != nil {
		v.Length("target_type", *target, 2, 64)
		filter.TargetType = target
	}
	if err := v.Err(); err != nil {
		return err
	}

	page, err := h.svc.AuditLog(r.Context(), filter, limit, offset)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, page)
	return nil
}

// cancellationReport returns cancellation and expiry pressure per field (PRD FR28).
//
//	@Summary		Cancellations report
//	@Description	Cancellation and expiry pressure per field, with the refundable amount. abandoned_after_confirm counts bookings that were paid for and then cancelled — the closest honest proxy for a no-show, since attendance is not recorded.
//	@Tags			reports
//	@Security		BearerAuth
//	@Produce		json
//	@Param			from	query		string	false	"Window start, RFC3339 (defaults to 30 days before to)"
//	@Param			to		query		string	false	"Window end, RFC3339, exclusive (defaults to now); the window may span at most 400 days"
//	@Success		200		{object}	admin.Report[admin.FieldCancellation]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/reports/cancellations [get]
func (h *Handler) cancellationReport(w http.ResponseWriter, r *http.Request) error {
	win, err := parseWindow(r)
	if err != nil {
		return err
	}
	report, err := h.svc.Cancellations(r.Context(), win)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, report)
	return nil
}
