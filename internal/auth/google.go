package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/api/idtoken"
)

// GoogleIdentity is the subset of a verified Google ID token this system uses.
type GoogleIdentity struct {
	// Subject is Google's stable, immutable user identifier. It, not the
	// email address, is what an account is linked to: an email can be
	// reassigned by a Workspace admin, a subject cannot.
	Subject string
	Email   string
	// EmailVerified reports whether Google has verified the address. An
	// unverified address must not be trusted to match an existing account,
	// or anyone could claim someone else's account by signing up to Google
	// with their address.
	EmailVerified bool
	Name          string
}

// IDTokenVerifier validates a Google ID token. Defined as an interface so the
// service can be tested without reaching Google.
type IDTokenVerifier interface {
	Verify(ctx context.Context, idToken string) (*GoogleIdentity, error)
}

// ErrInvalidIDToken means the presented Google credential could not be
// validated. The reason is deliberately not distinguished for the client.
var ErrInvalidIDToken = errors.New("auth: google id token invalid")

// GoogleVerifier validates ID tokens against Google's published keys. Signature,
// issuer, audience and expiry are all checked by the library; the audience is
// pinned to this application's OAuth client ID so a token minted for a
// different app cannot be replayed here.
type GoogleVerifier struct {
	clientID string
}

// NewGoogleVerifier builds a verifier bound to one OAuth client ID.
func NewGoogleVerifier(clientID string) *GoogleVerifier {
	return &GoogleVerifier{clientID: clientID}
}

// Verify validates the token and extracts the identity claims.
func (g *GoogleVerifier) Verify(ctx context.Context, rawToken string) (*GoogleIdentity, error) {
	if strings.TrimSpace(rawToken) == "" {
		return nil, ErrInvalidIDToken
	}
	payload, err := idtoken.Validate(ctx, rawToken, g.clientID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIDToken, err)
	}
	// Google issues tokens under both hostnames; anything else is not Google.
	switch payload.Issuer {
	case "https://accounts.google.com", "accounts.google.com":
	default:
		return nil, fmt.Errorf("%w: unexpected issuer %q", ErrInvalidIDToken, payload.Issuer)
	}
	if payload.Subject == "" {
		return nil, fmt.Errorf("%w: no subject claim", ErrInvalidIDToken)
	}

	identity := &GoogleIdentity{Subject: payload.Subject}
	if email, ok := payload.Claims["email"].(string); ok {
		identity.Email = strings.ToLower(strings.TrimSpace(email))
	}
	if verified, ok := payload.Claims["email_verified"].(bool); ok {
		identity.EmailVerified = verified
	}
	if name, ok := payload.Claims["name"].(string); ok {
		identity.Name = strings.TrimSpace(name)
	}
	return identity, nil
}
