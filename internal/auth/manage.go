package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// This file holds the account operations used by the operator CLI rather than
// by an HTTP handler. They never issue tokens: the CLI is a local/one-shot
// process, and a session minted there would be a credential nobody asked for.

// CreateAccountInput is an account created by the operator CLI.
type CreateAccountInput struct {
	FullName string
	Email    string
	Password string
	Phone    string
	Role     string
	// MarkEmailVerified skips the confirmation flow: an operator typed the
	// address, which is a stronger assertion than an emailed link.
	MarkEmailVerified bool
}

// CreateAccount creates a password account without signing it in. It mirrors
// Signup's password policy and duplicate handling, but accepts an explicit role
// so the CLI can bootstrap an admin.
func (s *Service) CreateAccount(ctx context.Context, in CreateAccountInput) (*db.User, error) {
	role := strings.TrimSpace(in.Role)
	if role != RolePlayer && role != RoleAdmin {
		return nil, httpx.ErrValidation.WithField("role", "must be player or admin")
	}
	email := normalizeEmail(in.Email)
	if email == "" {
		return nil, httpx.ErrValidation.WithField("email", "required")
	}
	fullName := strings.TrimSpace(in.FullName)
	if fullName == "" {
		return nil, httpx.ErrValidation.WithField("full_name", "required")
	}

	hash, err := hashPassword(in.Password)
	if err != nil {
		if weak, ok := AsWeakPassword(err); ok {
			return nil, httpx.ErrValidation.WithField("password", weak.Reason).WithCause(err)
		}
		return nil, err
	}

	var out *db.User
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		created, err := q.CreateUser(ctx, db.CreateUserParams{
			FullName:     fullName,
			Email:        email,
			PasswordHash: &hash,
			Phone:        optionalString(in.Phone),
			Role:         &role,
		})
		if err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.NewError(http.StatusConflict, httpx.CodeEmailInUse,
					"An account with that email already exists.").WithCause(err)
			}
			return fmt.Errorf("create user: %w", err)
		}
		if in.MarkEmailVerified {
			if err := q.MarkEmailVerified(ctx, created.ID); err != nil {
				return fmt.Errorf("mark email verified: %w", err)
			}
			// MarkEmailVerified is an exec, so re-read to return the row the
			// caller would see rather than a stale pre-update copy.
			created, err = q.GetUserByID(ctx, created.ID)
			if err != nil {
				return fmt.Errorf("reload created user: %w", err)
			}
		}
		out = &created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetPasswordByEmail sets a new password without a reset link, for the operator
// CLI. Every refresh token and outstanding reset token is killed: the person
// running this believes the account is locked out or compromised, and leaving
// either alive would let the previous holder keep or retake access.
func (s *Service) SetPasswordByEmail(ctx context.Context, email, newPassword string) (*db.User, error) {
	user, err := s.store.GetUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get user by email: %w", err)
	}

	hash, err := hashPassword(newPassword)
	if err != nil {
		if weak, ok := AsWeakPassword(err); ok {
			return nil, httpx.ErrValidation.WithField("password", weak.Reason).WithCause(err)
		}
		return nil, err
	}

	var out *db.User
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		if err := q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
			ID:           user.ID,
			PasswordHash: &hash,
		}); err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if _, err := q.RevokeAllUserRefreshTokens(ctx, user.ID); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if _, err := q.InvalidateUserPasswordResetTokens(ctx, user.ID); err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}
		updated, err := q.GetUserByID(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("reload user: %w", err)
		}
		out = &updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetRoleByEmail promotes or demotes an account. It is the CLI equivalent of
// the dashboard's role endpoint, with one extra guard the dashboard does not
// need: demoting the last active admin would leave nobody able to sign in, so
// it is refused rather than merely discouraged. The active admins are locked
// inside the transaction, so two concurrent demotions cannot both pass that
// check. The second result reports whether the role actually changed, so the
// CLI can say "already there" instead of claiming a write that never happened.
func (s *Service) SetRoleByEmail(ctx context.Context, email, role string) (*db.User, bool, error) {
	role = strings.TrimSpace(role)
	if role != RolePlayer && role != RoleAdmin {
		return nil, false, httpx.ErrValidation.WithField("role", "must be player or admin")
	}
	email = normalizeEmail(email)

	var out *db.User
	changed := false
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		user, err := q.GetUserByEmail(ctx, email)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get user by email: %w", err)
		}
		if user.Role == role {
			out = &user
			return nil
		}
		if role == RoleAdmin && user.Status != StatusActive {
			return httpx.ErrValidation.WithField("role",
				"only an active account can be made an admin")
		}
		if role == RolePlayer && user.Role == RoleAdmin && user.Status == StatusActive {
			admins, err := q.LockActiveAdmins(ctx)
			if err != nil {
				return fmt.Errorf("lock active admins: %w", err)
			}
			if len(admins) <= 1 {
				return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
					"That is the last active admin; promote another account first.")
			}
		}

		updated, err := q.SetUserRole(ctx, db.SetUserRoleParams{ID: user.ID, Role: role})
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("set user role: %w", err)
		}
		out = &updated
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, changed, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
