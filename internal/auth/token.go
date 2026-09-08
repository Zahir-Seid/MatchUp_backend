package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Audience separates the two trust domains. The public app and the admin
// dashboard issue tokens with different audiences and different signing keys;
// a token minted for one is rejected by the other even if the keys were ever
// misconfigured to match.
type Audience string

const (
	AudienceApp   Audience = "matchup-app"
	AudienceAdmin Audience = "matchup-admin"
)

const tokenIssuer = "matchup"

// accessClaims is the access token payload. Only the identifiers and the role
// travel in the token; anything mutable is read from the database per request.
type accessClaims struct {
	jwt.RegisteredClaims
	Role  string `json:"role"`
	Email string `json:"email"`
}

// TokenIssuer mints and validates access tokens and generates opaque refresh
// tokens.
type TokenIssuer struct {
	accessSecret  []byte
	refreshSecret []byte
	audience      Audience
	accessTTL     time.Duration
	refreshTTL    time.Duration
	now           func() time.Time
}

// NewTokenIssuer builds an issuer for one audience.
func NewTokenIssuer(accessSecret, refreshSecret string, audience Audience, accessTTL, refreshTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
		audience:      audience,
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
		now:           time.Now,
	}
}

// Errors surfaced by token validation.
var (
	ErrTokenInvalid = errors.New("auth: token invalid")
	ErrTokenExpired = errors.New("auth: token expired")
)

// IssueAccessToken signs a short-lived access token for the user.
func (t *TokenIssuer) IssueAccessToken(userID uuid.UUID, role, email string) (string, time.Time, error) {
	issuedAt := t.now()
	expiresAt := issuedAt.Add(t.accessTTL)

	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{string(t.audience)},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		Role:  role,
		Email: email,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.accessSecret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken validates the signature, issuer, audience and expiry, and
// returns the claims. The signing method is pinned so a token presented with
// alg=none or an asymmetric algorithm is rejected outright.
func (t *TokenIssuer) ParseAccessToken(raw string) (uuid.UUID, string, string, error) {
	var claims accessClaims
	parsed, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return t.accessSecret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(string(t.audience)),
		jwt.WithExpirationRequired(),
	)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return uuid.Nil, "", "", ErrTokenExpired
	case err != nil:
		return uuid.Nil, "", "", fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	case !parsed.Valid:
		return uuid.Nil, "", "", ErrTokenInvalid
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, "", "", fmt.Errorf("%w: subject is not a user id", ErrTokenInvalid)
	}
	return userID, claims.Role, claims.Email, nil
}

// refreshTokenBytes is the entropy in an opaque refresh token. 256 bits makes
// guessing infeasible without needing the token to carry any structure.
const refreshTokenBytes = 32

// NewRefreshToken returns a fresh opaque token and the hash to store. Only the
// hash is persisted, so a database leak does not hand over usable sessions.
//
// Refresh tokens are opaque random values rather than JWTs on purpose: a
// refresh token must be revocable the instant a session is ended or a reuse is
// detected, and a self-contained signed token cannot be revoked without a
// server-side record anyway.
func (t *TokenIssuer) NewRefreshToken() (plain string, hash []byte, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, t.HashRefreshToken(plain), nil
}

// HashRefreshToken derives the stored form of an opaque token, keyed with the
// refresh secret. A keyed hash rather than a bare digest means the stored rows
// are useless to anyone who obtains the database but not the application
// secret. A slow hash would add nothing: the input is 256 bits of uniform
// randomness, so there is no low-entropy guess space to protect.
func (t *TokenIssuer) HashRefreshToken(plain string) []byte {
	mac := hmac.New(sha256.New, t.refreshSecret)
	mac.Write([]byte(plain))
	return mac.Sum(nil)
}

// RefreshTTL exposes the configured refresh lifetime.
func (t *TokenIssuer) RefreshTTL() time.Duration { return t.refreshTTL }

// AccessTTL exposes the configured access lifetime.
func (t *TokenIssuer) AccessTTL() time.Duration { return t.accessTTL }

// sha256Sum returns the SHA-256 digest of b as a slice.
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
