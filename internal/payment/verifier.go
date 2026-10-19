// Package payment owns payment submission and the verification pipeline that
// decides whether a booking is paid for.
//
// Verification is asynchronous by design: the submission endpoint records the
// reference and queues a job, and the worker calls the bank. A slow or dead
// verifier service therefore delays a confirmation but never fails a request or
// loses a payment (SDD 9.2).
package payment

import (
	"context"
	"errors"

	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// Payment methods, matching the CHECK constraint on payments.method.
const (
	MethodCBE      = "cbe"
	MethodTelebirr = "telebirr"
)

// Payment statuses, matching the CHECK constraint on payments.status.
const (
	StatusPending  = "pending"
	StatusVerified = "verified"
	// StatusMismatch covers every "the bank answered, and the answer does not
	// support confirming this booking" case: wrong amount, wrong payee, or a
	// payee we could not read well enough to be sure.
	StatusMismatch = "mismatch"
	StatusNotFound = "not_found"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// VerifyRequest is what the worker asks a verifier to check.
//
// This diverges from the positional signature sketched in SDD 11.1
// (Verify(ctx, reference, suffix string, expectedAmount float64)): the amount
// travels as money.Santim rather than a float64, so the only float in the
// system stays inside the adapter that talks to the bank, and adding a field
// later does not change every call site.
type VerifyRequest struct {
	// Reference is exactly what the payer submitted: a bare CBE FT reference, a
	// full receipt URL, a modern CBE token, or a Telebirr reference.
	Reference string
	// AccountSuffix is the trailing digits of the PAYER's own CBE account,
	// which the legacy receipt service needs to address a bare FT reference.
	// Empty for receipt URLs (which embed it), modern tokens, and Telebirr.
	AccountSuffix string
	// ExpectedAmount is read from the booking, never from the client (FR16).
	ExpectedAmount money.Santim
	// MerchantAccount is the platform's receiving account for this method,
	// resolved when the job runs: the dashboard settings value once an admin
	// has saved one, the environment bootstrap otherwise. The adapter compares
	// the receipt's payee against it; empty means no payee check is possible
	// and the verdict can only be inconclusive.
	MerchantAccount string
}

// VerifyOutcome is the verifier's verdict.
type VerifyOutcome string

const (
	// OutcomeVerified means the bank confirms a payment of the expected amount
	// to our account. This is the only outcome that confirms a booking.
	OutcomeVerified VerifyOutcome = "verified"
	// OutcomeMismatch means the transaction exists but does not match: wrong
	// amount, wrong or unreadable payee, or a non-completed transaction.
	OutcomeMismatch VerifyOutcome = "mismatch"
	// OutcomeNotFound means the bank has no such transaction.
	OutcomeNotFound VerifyOutcome = "not_found"
)

// VerifyResult is a completed verification.
type VerifyResult struct {
	Outcome VerifyOutcome
	// Amount is what the bank says was actually paid, when it says so.
	Amount money.Santim
	// AmountKnown distinguishes "the bank reported zero" from "the bank did not
	// report an amount", which a bare money.Santim cannot.
	AmountKnown bool
	// PayeeCheck records whether the receiving account matched ours. The banks'
	// own libraries do not check this, so the platform does (see payee.go).
	PayeeCheck PayeeVerdict
	// Reasons are the human-readable mismatch reasons, shown to the admin
	// reviewing the payment and summarized for the payer.
	Reasons []string
	// Raw is the verifier library's own result, stored verbatim so an admin sees
	// what the bank said rather than our summary of it (PRD J7).
	Raw any
}

// Verifier is one payment channel. The adapters in cbeadapter and telebirradapter
// implement it; nothing in this package imports them, so wiring happens in the
// worker binary.
type Verifier interface {
	// Method returns the payments.method value this verifier handles.
	Method() string
	// Verify asks the bank about one transaction. A returned error means the
	// question could not be answered, not that the answer was no; that
	// distinction is what decides between retrying and recording an outcome.
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// ErrPermanent marks a verifier failure that retrying cannot fix: a malformed
// reference, an unsupported reference format, a missing account suffix. The
// worker records it against the payment instead of spending the retry budget.
var ErrPermanent = errors.New("permanent verification failure")

// Permanent wraps err as a permanent failure with a message safe to show the
// payer.
func Permanent(userMessage string, err error) error {
	return &permanentError{message: userMessage, cause: err}
}

type permanentError struct {
	message string
	cause   error
}

func (e *permanentError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}

// Is reports true for ErrPermanent so callers can classify with errors.Is.
func (e *permanentError) Is(target error) bool { return target == ErrPermanent }

func (e *permanentError) Unwrap() error { return e.cause }

// PermanentMessage returns the payer-facing message from a permanent failure.
func PermanentMessage(err error) (string, bool) {
	var perm *permanentError
	if errors.As(err, &perm) {
		return perm.message, true
	}
	return "", false
}
