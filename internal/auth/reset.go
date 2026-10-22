package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// RequestPasswordReset issues a single-use reset token and queues the email.
//
// It always reports success. Telling a caller whether an address is registered
// would turn this endpoint into an account-enumeration oracle, and there is no
// legitimate client that needs to know: the person either receives the mail or
// they do not have an account.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if db.IsNoRows(err) {
			s.log.Info("password reset requested for unknown address")
			return nil
		}
		return fmt.Errorf("get user by email: %w", err)
	}
	if user.Status != StatusActive {
		s.log.Info("password reset requested for disabled account",
			slog.String("user_id", user.ID.String()))
		return nil
	}

	token, tokenHash, err := newResetToken()
	if err != nil {
		return err
	}
	expiresAt := s.now().Add(s.resetTTL)

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		// Superseding earlier tokens means a second request invalidates the
		// first, so a mail forwarded or intercepted earlier stops working.
		if _, err := q.InvalidateUserPasswordResetTokens(ctx, user.ID); err != nil {
			return fmt.Errorf("invalidate previous reset tokens: %w", err)
		}
		if _, err := q.CreatePasswordResetToken(ctx, db.CreatePasswordResetTokenParams{
			UserID:    user.ID,
			TokenHash: tokenHash,
			ExpiresAt: expiresAt,
		}); err != nil {
			return fmt.Errorf("create reset token: %w", err)
		}

		payload, err := marshalPayload(map[string]any{
			"full_name":  user.FullName,
			"token":      token,
			"expires_at": expiresAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return err
		}
		// Queued inside the same transaction as the token: an email can never
		// carry a token that was not persisted, and a persisted token is
		// never left without its mail.
		if _, err := q.EnqueueEmail(ctx, db.EnqueueEmailParams{
			UserID:    &user.ID,
			ToAddress: user.Email,
			Template:  "password_reset",
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("enqueue reset email: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.log.Info("password reset email queued", slog.String("user_id", user.ID.String()))
	return nil
}

// ResetPassword consumes a reset token and sets a new password. Every existing
// session is revoked: a reset is the action someone takes when they believe
// their account is compromised, so leaving other sessions alive would defeat
// the point.
func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	invalid := httpx.NewError(http.StatusBadRequest, httpx.CodeTokenExpired,
		"That reset link is no longer valid. Request a new one.")

	stored, err := s.store.GetPasswordResetTokenByHash(ctx, hashResetToken(rawToken))
	if err != nil {
		if db.IsNoRows(err) {
			return invalid
		}
		return fmt.Errorf("get reset token: %w", err)
	}
	if stored.UsedAt != nil || !stored.ExpiresAt.After(s.now()) {
		return invalid
	}

	// Load the account before hashing so the password policy follows its role,
	// and so an invalid token does not buy an attacker a bcrypt round.
	user, err := s.store.GetUserByID(ctx, stored.UserID)
	if err != nil {
		if db.IsNoRows(err) {
			return invalid
		}
		return fmt.Errorf("get user for reset: %w", err)
	}
	hash, err := hashPasswordForRole(newPassword, user.Role)
	if err != nil {
		if weak, ok := AsWeakPassword(err); ok {
			return httpx.ErrValidation.WithField("password", weak.Reason).WithCause(err)
		}
		return err
	}

	return s.store.InTx(ctx, func(q *db.Queries) error {
		// The consume query is the gate: it only marks a token used when it is
		// still unused and unexpired, so two concurrent submissions of the
		// same link cannot both succeed.
		consumed, err := q.ConsumePasswordResetToken(ctx, stored.ID)
		if err != nil {
			return fmt.Errorf("consume reset token: %w", err)
		}
		if consumed == 0 {
			return invalid
		}
		if err := q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
			ID:           stored.UserID,
			PasswordHash: &hash,
		}); err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if _, err := q.RevokeAllUserRefreshTokens(ctx, stored.UserID); err != nil {
			return fmt.Errorf("revoke sessions after reset: %w", err)
		}
		return nil
	})
}

// ChangePassword updates the password for a signed-in user who can prove the
// current one. Other sessions are revoked for the same reason as a reset; the
// caller receives a fresh pair so their own session survives the change.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next, userAgent string) (*TokenPair, error) {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrUnauthorized
		}
		return nil, fmt.Errorf("get user: %w", err)
	}

	// A Google-only account has no current password to prove. Setting one is a
	// separate, deliberate action rather than something this endpoint allows
	// with an empty "current" value.
	if user.PasswordHash == nil {
		return nil, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"This account signs in with Google and has no password to change.")
	}
	if !verifyPassword(*user.PasswordHash, current) {
		return nil, httpx.ErrValidation.WithField("current_password", "incorrect")
	}

	hash, err := hashPasswordForRole(next, user.Role)
	if err != nil {
		if weak, ok := AsWeakPassword(err); ok {
			return nil, httpx.ErrValidation.WithField("new_password", weak.Reason).WithCause(err)
		}
		return nil, err
	}

	var pair *TokenPair
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
		issued, _, err := s.mintSession(ctx, q, user, userAgent)
		if err != nil {
			return err
		}
		pair = issued
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// resetTokenBytes gives the reset token 256 bits of entropy, so it cannot be
// guessed within its lifetime even under sustained attempts.
const resetTokenBytes = 32

// newResetToken returns the token to email and the digest to store.
func newResetToken() (token string, hash []byte, err error) {
	buf := make([]byte, resetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate reset token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashResetToken(token), nil
}

// hashResetToken derives the stored form. Unlike refresh tokens this is an
// unkeyed digest: reset tokens must remain verifiable by any process holding
// only the database, including a future password-reset flow served from the
// admin binary, which does not share the app's refresh secret.
func hashResetToken(token string) []byte {
	return sha256Sum([]byte(token))
}
