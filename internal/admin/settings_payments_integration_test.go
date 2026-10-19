//go:build integration

// The payment-settings loop: a dashboard edit changes which account the worker
// checks receipts against, immediately and without a reload, and every other
// admin hears about it.
//
// It lives with the admin package because the edit is the admin API's, but the
// assertion that matters is the worker's own resolution path reading the same
// row -- payment.ResolveAccount is exactly what the worker runs per job.
package admin_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func TestPaymentSettingsDrivePayeeChecks(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	// The harness truncates every table, including the settings row the
	// migration seeds, so recreate that row the way the migration does.
	if _, err := store.Pool().Exec(ctx, `INSERT INTO payment_settings (id) VALUES (true)`); err != nil {
		t.Fatalf("seed payment settings: %v", err)
	}

	envAccounts := payment.FallbackAccounts{CBE: "1000483542902", Telebirr: "0983096034"}
	adminSvc := admin.NewService(admin.Options{
		Store:           store,
		Audit:           audit.NewRecorder(store, log),
		Notifier:        notification.NewService(store, log, false),
		Logger:          log,
		PaymentAccounts: envAccounts,
	})

	// An empty row means every method still runs on the environment accounts.
	before, err := adminSvc.GetPaymentSettings(ctx)
	if err != nil {
		t.Fatalf("get payment settings: %v", err)
	}
	if before.MerchantCBEAccount != envAccounts.CBE ||
		before.MerchantCBEAccountSource != string(payment.SourceEnv) {
		t.Fatalf("empty row resolves to (%q, %q), want the environment account",
			before.MerchantCBEAccount, before.MerchantCBEAccountSource)
	}

	// The actor and one other admin; a player who must not be told.
	actor := testsupport.User(t, store)
	if _, err := store.SetUserRole(ctx, db.SetUserRoleParams{ID: actor.ID, Role: "admin"}); err != nil {
		t.Fatalf("promote actor: %v", err)
	}
	other := testsupport.User(t, store)
	if _, err := store.SetUserRole(ctx, db.SetUserRoleParams{ID: other.ID, Role: "admin"}); err != nil {
		t.Fatalf("promote other admin: %v", err)
	}
	player := testsupport.User(t, store)

	// The edit wins over the environment, per field, without a restart.
	newCBE := "1000111111111"
	updated, err := adminSvc.UpdatePaymentSettings(ctx, actor.ID,
		admin.PaymentSettingsPatch{MerchantCBEAccount: &newCBE}, "switching accounts (integration test)")
	if err != nil {
		t.Fatalf("update payment settings: %v", err)
	}
	if updated.MerchantCBEAccount != newCBE ||
		updated.MerchantCBEAccountSource != string(payment.SourceDatabase) {
		t.Errorf("CBE after the edit = (%q, %q), want the saved value",
			updated.MerchantCBEAccount, updated.MerchantCBEAccountSource)
	}
	if updated.MerchantTelebirrAccount != envAccounts.Telebirr ||
		updated.MerchantTelebirrAccountSource != string(payment.SourceEnv) {
		t.Errorf("untouched Telebirr = (%q, %q), want the environment account",
			updated.MerchantTelebirrAccount, updated.MerchantTelebirrAccountSource)
	}
	if updated.UpdatedBy == nil || *updated.UpdatedBy != actor.ID {
		t.Errorf("updated_by = %v, want the acting admin", updated.UpdatedBy)
	}

	// The worker's resolution path sees the edit on the very next read.
	row, err := store.GetPaymentSettings(ctx)
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	account, source := payment.ResolveAccount(row, payment.MethodCBE, envAccounts)
	if account != newCBE || source != payment.SourceDatabase {
		t.Errorf("worker resolution = (%q, %q), want the dashboard value", account, source)
	}
	account, source = payment.ResolveAccount(row, payment.MethodTelebirr, envAccounts)
	if account != envAccounts.Telebirr || source != payment.SourceEnv {
		t.Errorf("Telebirr resolution = (%q, %q), want the environment value", account, source)
	}

	// Every other admin is told; the actor and players are not.
	if got := notificationCount(t, store, notification.TypePaymentAccountsUpdated, other.ID); got != 1 {
		t.Errorf("notifications to the other admin = %d, want 1", got)
	}
	if got := notificationCount(t, store, notification.TypePaymentAccountsUpdated, actor.ID); got != 0 {
		t.Errorf("notifications to the actor = %d, want none", got)
	}
	if got := notificationCount(t, store, notification.TypePaymentAccountsUpdated, player.ID); got != 0 {
		t.Errorf("notifications to a player = %d, want none", got)
	}

	// A patch that changes nothing is not an edit: no audit entry, no alert.
	if _, err := adminSvc.UpdatePaymentSettings(ctx, actor.ID,
		admin.PaymentSettingsPatch{MerchantCBEAccount: &newCBE}, "same again"); err != nil {
		t.Fatalf("no-change patch: %v", err)
	}
	if got := auditEntries(t, store); got != 1 {
		t.Errorf("audit entries after a no-change patch = %d, want 1", got)
	}
	if got := notificationCount(t, store, notification.TypePaymentAccountsUpdated, other.ID); got != 1 {
		t.Errorf("notifications after a no-change patch = %d, want 1", got)
	}

	// Clearing the override hands the method back to the environment value.
	cleared := ""
	reverted, err := adminSvc.UpdatePaymentSettings(ctx, actor.ID,
		admin.PaymentSettingsPatch{MerchantCBEAccount: &cleared}, "revert to the configured account")
	if err != nil {
		t.Fatalf("clear the override: %v", err)
	}
	if reverted.MerchantCBEAccount != envAccounts.CBE ||
		reverted.MerchantCBEAccountSource != string(payment.SourceEnv) {
		t.Errorf("cleared CBE = (%q, %q), want the environment account",
			reverted.MerchantCBEAccount, reverted.MerchantCBEAccountSource)
	}
	if got := auditEntries(t, store); got != 2 {
		t.Errorf("audit entries after the revert = %d, want 2", got)
	}
	if got := notificationCount(t, store, notification.TypePaymentAccountsUpdated, other.ID); got != 2 {
		t.Errorf("notifications after the revert = %d, want 2", got)
	}
}

func auditEntries(t *testing.T, store *db.Store) int64 {
	t.Helper()
	var entries int64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = $1`,
		audit.ActionPaymentSettingsUpdated).Scan(&entries); err != nil {
		t.Fatalf("count audit entries: %v", err)
	}
	return entries
}

func notificationCount(t *testing.T, store *db.Store, typ string, userID uuid.UUID) int64 {
	t.Helper()
	var count int64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE type = $1 AND user_id = $2`,
		typ, userID).Scan(&count); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return count
}
