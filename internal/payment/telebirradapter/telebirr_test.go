package telebirradapter

import (
	"errors"
	"strings"
	"testing"

	"github.com/Zahir-Seid/telebirr-verifier/pkg/telebirr"

	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

const merchantNumber = "251911223344"

func good() telebirr.Receipt {
	return telebirr.Receipt{
		PayerName:              "Abebe Bekele",
		ReceiptNo:              "CH1234567890",
		TransactionStatus:      "Successful",
		CreditedPartyName:      "Addis Football",
		CreditedPartyAccountNo: "251911223344",
		SettledAmount:          "150.00 Birr",
		ServiceFee:             "1.50 Birr",
		ServiceFeeVAT:          "0.23 Birr",
		TotalPaidAmount:        "151.73 Birr",
		PaymentDate:            "12-09-2026 15:30:00",
	}
}

func newTestVerifier() *Verifier { return New(nil, merchantNumber) }

func TestCompleteReceiptVerifies(t *testing.T) {
	got := newTestVerifier().interpret(good(), money.Santim(15000))

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

// The asymmetry that makes this adapter's amount check non-obvious: the payer
// pays the fee on top, so TotalPaidAmount is larger than what reaches us.
// Comparing against the total would accept a transfer short by exactly the fee.
func TestAmountIsReadFromSettledNotTotal(t *testing.T) {
	r := good()
	r.SettledAmount = "148.30 Birr"   // short of the booking price
	r.TotalPaidAmount = "150.00 Birr" // but the payer's total looks right

	got := newTestVerifier().interpret(r, money.Santim(15000))
	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.Amount != 14830 {
		t.Errorf("amount = %d, want the settled 14830 santim", got.Amount)
	}
	if len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "Amount does not match") {
		t.Errorf("reasons = %v, want an amount reason", got.Reasons)
	}
}

func TestAmountAcceptsPrintedFormatting(t *testing.T) {
	for _, printed := range []string{"1,500.00 Birr", "ETB 1,500.00", "1500.00", "1,500.00 ብር"} {
		r := good()
		r.SettledAmount = printed
		got := newTestVerifier().interpret(r, money.Santim(150000))
		if got.Outcome != payment.OutcomeVerified {
			t.Errorf("%q: outcome = %q, want verified (reasons: %v)", printed, got.Outcome, got.Reasons)
		}
	}
}

func TestUnreadableAmountDoesNotVerify(t *testing.T) {
	r := good()
	r.SettledAmount = "" // the scrape missed it
	got := newTestVerifier().interpret(r, money.Santim(15000))
	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.AmountKnown {
		t.Error("an unparseable amount is not a known amount")
	}
}

func TestOnlySuccessfulStatusVerifies(t *testing.T) {
	verified := []string{"Successful", "successful", "COMPLETED", "Completed", "Paid", "ተሳክቷል"}
	for _, status := range verified {
		r := good()
		r.TransactionStatus = status
		if got := newTestVerifier().interpret(r, money.Santim(15000)); got.Outcome != payment.OutcomeVerified {
			t.Errorf("status %q: outcome = %q, want verified (reasons: %v)", status, got.Outcome, got.Reasons)
		}
	}

	rejected := []string{"Pending", "Failed", "Reversed", "Cancelled", "In Progress", "something new"}
	for _, status := range rejected {
		r := good()
		r.TransactionStatus = status
		got := newTestVerifier().interpret(r, money.Santim(15000))
		if got.Outcome != payment.OutcomeMismatch {
			t.Errorf("status %q: outcome = %q, want mismatch", status, got.Outcome)
		}
	}
}

// The check neither library performs.
func TestPaymentToAnotherAccountIsMismatch(t *testing.T) {
	r := good()
	r.CreditedPartyAccountNo = "251911999888"
	got := newTestVerifier().interpret(r, money.Santim(15000))

	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.PayeeCheck != payment.PayeeMismatch {
		t.Errorf("payee = %q, want mismatch", got.PayeeCheck)
	}
	if len(got.Reasons) == 0 {
		t.Error("a payee mismatch must explain itself")
	}
}

// Local and international forms of the same number are the same account.
func TestLocalAndInternationalNumberFormsMatch(t *testing.T) {
	r := good()
	r.CreditedPartyAccountNo = "0911223344"
	if got := newTestVerifier().interpret(r, money.Santim(15000)); got.PayeeCheck != payment.PayeeMatch {
		t.Errorf("payee = %q, want match for the local form of the same number", got.PayeeCheck)
	}
}

func TestMissingCreditedAccountDoesNotVerify(t *testing.T) {
	r := good()
	r.CreditedPartyAccountNo = ""
	got := newTestVerifier().interpret(r, money.Santim(15000))
	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if got.PayeeCheck != payment.PayeeUnknown {
		t.Errorf("payee = %q, want unknown", got.PayeeCheck)
	}
}

// A half-scraped receipt is not evidence of anything, whatever its other fields
// happen to say.
func TestIncompleteReceiptIsRejectedBeforeAnyOtherCheck(t *testing.T) {
	r := good()
	r.ReceiptNo = ""
	got := newTestVerifier().interpret(r, money.Santim(15000))
	if got.Outcome != payment.OutcomeMismatch {
		t.Fatalf("outcome = %q, want mismatch", got.Outcome)
	}
	if len(got.Reasons) != 1 {
		t.Errorf("reasons = %v, want the single 'could not be read' reason", got.Reasons)
	}
}

func TestEveryFailureIsListedTogether(t *testing.T) {
	r := good()
	r.SettledAmount = "10.00 Birr"
	r.TransactionStatus = "Pending"
	r.CreditedPartyAccountNo = "251911999888"

	got := newTestVerifier().interpret(r, money.Santim(15000))
	if len(got.Reasons) != 3 {
		t.Fatalf("reasons = %v, want all three (amount, status, payee)", got.Reasons)
	}
}

func TestUnconfiguredMerchantNumberNeverVerifies(t *testing.T) {
	got := New(nil, "").interpret(good(), money.Santim(15000))
	if got.Outcome == payment.OutcomeVerified {
		t.Fatal("no merchant number configured must not verify")
	}
	if got.PayeeCheck != payment.PayeeInconclusive {
		t.Errorf("payee = %q, want inconclusive", got.PayeeCheck)
	}
}

func TestRawIsTheReceipt(t *testing.T) {
	r := good()
	got := newTestVerifier().interpret(r, money.Santim(15000))
	stored, ok := got.Raw.(telebirr.Receipt)
	if !ok {
		t.Fatalf("Raw = %T, want telebirr.Receipt", got.Raw)
	}
	if stored.ReceiptNo != r.ReceiptNo {
		t.Error("Raw must hold the receipt verbatim (PRD J7)")
	}
}

func TestClassifyOnlyMalformedReferenceIsPermanent(t *testing.T) {
	err := classify(errors.New("telebirr: lookup failed: " + telebirr.ErrInvalidReference.Error()))
	if errors.Is(err, payment.ErrPermanent) {
		t.Fatal("classification must use errors.Is on the sentinel, not string matching")
	}

	perm := classify(&telebirr.VerificationError{Kind: telebirr.ErrorDomain, Message: "bad ref", Err: telebirr.ErrInvalidReference})
	if !errors.Is(perm, payment.ErrPermanent) {
		t.Error("a malformed reference is permanent")
	}
	if msg, ok := payment.PermanentMessage(perm); !ok || msg == "" {
		t.Error("a permanent failure must carry a payer-facing message")
	}

	retryable := []error{
		&telebirr.VerificationError{Kind: telebirr.ErrorTransport, Message: "relay unreachable"},
		&telebirr.VerificationError{Kind: telebirr.ErrorCancelled, Message: "cancelled"},
		// A relay that does not know a receipt is not proof the payment is
		// absent, so it retries rather than rejecting a real transfer.
		&telebirr.VerificationError{Kind: telebirr.ErrorDomain, Message: "receipt not found"},
		telebirr.ErrNoRoutes,
	}
	for _, err := range retryable {
		if errors.Is(classify(err), payment.ErrPermanent) {
			t.Errorf("%v must stay retryable", err)
		}
	}
}

func TestVerifyWithoutClientFails(t *testing.T) {
	if _, err := New(nil, merchantNumber).Verify(t.Context(), payment.VerifyRequest{Reference: "CH1"}); err == nil {
		t.Fatal("a verifier with no client must report an error, not a verdict")
	}
}
