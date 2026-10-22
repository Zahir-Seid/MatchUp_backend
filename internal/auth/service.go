// Package auth owns identity: account creation, sign-in with a password or a
// Google credential, access/refresh token lifecycle, and password reset.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Roles recognized by the system.
const (
	RolePlayer = "player"
	RoleAdmin  = "admin"
)

// Account statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Service implements the identity operations. It is safe for concurrent use.
type Service struct {
	store  *db.Store
	tokens *TokenIssuer
	google IDTokenVerifier
	log    *slog.Logger

	resetTTL time.Duration
	// verifyTTL bounds how long a signup email-verification link works.
	verifyTTL time.Duration
	// onLogin is an optional hook fired after a successful password sign-in.
	onLogin func(ctx context.Context, user db.User)

	// requireAdminRole restricts sign-in to admins. The admin API sets it so
	// a player account cannot obtain an admin-audience token even by hitting
	// the admin login endpoint directly.
	requireAdminRole bool

	now func() time.Time
}

// Options configures a Service.
type Options struct {
	Store            *db.Store
	Tokens           *TokenIssuer
	Google           IDTokenVerifier
	Logger           *slog.Logger
	ResetTTL         time.Duration
	EmailVerifyTTL   time.Duration
	RequireAdminRole bool
	// OnLogin, when set, is called after a successful password sign-in. The
	// admin binary uses it to record dashboard access in the audit log; the
	// public API leaves it nil, because "a player signed in" is not an event
	// anybody reviews, and recording every one of them would bury the entries
	// that matter.
	OnLogin func(ctx context.Context, user db.User)
}

// NewService builds the identity service.
func NewService(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	verifyTTL := opts.EmailVerifyTTL
	if verifyTTL <= 0 {
		verifyTTL = 24 * time.Hour
	}
	return &Service{
		store:            opts.Store,
		tokens:           opts.Tokens,
		google:           opts.Google,
		log:              log,
		resetTTL:         opts.ResetTTL,
		verifyTTL:        verifyTTL,
		onLogin:          opts.OnLogin,
		requireAdminRole: opts.RequireAdminRole,
		now:              time.Now,
	}
}

// TokenPair is what a successful authentication returns.
type TokenPair struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	TokenType        string    `json:"token_type"`
} //	@name	TokenPair

// SignupInput carries a new account's details. Phone is contact information
// only and is never used to authenticate.
type SignupInput struct {
	FullName  string
	Email     string
	Password  string
	Age       *int32
	Phone     string
	UserAgent string
}

// Signup creates a password account and returns a session for it.
func (s *Service) Signup(ctx context.Context, in SignupInput) (*db.User, *TokenPair, error) {
	hash, err := hashPasswordForRole(in.Password, RolePlayer)
	if err != nil {
		if weak, ok := AsWeakPassword(err); ok {
			return nil, nil, httpx.ErrValidation.WithField("password", weak.Reason).WithCause(err)
		}
		return nil, nil, err
	}

	user, err := s.store.CreateUser(ctx, db.CreateUserParams{
		FullName:     in.FullName,
		Email:        in.Email,
		PasswordHash: &hash,
		Age:          in.Age,
		Phone:        optionalString(in.Phone),
		Role:         nil, // defaults to player
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			// Sign-up is the one place where revealing that an address is
			// taken is unavoidable: the account cannot be created and the
			// person needs to know to sign in instead.
			return nil, nil, httpx.NewError(http.StatusConflict, httpx.CodeEmailInUse,
				"An account with that email already exists.").WithCause(err)
		}
		return nil, nil, fmt.Errorf("create user: %w", err)
	}

	pair, err := s.issueSession(ctx, user, in.UserAgent)
	if err != nil {
		return nil, nil, err
	}
	return &user, pair, nil
}

// Login authenticates with email and password.
func (s *Service) Login(ctx context.Context, email, password, userAgent string) (*db.User, *TokenPair, error) {
	invalid := httpx.NewError(http.StatusUnauthorized, httpx.CodeInvalidCredential,
		"Email or password is incorrect.")

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if db.IsNoRows(err) {
			// Run a comparison against a dummy hash anyway so a missing
			// account and a wrong password take indistinguishable time.
			_ = verifyPassword(dummyBcryptHash, password)
			return nil, nil, invalid
		}
		return nil, nil, fmt.Errorf("get user by email: %w", err)
	}

	// Google-only accounts have no password hash; they must not be sign-in-able
	// with an empty or guessed password.
	if user.PasswordHash == nil || !verifyPassword(*user.PasswordHash, password) {
		return nil, nil, invalid
	}
	if err := s.assertLoginAllowed(user); err != nil {
		return nil, nil, err
	}

	pair, err := s.issueSession(ctx, user, userAgent)
	if err != nil {
		return nil, nil, err
	}
	// After the session exists, so a hook that writes cannot cost a caller a
	// sign-in it already earned.
	if s.onLogin != nil {
		s.onLogin(ctx, user)
	}
	return &user, pair, nil
}

// dummyBcryptHash is a valid bcrypt hash of a value nobody uses. Comparing
// against it on the account-not-found path keeps response timing flat, so the
// endpoint cannot be used to enumerate registered addresses.
const dummyBcryptHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.rGjxLZLoDh8jRxaBTLBDs5FVKtFtNAG"

// GoogleSignIn exchanges a Google ID token for a session, creating the account
// on first use.
func (s *Service) GoogleSignIn(ctx context.Context, idToken, userAgent string) (*db.User, *TokenPair, error) {
	if s.google == nil {
		return nil, nil, httpx.ErrUnavailable.WithCause(errors.New("google sign-in not configured"))
	}
	identity, err := s.google.Verify(ctx, idToken)
	if err != nil {
		return nil, nil, httpx.NewError(http.StatusUnauthorized, httpx.CodeInvalidCredential,
			"That Google sign-in could not be verified.").WithCause(err)
	}

	user, err := s.resolveGoogleUser(ctx, identity)
	if err != nil {
		return nil, nil, err
	}
	if err := s.assertLoginAllowed(*user); err != nil {
		return nil, nil, err
	}

	pair, err := s.issueSession(ctx, *user, userAgent)
	if err != nil {
		return nil, nil, err
	}
	return user, pair, nil
}

// resolveGoogleUser finds or creates the account behind a verified Google
// identity. Linking runs in one transaction so a concurrent first sign-in
// cannot produce two accounts for the same subject.
func (s *Service) resolveGoogleUser(ctx context.Context, identity *GoogleIdentity) (*db.User, error) {
	// Already linked: the common path.
	user, err := s.store.GetUserByOAuthSubject(ctx, db.GetUserByOAuthSubjectParams{
		Provider:        "google",
		ProviderSubject: identity.Subject,
	})
	if err == nil {
		return &user, nil
	}
	if !db.IsNoRows(err) {
		return nil, fmt.Errorf("get user by oauth subject: %w", err)
	}

	if identity.Email == "" || !identity.EmailVerified {
		// Without a Google-verified address there is no safe way to decide
		// whether this is a new account or an existing one.
		return nil, httpx.NewError(http.StatusUnauthorized, httpx.CodeInvalidCredential,
			"Your Google account needs a verified email address to sign in.")
	}

	var resolved db.User
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		existing, err := q.GetUserByEmail(ctx, identity.Email)
		switch {
		case err == nil:
			// An account already holds this Google-verified address: link the
			// identity to it rather than creating a duplicate.
			resolved = existing
		case db.IsNoRows(err):
			created, err := q.CreateUser(ctx, db.CreateUserParams{
				FullName:     fallbackName(identity),
				Email:        identity.Email,
				PasswordHash: nil, // Google-only account
				Role:         nil,
			})
			if err != nil {
				return fmt.Errorf("create google user: %w", err)
			}
			// Google has already verified the address, so there is nothing
			// left for this system to confirm.
			if err := q.MarkEmailVerified(ctx, created.ID); err != nil {
				return fmt.Errorf("mark email verified: %w", err)
			}
			created.EmailVerifiedAt = ptr(s.now())
			resolved = created
		default:
			return fmt.Errorf("get user by email: %w", err)
		}

		if _, err := q.CreateOAuthIdentity(ctx, db.CreateOAuthIdentityParams{
			UserID:          resolved.ID,
			Provider:        "google",
			ProviderSubject: identity.Subject,
		}); err != nil {
			return fmt.Errorf("link google identity: %w", err)
		}
		return nil
	})
	if err != nil {
		// A concurrent first sign-in won the race and linked the same
		// subject; read its result instead of failing the request.
		if db.IsUniqueViolation(err) {
			user, getErr := s.store.GetUserByOAuthSubject(ctx, db.GetUserByOAuthSubjectParams{
				Provider:        "google",
				ProviderSubject: identity.Subject,
			})
			if getErr == nil {
				return &user, nil
			}
		}
		return nil, err
	}
	return &resolved, nil
}

// Refresh exchanges a refresh token for a new pair, rotating the old token.
//
// Reuse of an already-rotated token is treated as a compromise: every session
// for that user is revoked, because either the token was stolen or a legitimate
// client is replaying, and the safe reading is the former.
func (s *Service) Refresh(ctx context.Context, rawToken, userAgent string) (*TokenPair, error) {
	invalid := httpx.NewError(http.StatusUnauthorized, httpx.CodeTokenExpired,
		"Your session has ended. Please sign in again.")

	stored, err := s.store.GetRefreshTokenByHash(ctx, s.tokens.HashRefreshToken(rawToken))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, invalid
		}
		return nil, fmt.Errorf("get refresh token: %w", err)
	}

	if stored.RevokedAt != nil {
		if stored.RotatedTo != nil {
			s.log.Warn("refresh token reuse detected; revoking all sessions",
				slog.String("user_id", stored.UserID.String()),
				slog.String("token_id", stored.ID.String()))
			if _, err := s.store.RevokeAllUserRefreshTokens(ctx, stored.UserID); err != nil {
				s.log.Error("revoke all refresh tokens failed",
					slog.String("user_id", stored.UserID.String()),
					slog.String("error", err.Error()))
			}
			return nil, httpx.NewError(http.StatusUnauthorized, httpx.CodeTokenReused,
				"Your session was ended for security reasons. Please sign in again.")
		}
		return nil, invalid
	}
	if !stored.ExpiresAt.After(s.now()) {
		return nil, invalid
	}

	user, err := s.store.GetUserByID(ctx, stored.UserID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, invalid
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	if err := s.assertLoginAllowed(user); err != nil {
		return nil, err
	}

	var pair *TokenPair
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		issued, replacement, err := s.mintSession(ctx, q, user, userAgent)
		if err != nil {
			return err
		}
		// Link the outgoing token to its replacement in the same transaction,
		// so a replay is recognizable as reuse rather than merely revoked.
		if err := q.RotateRefreshToken(ctx, db.RotateRefreshTokenParams{
			ID:        stored.ID,
			RotatedTo: &replacement,
		}); err != nil {
			return fmt.Errorf("rotate refresh token: %w", err)
		}
		pair = issued
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// Logout revokes one refresh token. It reports success even when the token is
// unknown: the caller's session is over either way, and distinguishing the
// cases would leak whether a token was ever valid.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	stored, err := s.store.GetRefreshTokenByHash(ctx, s.tokens.HashRefreshToken(rawToken))
	if err != nil {
		if db.IsNoRows(err) {
			return nil
		}
		return fmt.Errorf("get refresh token: %w", err)
	}
	if err := s.store.RevokeRefreshToken(ctx, stored.ID); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// LogoutAll revokes every session for a user.
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	if _, err := s.store.RevokeAllUserRefreshTokens(ctx, userID); err != nil {
		return fmt.Errorf("revoke all refresh tokens: %w", err)
	}
	return nil
}

// VerifyAccessToken implements httpx.TokenVerifier. It validates the token and
// confirms the account is still active, so disabling an account takes effect
// within the access token's lifetime rather than at its expiry.
func (s *Service) VerifyAccessToken(ctx context.Context, raw string) (httpx.Principal, error) {
	userID, role, email, err := s.tokens.ParseAccessToken(raw)
	switch {
	case errors.Is(err, ErrTokenExpired):
		return httpx.Principal{}, httpx.NewError(http.StatusUnauthorized, httpx.CodeTokenExpired,
			"Your session expired. Please sign in again.").WithCause(err)
	case err != nil:
		return httpx.Principal{}, httpx.ErrUnauthorized.WithCause(err)
	}

	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.Principal{}, httpx.ErrUnauthorized.WithCause(err)
		}
		return httpx.Principal{}, fmt.Errorf("get user: %w", err)
	}
	if err := s.assertLoginAllowed(user); err != nil {
		return httpx.Principal{}, err
	}
	// A role change must not wait for the token to expire either.
	if user.Role != role {
		return httpx.Principal{}, httpx.ErrUnauthorized.WithCause(
			fmt.Errorf("token role %q no longer matches account role %q", role, user.Role))
	}
	_ = email

	return httpx.Principal{UserID: user.ID, Role: user.Role, Email: user.Email}, nil
}

// assertLoginAllowed enforces account status and, on the admin API, the admin
// role.
func (s *Service) assertLoginAllowed(user db.User) error {
	if user.Status != StatusActive {
		return httpx.NewError(http.StatusForbidden, httpx.CodeAccountDisabled,
			"This account has been disabled. Contact support if you think this is a mistake.")
	}
	if s.requireAdminRole && user.Role != RoleAdmin {
		return httpx.ErrForbidden
	}
	return nil
}

// issueSession mints a token pair outside any caller-held transaction.
func (s *Service) issueSession(ctx context.Context, user db.User, userAgent string) (*TokenPair, error) {
	var pair *TokenPair
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		issued, _, err := s.mintSession(ctx, q, user, userAgent)
		if err != nil {
			return err
		}
		pair = issued
		return nil
	})
	return pair, err
}

// mintSession signs an access token and persists a new refresh token, returning
// the pair and the new refresh token's row id for rotation linking.
func (s *Service) mintSession(ctx context.Context, q *db.Queries, user db.User, userAgent string) (*TokenPair, uuid.UUID, error) {
	access, accessExpiry, err := s.tokens.IssueAccessToken(user.ID, user.Role, user.Email)
	if err != nil {
		return nil, uuid.Nil, err
	}
	refresh, refreshHash, err := s.tokens.NewRefreshToken()
	if err != nil {
		return nil, uuid.Nil, err
	}
	refreshExpiry := s.now().Add(s.tokens.RefreshTTL())

	row, err := q.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: refreshExpiry,
		UserAgent: optionalString(truncate(userAgent, 255)),
	})
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:      access,
		RefreshToken:     refresh,
		AccessExpiresAt:  accessExpiry,
		RefreshExpiresAt: refreshExpiry,
		TokenType:        "Bearer",
	}, row.ID, nil
}

func optionalString(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func ptr[T any](v T) *T { return &v }

func fallbackName(identity *GoogleIdentity) string {
	if identity.Name != "" {
		return identity.Name
	}
	if local, _, ok := strings.Cut(identity.Email, "@"); ok && local != "" {
		return local
	}
	return "Player"
}

// marshalPayload serializes email template variables.
func marshalPayload(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal email payload: %w", err)
	}
	return raw, nil
}
