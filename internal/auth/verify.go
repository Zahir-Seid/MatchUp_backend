package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// RequestEmailVerification issues a single-use verification token and queues
// the confirmation email. Already-verified accounts are a no-op: the address
// cannot be confirmed twice, and re-sending would only mail a link that does
// nothing.
//
// Disabled accounts are also a no-op. The caller is authenticated, so a valid
// session for a disabled account is an edge case; refusing silently matches how
// password reset treats the same state.
func (s *Service) RequestEmailVerification(ctx context.Context, userID uuid.UUID) error {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrUnauthorized
		}
		return fmt.Errorf("get user: %w", err)
	}
	if user.EmailVerifiedAt != nil {
		s.log.Info("email verification requested for verified account",
			slog.String("user_id", user.ID.String()))
		return nil
	}
	if user.Status != StatusActive {
		s.log.Info("email verification requested for disabled account",
			slog.String("user_id", user.ID.String()))
		return nil
	}

	token, tokenHash, err := newResetToken()
	if err != nil {
		return err
	}
	expiresAt := s.now().Add(s.verifyTTL)

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		// Superseding earlier tokens means a second request invalidates the
		// first, so an older mail stops working.
		if _, err := q.InvalidateUserEmailVerificationTokens(ctx, user.ID); err != nil {
			return fmt.Errorf("invalidate previous verification tokens: %w", err)
		}
		if _, err := q.CreateEmailVerificationToken(ctx, db.CreateEmailVerificationTokenParams{
			UserID:    user.ID,
			TokenHash: tokenHash,
			ExpiresAt: expiresAt,
		}); err != nil {
			return fmt.Errorf("create verification token: %w", err)
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
		// carry a token that was not persisted, and a persisted token is never
		// left without its mail.
		if _, err := q.EnqueueEmail(ctx, db.EnqueueEmailParams{
			UserID:    &user.ID,
			ToAddress: user.Email,
			Template:  "email_verification",
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("enqueue verification email: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.log.Info("email verification email queued", slog.String("user_id", user.ID.String()))
	return nil
}

// ConfirmEmailVerification consumes a verification token and marks the address
// verified. The consume query is the gate: two concurrent submissions of the
// same link cannot both succeed.
func (s *Service) ConfirmEmailVerification(ctx context.Context, rawToken string) error {
	invalid := httpx.NewError(http.StatusBadRequest, httpx.CodeTokenExpired,
		"That verification link is no longer valid. Request a new one.")

	stored, err := s.store.GetEmailVerificationTokenByHash(ctx, hashResetToken(rawToken))
	if err != nil {
		if db.IsNoRows(err) {
			return invalid
		}
		return fmt.Errorf("get verification token: %w", err)
	}
	if stored.UsedAt != nil {
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"That verification link has already been used.")
	}

	return s.store.InTx(ctx, func(q *db.Queries) error {
		consumed, err := q.ConsumeEmailVerificationToken(ctx, stored.ID)
		if err != nil {
			return fmt.Errorf("consume verification token: %w", err)
		}
		if consumed == 0 {
			return invalid
		}
		if err := q.MarkEmailVerified(ctx, stored.UserID); err != nil {
			return fmt.Errorf("mark email verified: %w", err)
		}
		return nil
	})
}
