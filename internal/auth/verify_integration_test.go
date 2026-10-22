//go:build integration

// Email-verification tests.
//
// The invariant under test spans a transaction: a queued link and the token row
// it carries are written together, so a mail can never point at a token that
// does not exist, and a token never sits unreachable. Only a real Postgres can
// show that, so these run under the integration tag against the test store.
package auth

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func newVerifyService(store *db.Store) *Service {
	return NewService(Options{
		Store:          store,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmailVerifyTTL: time.Hour,
	})
}

// verificationToken reads the token out of the most recent verification mail,
// which is how the app gets it too -- the service never hands the plaintext
// token back to its caller.
func verificationToken(t *testing.T, store *db.Store, to string) (string, uuid.UUID) {
	t.Helper()
	var token string
	var id uuid.UUID
	err := store.Pool().QueryRow(context.Background(), `
		SELECT payload->>'token', id
		FROM email_outbox
		WHERE template = 'email_verification' AND to_address = $1
		ORDER BY created_at DESC LIMIT 1`, to).Scan(&token, &id)
	if err != nil {
		t.Fatalf("read verification mail: %v", err)
	}
	return token, id
}

// countVerificationMails counts the mails an address was sent, to prove a
// second request after verifying queued nothing.
func countVerificationMails(t *testing.T, store *db.Store, to string) int64 {
	t.Helper()
	var n int64
	err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM email_outbox WHERE template = 'email_verification' AND to_address = $1`, to).Scan(&n)
	if err != nil {
		t.Fatalf("count verification mails: %v", err)
	}
	return n
}

// TestRequestThenConfirmVerifies drives the happy path end to end: a request
// queues exactly one mail and leaves the account unverified, and redeeming the
// link it carries marks the address verified and spends the token.
func TestRequestThenConfirmVerifies(t *testing.T) {
	store := testsupport.Store(t)
	svc := newVerifyService(store)
	ctx := context.Background()

	user := testsupport.User(t, store)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("request: %v", err)
	}

	token, mailID := verificationToken(t, store, user.Email)

	after, err := store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user after request: %v", err)
	}
	if after.EmailVerifiedAt != nil {
		t.Error("request alone marked the address verified")
	}

	if err := svc.ConfirmEmailVerification(ctx, token); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	after, err = store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user after confirm: %v", err)
	}
	if after.EmailVerifiedAt == nil {
		t.Error("confirm did not mark the address verified")
	}

	stored, err := store.GetEmailVerificationTokenByHash(ctx, hashResetToken(token))
	if err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if stored.UsedAt == nil {
		t.Error("confirm did not spend the token")
	}

	var status string
	if err := store.Pool().QueryRow(ctx,
		`SELECT status FROM email_outbox WHERE id = $1`, mailID).Scan(&status); err != nil {
		t.Fatalf("read mail status: %v", err)
	}
	if status != "pending" {
		t.Errorf("queued mail status = %q, want pending", status)
	}
}

// TestConfirmIsSingleUse submits one link twice and requires the second attempt
// to be refused, so a leaked or replayed link cannot pass for a fresh one.
func TestConfirmIsSingleUse(t *testing.T) {
	store := testsupport.Store(t)
	svc := newVerifyService(store)
	ctx := context.Background()

	user := testsupport.User(t, store)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("request: %v", err)
	}
	token, _ := verificationToken(t, store, user.Email)

	if err := svc.ConfirmEmailVerification(ctx, token); err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	err := svc.ConfirmEmailVerification(ctx, token)
	if err == nil {
		t.Fatal("second confirm of the same link succeeded")
	}
	if code := httpx.AsError(err).Code; code != httpx.CodeConflict {
		t.Errorf("second confirm code = %s, want %s", code, httpx.CodeConflict)
	}
}

// TestRequestAfterVerifiedIsNoOp requires an already-verified account to be
// sent nothing: re-mailing a link that does nothing is pure noise.
func TestRequestAfterVerifiedIsNoOp(t *testing.T) {
	store := testsupport.Store(t)
	svc := newVerifyService(store)
	ctx := context.Background()

	user := testsupport.User(t, store)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("request: %v", err)
	}
	token, _ := verificationToken(t, store, user.Email)
	if err := svc.ConfirmEmailVerification(ctx, token); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	before := countVerificationMails(t, store, user.Email)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if after := countVerificationMails(t, store, user.Email); after != before {
		t.Errorf("verified account was sent %d more mail(s)", after-before)
	}
}

// TestUnknownAndExpiredTokensAreIndistinguishable requires both a token that
// never existed and one whose time ran out to fail the same way, so the
// response cannot be used to probe which tokens are real.
func TestUnknownAndExpiredTokensAreIndistinguishable(t *testing.T) {
	store := testsupport.Store(t)
	svc := newVerifyService(store)
	ctx := context.Background()

	user := testsupport.User(t, store)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("request: %v", err)
	}
	token, _ := verificationToken(t, store, user.Email)

	if _, err := store.Pool().Exec(ctx, `
		UPDATE email_verification_tokens SET expires_at = now() - interval '1 minute'
		WHERE user_id = $1`, user.ID); err != nil {
		t.Fatalf("expire token: %v", err)
	}

	cases := map[string]string{
		"expired": token,
		"unknown": "definitely-not-a-real-token",
	}
	for name, tok := range cases {
		err := svc.ConfirmEmailVerification(ctx, tok)
		if err == nil {
			t.Fatalf("%s token was accepted", name)
		}
		if code := httpx.AsError(err).Code; code != httpx.CodeTokenExpired {
			t.Errorf("%s token code = %s, want %s", name, code, httpx.CodeTokenExpired)
		}
	}

	after, err := store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if after.EmailVerifiedAt != nil {
		t.Error("a rejected token verified the address")
	}
}

// TestSupersedeInvalidatesEarlierLink requires a second request to kill the
// first link: a re-request is the player saying the old mail never arrived, so
// the old link must not still work.
func TestSupersedeInvalidatesEarlierLink(t *testing.T) {
	store := testsupport.Store(t)
	svc := newVerifyService(store)
	ctx := context.Background()

	user := testsupport.User(t, store)
	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("first request: %v", err)
	}
	first, _ := verificationToken(t, store, user.Email)

	if err := svc.RequestEmailVerification(ctx, user.ID); err != nil {
		t.Fatalf("second request: %v", err)
	}

	// The second request's mail is now the newest; read it before asserting on
	// the first, since both come from the same outbox.
	second, _ := verificationToken(t, store, user.Email)
	if second == first {
		t.Fatal("second request reused the first token")
	}

	if err := svc.ConfirmEmailVerification(ctx, first); err == nil {
		t.Error("superseded link was still accepted")
	}
	if err := svc.ConfirmEmailVerification(ctx, second); err != nil {
		t.Errorf("fresh link rejected: %v", err)
	}
}
