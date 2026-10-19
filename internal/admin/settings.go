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
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// BookingSettings is the dashboard-editable pricing policy for monthly plans.
// The public API reads the same row when it prices a quote, so an edit here
// changes the next quote without a deploy.
type BookingSettings struct {
	MonthlyDiscountBPS   int32      `json:"monthly_discount_bps"`
	MonthlyDiscountLabel string     `json:"monthly_discount_label"`
	UpdatedBy            *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
} //	@name	BookingSettings

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

// getBookingSettings returns the current pricing policy.
//
//	@Summary		Get booking settings
//	@Description	The platform-owned pricing policy: the monthly-plan discount the public API applies to the next quote, so an edit here lands without a deploy.
//	@Tags			settings
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	admin.BookingSettings
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/settings/booking [get]
func (h *Handler) getBookingSettings(w http.ResponseWriter, r *http.Request) error {
	settings, err := h.svc.GetBookingSettings(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, settings)
	return nil
}

// updateBookingSettingsRequest edits the pricing policy.
type updateBookingSettingsRequest struct {
	MonthlyDiscountBPS   *int32  `json:"monthly_discount_bps"`
	MonthlyDiscountLabel *string `json:"monthly_discount_label"`
	Reason               string  `json:"reason"`
} //	@name	UpdateBookingSettingsRequest

// updateBookingSettings edits the pricing policy.
//
//	@Summary		Update booking settings
//	@Description	A partial update of the monthly-plan discount, in basis points (1250 = 12.5%). A patch that changes nothing answers with the settings as they stand and writes no audit entry. The reason is recorded in the audit log.
//	@Tags			settings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		admin.updateBookingSettingsRequest	true	"Members to change"
//	@Success		200		{object}	admin.BookingSettings
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed, or no changes were given"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/settings/booking [patch]
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

// ---- Payment settings ----

// PaymentSettings is the dashboard view of the accounts payments are verified
// against. Each account reports where its effective value comes from: "db"
// once an admin has saved one, "env" while it still inherits the environment
// bootstrap. There is no separate raw field: when the source is env, no
// dashboard value is saved.
type PaymentSettings struct {
	MerchantCBEAccount            string     `json:"merchant_cbe_account"`
	MerchantCBEAccountSource      string     `json:"merchant_cbe_account_source"`
	MerchantTelebirrAccount       string     `json:"merchant_telebirr_account"`
	MerchantTelebirrAccountSource string     `json:"merchant_telebirr_account_source"`
	UpdatedBy                     *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt                     time.Time  `json:"updated_at"`
} //	@name	PaymentSettings

func toPaymentSettings(row db.PaymentSetting, fallback payment.FallbackAccounts) PaymentSettings {
	cbe, cbeSource := payment.ResolveAccount(row, payment.MethodCBE, fallback)
	telebirr, telebirrSource := payment.ResolveAccount(row, payment.MethodTelebirr, fallback)
	return PaymentSettings{
		MerchantCBEAccount:            cbe,
		MerchantCBEAccountSource:      string(cbeSource),
		MerchantTelebirrAccount:       telebirr,
		MerchantTelebirrAccountSource: string(telebirrSource),
		UpdatedBy:                     row.UpdatedBy,
		UpdatedAt:                     row.UpdatedAt,
	}
}

// PaymentSettingsPatch carries the fields a PATCH is changing. Nil leaves the
// field alone; an empty string clears the override so the method falls back to
// the environment value.
type PaymentSettingsPatch struct {
	MerchantCBEAccount      *string
	MerchantTelebirrAccount *string
}

// GetPaymentSettings returns the receiving accounts and where each effective
// value came from.
func (s *Service) GetPaymentSettings(ctx context.Context) (PaymentSettings, error) {
	row, err := s.store.GetPaymentSettings(ctx)
	if err != nil {
		if db.IsNoRows(err) {
			return PaymentSettings{}, fmt.Errorf("payment settings row is missing; are the migrations applied?")
		}
		return PaymentSettings{}, fmt.Errorf("get payment settings: %w", err)
	}
	return toPaymentSettings(row, s.paymentAccounts), nil
}

// UpdatePaymentSettings applies a patch, records who changed what, and alerts
// every other admin.
//
// A changed receiving account redirects where future payments are verified,
// which is exactly the kind of thing a receipt should never be the first to
// reveal, so unlike the pricing policy this edit notifies. The read-modify-
// write, the audit entry and the alerts run in one transaction: the change and
// the warning about it commit together.
func (s *Service) UpdatePaymentSettings(ctx context.Context, actor uuid.UUID, patch PaymentSettingsPatch, reason string) (PaymentSettings, error) {
	var out PaymentSettings
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetPaymentSettings(ctx)
		if err != nil {
			if db.IsNoRows(err) {
				return fmt.Errorf("payment settings row is missing; are the migrations applied?")
			}
			return fmt.Errorf("get payment settings: %w", err)
		}

		cbe := before.MerchantCbeAccount
		if patch.MerchantCBEAccount != nil {
			cbe = *patch.MerchantCBEAccount
		}
		telebirr := before.MerchantTelebirrAccount
		if patch.MerchantTelebirrAccount != nil {
			telebirr = *patch.MerchantTelebirrAccount
		}
		if cbe == before.MerchantCbeAccount && telebirr == before.MerchantTelebirrAccount {
			// A patch that changes nothing is not an error and is not audited
			// or alerted, for the same reason as the pricing policy.
			out = toPaymentSettings(before, s.paymentAccounts)
			return nil
		}

		row, err := q.UpdatePaymentSettings(ctx, db.UpdatePaymentSettingsParams{
			MerchantCbeAccount:      cbe,
			MerchantTelebirrAccount: telebirr,
			UpdatedBy:               &actor,
		})
		if err != nil {
			return fmt.Errorf("update payment settings: %w", err)
		}
		out = toPaymentSettings(row, s.paymentAccounts)

		if err := s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionPaymentSettingsUpdated,
			TargetType: audit.TargetSettings,
			TargetID:   "payments",
			Reason:     reason,
			Metadata: map[string]any{
				"from": map[string]any{
					"merchant_cbe_account":      before.MerchantCbeAccount,
					"merchant_telebirr_account": before.MerchantTelebirrAccount,
				},
				"to": map[string]any{
					"merchant_cbe_account":      row.MerchantCbeAccount,
					"merchant_telebirr_account": row.MerchantTelebirrAccount,
				},
			},
		}); err != nil {
			return err
		}

		admins, err := q.ListAdminUserIDs(ctx, actor)
		if err != nil {
			return fmt.Errorf("list admins to alert: %w", err)
		}
		for _, adminID := range admins {
			if err := s.notify(ctx, q, adminID, notification.TypePaymentAccountsUpdated, map[string]any{
				"cbe_account":      out.MerchantCBEAccount,
				"telebirr_account": out.MerchantTelebirrAccount,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PaymentSettings{}, err
	}
	return out, nil
}

// getPaymentSettings returns the receiving accounts payments are verified
// against.
//
//	@Summary		Get payment settings
//	@Description	The receiving accounts the worker checks receipts against and the public API shows payers, with where each effective value comes from: "db" once an admin has saved one, "env" while it inherits the environment bootstrap.
//	@Tags			settings
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	admin.PaymentSettings
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/settings/payments [get]
func (h *Handler) getPaymentSettings(w http.ResponseWriter, r *http.Request) error {
	settings, err := h.svc.GetPaymentSettings(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, settings)
	return nil
}

// updatePaymentSettingsRequest edits the receiving accounts.
type updatePaymentSettingsRequest struct {
	MerchantCBEAccount      *string `json:"merchant_cbe_account"`
	MerchantTelebirrAccount *string `json:"merchant_telebirr_account"`
	Reason                  string  `json:"reason"`
} //	@name	UpdatePaymentSettingsRequest

// updatePaymentSettings edits the receiving accounts.
//
//	@Summary		Update payment settings
//	@Description	A partial update of the receiving accounts. Each value is 8-20 digits, or an empty string to clear the override and fall back to the environment bootstrap. A patch that changes nothing answers with the settings as they stand and writes no audit entry. The reason is recorded in the audit log, and every other admin is alerted.
//	@Tags			settings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		admin.updatePaymentSettingsRequest	true	"Accounts to change"
//	@Success		200		{object}	admin.PaymentSettings
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed, or no changes were given"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/settings/payments [patch]
func (h *Handler) updatePaymentSettings(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	var req updatePaymentSettingsRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	patch := PaymentSettingsPatch{}
	if req.MerchantCBEAccount != nil {
		account := v.MerchantAccount("merchant_cbe_account", *req.MerchantCBEAccount)
		patch.MerchantCBEAccount = &account
	}
	if req.MerchantTelebirrAccount != nil {
		account := v.MerchantAccount("merchant_telebirr_account", *req.MerchantTelebirrAccount)
		patch.MerchantTelebirrAccount = &account
	}
	if patch.MerchantCBEAccount == nil && patch.MerchantTelebirrAccount == nil {
		return errNoChanges
	}
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	settings, err := h.svc.UpdatePaymentSettings(r.Context(), principal.UserID, patch, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, settings)
	return nil
}
