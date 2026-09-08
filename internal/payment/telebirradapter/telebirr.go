// Package telebirradapter verifies Telebirr payments through the
// telebirr-verifier library and translates the receipt into the payment
// package's verdict.
//
// The library scrapes the receipt and returns its fields as printed; it compares
// nothing. Every check that decides whether a booking is paid for therefore
// happens here: the amount, the transaction status, and who was credited.
package telebirradapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Zahir-Seid/telebirr-verifier/pkg/telebirr"

	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// Verifier checks Telebirr payments. Construct it with New.
type Verifier struct {
	client          *telebirr.Client
	merchantAccount string
}

// New builds a Telebirr verifier around an already-configured client. The client
// carries the relay pool, timeouts and circuit breaker, all of which come from
// the environment the library itself reads, so nothing about them is restated
// here.
//
// merchantAccount is the business's own Telebirr number, compared against the
// credited party on every receipt. A blank value makes every payee check
// inconclusive, which parks every payment for review rather than confirming it.
func New(client *telebirr.Client, merchantAccount string) *Verifier {
	return &Verifier{client: client, merchantAccount: merchantAccount}
}

// Method reports the payments.method value this verifier handles.
func (v *Verifier) Method() string { return payment.MethodTelebirr }

// Verify looks up one Telebirr receipt and decides whether it pays for the
// booking.
func (v *Verifier) Verify(ctx context.Context, req payment.VerifyRequest) (payment.VerifyResult, error) {
	if v.client == nil {
		return payment.VerifyResult{}, errors.New("telebirr: verifier has no client configured")
	}

	receipt, err := v.client.Verify(ctx, strings.TrimSpace(req.Reference))
	if err != nil {
		// A reference nobody knows is an answer, not a failure: record it as
		// not_found so the payer is told to check the number instead of waiting
		// out a retry curve that cannot change the outcome.
		if errors.Is(err, telebirr.ErrNotFound) {
			return payment.VerifyResult{
				Outcome:    payment.OutcomeNotFound,
				PayeeCheck: payment.PayeeUnknown,
				Reasons:    []string{"Telebirr has no receipt for this transaction number."},
			}, nil
		}
		return payment.VerifyResult{}, classify(err)
	}
	if receipt == nil {
		// Contract says a nil error carries a receipt. Retryable rather than
		// fatal, so a library bug cannot lose a real payment.
		return payment.VerifyResult{}, errors.New("telebirr: verifier returned no receipt")
	}
	return v.interpret(*receipt, req.ExpectedAmount), nil
}

// interpret applies the three checks the library leaves to the caller. Any one
// of them failing parks the payment for review; only all three passing confirms
// the booking.
func (v *Verifier) interpret(receipt telebirr.Receipt, expected money.Santim) payment.VerifyResult {
	out := payment.VerifyResult{
		Raw:        receipt,
		PayeeCheck: payment.ComparePayee(receipt.CreditedPartyAccountNo, v.merchantAccount),
	}

	var reasons []string

	// A receipt missing its number, payer or status was not parsed properly.
	// Nothing further about it is worth trusting.
	if !receipt.IsValid() {
		out.Outcome = payment.OutcomeMismatch
		out.Reasons = []string{"The Telebirr receipt could not be read completely, so the payment could not be confirmed automatically."}
		return out
	}

	// SettledAmount is the sum that reaches the merchant. TotalPaidAmount is
	// larger: it adds the payer's service fee and its VAT, which never arrive
	// here. Comparing against the total would accept a transfer short of the
	// booking price by exactly the fee.
	amount, amountErr := money.ParseETBText(receipt.SettledAmount)
	switch {
	case amountErr != nil:
		reasons = append(reasons, fmt.Sprintf("The settled amount on the receipt (%q) could not be read.", receipt.SettledAmount))
	default:
		out.Amount = amount
		out.AmountKnown = true
		if amount != expected {
			reasons = append(reasons, fmt.Sprintf(
				"Amount does not match: the booking expects %s, the receipt settled %s.", expected.Format(), amount.Format()))
		}
	}

	if !isSuccessful(receipt.TransactionStatus) {
		reasons = append(reasons, fmt.Sprintf("Telebirr reports this transaction as %q, not completed.", receipt.TransactionStatus))
	}

	reasons = append(reasons, payeeReasons(out.PayeeCheck)...)

	if len(reasons) > 0 {
		out.Outcome = payment.OutcomeMismatch
		out.Reasons = reasons
		return out
	}
	out.Outcome = payment.OutcomeVerified
	return out
}

// isSuccessful reads Telebirr's status wording. The receipt prints
// "Successful"; the wording has varied and the page is bilingual, so a small
// set of positive forms is matched and everything else -- pending, failed,
// reversed, blank, or a phrasing not seen before -- is treated as not
// completed. Erring that way parks the payment for a human instead of
// confirming a booking on an unrecognized word.
func isSuccessful(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return false
	}
	for _, ok := range []string{"success", "complete", "paid", "ተሳክቷል"} {
		if strings.Contains(s, ok) {
			return true
		}
	}
	return false
}

// payeeReasons explains a payee verdict. A match adds nothing to say.
func payeeReasons(verdict payment.PayeeVerdict) []string {
	switch verdict {
	case payment.PayeeMismatch:
		return []string{"The payment was credited to a different Telebirr account than the platform's."}
	case payment.PayeeInconclusive:
		return []string{"The credited Telebirr account on the receipt is not complete enough to confirm it is the platform's."}
	case payment.PayeeUnknown:
		return []string{"The receipt does not show which Telebirr account was credited, so the payee could not be checked."}
	default:
		return nil
	}
}

// classify splits the library's failures into permanent and retryable.
//
// Only a malformed reference is permanent. A relay's domain rejection is not
// treated as proof the receipt does not exist: the library documents it as
// "the reference may be wrong or the relay may simply not know it", and a
// relay that does not know a receipt must not reject a real payment. Those
// retry, and an exhausted retry budget parks the payment for review, which is
// where an unresolvable reference belongs anyway.
func classify(err error) error {
	if errors.Is(err, telebirr.ErrInvalidReference) {
		return payment.Permanent("That does not look like a Telebirr transaction number. Check your SMS or receipt and submit it again.", err)
	}
	// ErrNoRoutes, transport failures, cancellation and relay domain
	// rejections: retry on the backoff curve.
	return err
}
