package cbeadapter

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Zahir-Seid/cbe-verifier/v2/pkg/cbeverifier"

	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

const merchant = "1000123456789"

func newTestVerifier() *Verifier { return New(Options{}) }

func TestValidReceiptToOurAccountVerifies(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{
		Status: cbeverifier.StatusValid,
		Valid:  true,
		Details: &cbeverifier.TransactionDetails{
			ReceiverAccount: "1000****6789",
			Amount:          150.00,
		},
	}, merchant)

	if got.Outcome != payment.OutcomeVerified {
		t.Fatalf("outcome = %q, want verified (reasons: %v)", got.Outcome, got.Reasons)
	}
	if got.PayeeCheck != payment.PayeeMatch {
		t.Errorf("payee = %q, want match", got.PayeeCheck)
	}
	if !got.AmountKnown || got.Amount != 15000 {
		t.Errorf("amount = %d (known %v), want 15000 santim", got.Amount, got.AmountKnown)
	}
	if len(got.Reasons) != 0 {
		t.Errorf("a verified payment should carry no reasons, got %v", got.Reasons)
	}
}

// The whole point of the adapter: the library says valid, but the money went
// somewhere else. This must never confirm a booking.
func TestValidReceiptToAnotherAccountIsMismatch(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{
		Status: cbeverifier.StatusValid,
		Valid:  true,
		Details: &cbeverifier.TransactionDetails{
			ReceiverAccount: "1000****1111",
			Amount:          150.00,
		},
	}, merchant)

	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.PayeeCheck != payment.PayeeMismatch {
		t.Errorf("payee = %q, want mismatch", got.PayeeCheck)
	}
	if len(got.Reasons) == 0 {
		t.Error("a payee mismatch must explain itself to the admin queue")
	}
}

func TestValidReceiptWithUnreadablePayeeIsNotVerified(t *testing.T) {
	// "1000****" is the branch prefix every CBE account shares; it identifies
	// nothing, so it cannot stand in for a payee check.
	got := newTestVerifier().interpret(&cbeverifier.Result{
		Status:  cbeverifier.StatusValid,
		Valid:   true,
		Details: &cbeverifier.TransactionDetails{ReceiverAccount: "1000****", Amount: 150.00},
	}, merchant)

	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.PayeeCheck != payment.PayeeInconclusive {
		t.Errorf("payee = %q, want inconclusive", got.PayeeCheck)
	}
}

func TestMissingDetailsLeavesPayeeUnknown(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{Status: cbeverifier.StatusValid, Valid: true}, merchant)

	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.PayeeCheck != payment.PayeeUnknown {
		t.Errorf("payee = %q, want unknown", got.PayeeCheck)
	}
	if got.AmountKnown {
		t.Error("no details means no reported amount")
	}
}

func TestMismatchCarriesFieldReasons(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{
		Status:  cbeverifier.StatusMismatch,
		Details: &cbeverifier.TransactionDetails{ReceiverAccount: "1000****6789", Amount: 100.00},
		Mismatches: []cbeverifier.Mismatch{
			{Field: "amount", Provided: 150.00, Official: 100.00},
		},
	}, merchant)

	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "Amount does not match") {
		t.Fatalf("reasons = %v, want one amount reason", got.Reasons)
	}
	// The payee matched here, so it must not add noise to the admin's reading.
	if strings.Contains(strings.Join(got.Reasons, " "), "account") {
		t.Errorf("a matching payee should stay silent, got %v", got.Reasons)
	}
}

func TestMismatchWithoutNamedFieldStillExplainsItself(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{Status: cbeverifier.StatusMismatch}, merchant)
	if len(got.Reasons) == 0 {
		t.Fatal("an admin must never see an empty reason list")
	}
}

func TestNotFound(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{Status: cbeverifier.StatusNotFound}, merchant)
	if got.Outcome != payment.OutcomeNotFound {
		t.Fatalf("outcome = %q, want not_found", got.Outcome)
	}
	if len(got.Reasons) == 0 {
		t.Error("not_found should say so in words")
	}
}

// A library that grows a fourth status must not have it confirm bookings.
func TestUnknownStatusIsNotAPass(t *testing.T) {
	got := newTestVerifier().interpret(&cbeverifier.Result{
		Status:  cbeverifier.Status("under_review"),
		Details: &cbeverifier.TransactionDetails{ReceiverAccount: "1000****6789", Amount: 150.00},
	}, merchant)
	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
}

func TestRawIsTheLibraryResult(t *testing.T) {
	res := &cbeverifier.Result{Status: cbeverifier.StatusNotFound, Backend: cbeverifier.BackendJSONAPI}
	if got := newTestVerifier().interpret(res, merchant); got.Raw != res {
		t.Error("Raw must hold the library's own result verbatim (PRD J7)")
	}
}

func TestClassifySplitsPermanentFromRetryable(t *testing.T) {
	permanent := []error{
		cbeverifier.ErrEmptyReference,
		cbeverifier.ErrUnsupportedReference,
		cbeverifier.ErrMissingSuffix,
		cbeverifier.ErrInvalidAmount,
	}
	for _, sentinel := range permanent {
		err := classify(fmt.Errorf("verify: %w", sentinel))
		if !errors.Is(err, payment.ErrPermanent) {
			t.Errorf("%v should classify as permanent", sentinel)
		}
		msg, ok := payment.PermanentMessage(err)
		if !ok || msg == "" {
			t.Errorf("%v must carry a payer-facing message", sentinel)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("%v must stay unwrappable for logs", sentinel)
		}
	}

	retryable := []error{
		cbeverifier.ErrNetwork,
		cbeverifier.ErrServiceUnavailable,
		cbeverifier.ErrUnexpectedResponse,
		cbeverifier.ErrReceiptParse,
		errors.New("something nobody foresaw"),
	}
	for _, sentinel := range retryable {
		if errors.Is(classify(fmt.Errorf("verify: %w", sentinel)), payment.ErrPermanent) {
			t.Errorf("%v must stay retryable: CBE being down is not the payer's fault", sentinel)
		}
	}
}

// A blank merchant account cannot confirm anything. It is a misconfiguration,
// and the failure mode has to be "park for review", not "accept".
func TestUnconfiguredMerchantAccountNeverVerifies(t *testing.T) {
	got := New(Options{}).interpret(&cbeverifier.Result{
		Status:  cbeverifier.StatusValid,
		Valid:   true,
		Details: &cbeverifier.TransactionDetails{ReceiverAccount: "1000****6789", Amount: 150.00},
	}, "")
	if got.Outcome == payment.OutcomeVerified {
		t.Fatal("no merchant account configured must not verify")
	}
	if got.PayeeCheck != payment.PayeeInconclusive {
		t.Errorf("payee = %q, want inconclusive", got.PayeeCheck)
	}
}
