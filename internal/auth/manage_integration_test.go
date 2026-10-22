//go:build integration

// Operator-CLI account operations. These are the paths that can create an
// admin, take over an account, or change who can sign in, so they are exercised
// against a real Postgres: the last-admin guard depends on row locks inside the
// transaction and cannot be proven with a stub.
package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

const cliTestPassword = "correct-horse-42"

func newManageService(store *db.Store) *Service {
	return NewService(Options{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func cliAccount(t *testing.T, svc *Service, role string) db.User {
	t.Helper()
	user, err := svc.CreateAccount(context.Background(), CreateAccountInput{
		FullName:          "CLI Account",
		Email:             "cli-" + uuid.NewString() + "@example.test",
		Password:          cliTestPassword,
		Phone:             "0911223344",
		Role:              role,
		MarkEmailVerified: true,
	})
	if err != nil {
		t.Fatalf("create %s account: %v", role, err)
	}
	return *user
}

func errorCode(t *testing.T, err error) httpx.Code {
	t.Helper()
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an httpx.Error", err)
	}
	return apiErr.Code
}

func TestCreateAccountCreatesVerifiedAdmin(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)

	user := cliAccount(t, svc, RoleAdmin)

	if user.Role != RoleAdmin {
		t.Errorf("role = %q, want admin", user.Role)
	}
	if user.EmailVerifiedAt == nil {
		t.Error("CLI-created account is not email-verified")
	}
	if user.Status != StatusActive {
		t.Errorf("status = %q, want active", user.Status)
	}
	if user.PasswordHash == nil || !verifyPassword(*user.PasswordHash, cliTestPassword) {
		t.Error("stored password does not verify")
	}

	stored, err := store.GetUserByEmail(context.Background(), user.Email)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if stored.EmailVerifiedAt == nil {
		t.Error("verification timestamp was not persisted")
	}
}

func TestCreateAccountRejectsDuplicateEmail(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)
	user := cliAccount(t, svc, RoleAdmin)

	_, err := svc.CreateAccount(context.Background(), CreateAccountInput{
		FullName:          "Second",
		Email:             user.Email,
		Password:          cliTestPassword,
		Phone:             "0911223344",
		Role:              RoleAdmin,
		MarkEmailVerified: true,
	})
	if err == nil {
		t.Fatal("duplicate email was accepted")
	}
	if code := errorCode(t, err); code != httpx.CodeEmailInUse {
		t.Errorf("code = %q, want %q", code, httpx.CodeEmailInUse)
	}
}

func TestCreateAccountEnforcesPasswordPolicy(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)

	_, err := svc.CreateAccount(context.Background(), CreateAccountInput{
		FullName: "Weak",
		Email:    "weak-" + uuid.NewString() + "@example.test",
		Password: "password",
		Role:     RoleAdmin,
	})
	if err == nil {
		t.Fatal("weak password was accepted")
	}
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) || apiErr.Fields["password"] == "" {
		t.Fatalf("expected a password field error, got %v", err)
	}
}

func TestSetPasswordByEmailRevokesSessionsAndResetTokens(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)
	ctx := context.Background()

	user := cliAccount(t, svc, RolePlayer)
	if _, err := store.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		UserID:    user.ID,
		TokenHash: []byte("refresh-hash-" + uuid.NewString()),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create refresh token: %v", err)
	}
	resetToken, resetHash, err := newResetToken()
	if err != nil {
		t.Fatalf("new reset token: %v", err)
	}
	if _, err := store.CreatePasswordResetToken(ctx, db.CreatePasswordResetTokenParams{
		UserID:    user.ID,
		TokenHash: resetHash,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create reset token: %v", err)
	}

	const next = "new-password-99"
	updated, err := svc.SetPasswordByEmail(ctx, user.Email, next)
	if err != nil {
		t.Fatalf("set password: %v", err)
	}
	if updated.PasswordHash == nil || !verifyPassword(*updated.PasswordHash, next) {
		t.Error("new password does not verify")
	}
	if verifyPassword(*updated.PasswordHash, cliTestPassword) {
		t.Error("old password still verifies")
	}

	sessions, err := store.Pool().Query(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID)
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	defer sessions.Close()
	var live int
	sessions.Next()
	if err := sessions.Scan(&live); err != nil {
		t.Fatalf("scan sessions: %v", err)
	}
	if live != 0 {
		t.Errorf("%d refresh tokens survived the reset", live)
	}

	stored, err := store.GetPasswordResetTokenByHash(ctx, hashResetToken(resetToken))
	if err != nil {
		t.Fatalf("read reset token: %v", err)
	}
	if stored.UsedAt == nil {
		t.Error("outstanding reset token was not invalidated")
	}
}

func TestSetPasswordByEmailUnknownAccountIsNotFound(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)

	_, err := svc.SetPasswordByEmail(context.Background(), "nobody@example.test", cliTestPassword)
	if err == nil {
		t.Fatal("unknown account was accepted")
	}
	if code := errorCode(t, err); code != httpx.CodeNotFound {
		t.Errorf("code = %q, want %q", code, httpx.CodeNotFound)
	}
}

func TestSetRolePromotesPlayerAndDemotesWithAnotherAdmin(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)
	ctx := context.Background()

	first := cliAccount(t, svc, RoleAdmin)
	second := cliAccount(t, svc, RolePlayer)

	promoted, changed, err := svc.SetRoleByEmail(ctx, second.Email, RoleAdmin)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !changed {
		t.Error("promote reported no change")
	}
	if promoted.Role != RoleAdmin {
		t.Errorf("role = %q, want admin", promoted.Role)
	}

	demoted, changed, err := svc.SetRoleByEmail(ctx, first.Email, RolePlayer)
	if err != nil {
		t.Fatalf("demote with another admin present: %v", err)
	}
	if !changed {
		t.Error("demote reported no change")
	}
	if demoted.Role != RolePlayer {
		t.Errorf("role = %q, want player", demoted.Role)
	}

	if _, changed, err := svc.SetRoleByEmail(ctx, second.Email, RoleAdmin); err != nil {
		t.Fatalf("promote already-admin: %v", err)
	} else if changed {
		t.Error("promoting an existing admin reported a change")
	}
}

func TestSetRoleRefusesToDemoteLastActiveAdmin(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)

	admin := cliAccount(t, svc, RoleAdmin)

	_, _, err := svc.SetRoleByEmail(context.Background(), admin.Email, RolePlayer)
	if err == nil {
		t.Fatal("last active admin was demoted")
	}
	if code := errorCode(t, err); code != httpx.CodeConflict {
		t.Errorf("code = %q, want %q", code, httpx.CodeConflict)
	}
}

func TestSetRoleRefusesInactiveAdminTarget(t *testing.T) {
	store := testsupport.Store(t)
	svc := newManageService(store)
	ctx := context.Background()

	player := testsupport.User(t, store)
	if _, err := store.SetUserStatus(ctx, db.SetUserStatusParams{
		ID:     player.ID,
		Status: StatusDisabled,
	}); err != nil {
		t.Fatalf("disable player: %v", err)
	}

	_, _, err := svc.SetRoleByEmail(ctx, player.Email, RoleAdmin)
	if err == nil {
		t.Fatal("inactive account was promoted")
	}
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected a validation error, got %v", err)
	}
}
