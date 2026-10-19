package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// BookingSettings is the dashboard-editable pricing policy for monthly plans.
// The public API reads the same row when it prices a quote, so an edit here
// changes the next quote without a deploy.
type BookingSettings struct {
	MonthlyDiscountBPS   int32      `json:"monthly_discount_bps"`
	MonthlyDiscountLabel string     `json:"monthly_discount_label"`
	UpdatedBy            *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

func toBookingSettings(row db.BookingSetting) BookingSettings {
	return BookingSettings{
		MonthlyDiscountBPS:   row.MonthlyDiscountBps,
		MonthlyDiscountLabel: row.MonthlyDiscountLabel,
		UpdatedBy:            row.UpdatedBy,
		UpdatedAt:            row.UpdatedAt,
	}
}

// BookingSettingsPatch carries the fields a PATCH is changing. Nil leaves the
// field at its current value.
type BookingSettingsPatch struct {
	MonthlyDiscountBPS   *int32
	MonthlyDiscountLabel *string
}

// GetBookingSettings returns the current pricing policy.
func (s *Service) GetBookingSettings(ctx context.Context) (BookingSettings, error) {
	row, err := s.store.GetBookingSettings(ctx)
	if err != nil {
		if db.IsNoRows(err) {
			return BookingSettings{}, fmt.Errorf("booking settings row is missing; are the migrations applied?")
		}
		return BookingSettings{}, fmt.Errorf("get booking settings: %w", err)
	}
	return toBookingSettings(row), nil
}

// UpdateBookingSettings applies a patch and records who changed what.
//
// A patch that changes nothing is not an error and is not audited: the response
// is the settings as they stand, while an entry would claim an edit that never
// happened. The read-modify-write runs in one transaction so a concurrent edit
// cannot interleave between the before-value the audit records and the write.
func (s *Service) UpdateBookingSettings(ctx context.Context, actor uuid.UUID, patch BookingSettingsPatch, reason string) (BookingSettings, error) {
	var out BookingSettings
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetBookingSettings(ctx)
		if err != nil {
			if db.IsNoRows(err) {
				return fmt.Errorf("booking settings row is missing; are the migrations applied?")
			}
			return fmt.Errorf("get booking settings: %w", err)
		}

		bps := before.MonthlyDiscountBps
		if patch.MonthlyDiscountBPS != nil {
			bps = *patch.MonthlyDiscountBPS
		}
		label := before.MonthlyDiscountLabel
		if patch.MonthlyDiscountLabel != nil {
			label = *patch.MonthlyDiscountLabel
		}
		if bps == before.MonthlyDiscountBps && label == before.MonthlyDiscountLabel {
			out = toBookingSettings(before)
			return nil
		}

		row, err := q.UpdateBookingSettings(ctx, db.UpdateBookingSettingsParams{
			MonthlyDiscountBps:   bps,
			MonthlyDiscountLabel: label,
			UpdatedBy:            &actor,
		})
		if err != nil {
			return fmt.Errorf("update booking settings: %w", err)
		}
		out = toBookingSettings(row)

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionBookingSettingsUpdated,
			TargetType: audit.TargetSettings,
			TargetID:   "booking",
			Reason:     reason,
			Metadata: map[string]any{
				"from": map[string]any{
					"monthly_discount_bps":   before.MonthlyDiscountBps,
					"monthly_discount_label": before.MonthlyDiscountLabel,
				},
				"to": map[string]any{
					"monthly_discount_bps":   row.MonthlyDiscountBps,
					"monthly_discount_label": row.MonthlyDiscountLabel,
				},
			},
		})
	})
	if err != nil {
		return BookingSettings{}, err
	}
	return out, nil
}

// ---- HTTP ----

func (h *Handler) getBookingSettings(w http.ResponseWriter, r *http.Request) error {
	settings, err := h.svc.GetBookingSettings(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, settings)
	return nil
}

type updateBookingSettingsRequest struct {
	MonthlyDiscountBPS   *int32  `json:"monthly_discount_bps"`
	MonthlyDiscountLabel *string `json:"monthly_discount_label"`
	Reason               string  `json:"reason"`
}

func (h *Handler) updateBookingSettings(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	var req updateBookingSettingsRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	patch := BookingSettingsPatch{}
	if req.MonthlyDiscountBPS != nil {
		v.Range("monthly_discount_bps", int(*req.MonthlyDiscountBPS), 0, 9999)
		patch.MonthlyDiscountBPS = req.MonthlyDiscountBPS
	}
	if req.MonthlyDiscountLabel != nil {
		label := v.Required("monthly_discount_label", *req.MonthlyDiscountLabel)
		v.Length("monthly_discount_label", label, 1, 120)
		patch.MonthlyDiscountLabel = &label
	}
	if patch.MonthlyDiscountBPS == nil && patch.MonthlyDiscountLabel == nil {
		return errNoChanges
	}
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	settings, err := h.svc.UpdateBookingSettings(r.Context(), principal.UserID, patch, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, settings)
	return nil
}
