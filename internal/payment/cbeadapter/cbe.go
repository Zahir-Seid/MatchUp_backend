// Package cbeadapter verifies Commercial Bank of Ethiopia transfers through the
// cbe-verifier library and translates its answer into the payment package's
// verdict.
//
// Two things happen here that the library does not do. It compares the amount
// (and, for legacy references, the reference) and stops there, so this adapter
// runs the payee check itself: a real transfer to somebody else's account would
// otherwise come back valid. And it reports infrastructure trouble and caller
// mistakes through the same error return, so the sentinels are split into
// "retry this later" and "this will never work", which is what decides whether
// the job spends its retry budget.
package cbeadapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zahir-Seid/cbe-verifier/v2/pkg/cbeverifier"

	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/payment"
)

// Verifier checks CBE transfers. Construct it with New.
type Verifier struct {
	// merchantAccount is the platform's own CBE account, compared against the
	// receiver on every receipt. Without it no payee check is possible and the
	// verdict can only ever be inconclusive.
	merchantAccount string
	opts            []cbeverifier.Option
}

// Options configures the adapter.
type Options struct {
	// MerchantAccount is the business's receiving CBE account number. The
	// receipt reports it masked ("1000****6789"), so payee.ComparePayee
	// compares the digits both sides actually show.
	MerchantAccount string
	// Timeout bounds a single HTTP round trip to CBE. Zero uses the library
	// default (30s).
	Timeout time.Duration
	// RetryAttempts and RetryDelay control the library's own retrying of
	// transient failures, which sits inside one job attempt. Zero uses the
	// library defaults (4 attempts, 1.8s apart). Keep the product of the two
	// well under the worker's stale-claim window, or a job will be reclaimed
	// while it is still in flight.
	RetryAttempts int
	RetryDelay    time.Duration
	// AppID and AppVersion override the mobile-app credentials the modern JSON
	// backend expects. Empty uses the library's built-in pair.
	AppID      string
	AppVersion string
	// InsecureSkipTLS disables certificate verification against CBE's
	// endpoints. It exists because CBE's legacy host has served an invalid
	// chain in the past. It makes verification forgeable by anyone on the
	// network path, so config.Load refuses to set it outside APP_ENV=local.
	InsecureSkipTLS bool
}

// New builds a CBE verifier. A blank MerchantAccount is accepted rather than
// rejected — the platform can run without one — but every payment then parks for
// review with an inconclusive payee, so it is a misconfiguration in practice.
func New(o Options) *Verifier {
	var opts []cbeverifier.Option
	if o.Timeout > 0 {
		opts = append(opts, cbeverifier.WithTimeout(o.Timeout))
	}
	if o.RetryAttempts > 0 {
		delay := o.RetryDelay
		if delay <= 0 {
			delay = time.Second
		}
		opts = append(opts, cbeverifier.WithRetry(o.RetryAttempts, delay))
	}
	if o.AppID != "" && o.AppVersion != "" {
		opts = append(opts, cbeverifier.WithAppCredentials(o.AppID, o.AppVersion))
	}
	if o.InsecureSkipTLS {
		opts = append(opts, cbeverifier.WithInsecureSkipVerify())
	}
	return &Verifier{merchantAccount: o.MerchantAccount, opts: opts}
}

// Method reports the payments.method value this verifier handles.
func (v *Verifier) Method() string { return payment.MethodCBE }

// Verify asks CBE about one transfer.
func (v *Verifier) Verify(ctx context.Context, req payment.VerifyRequest) (payment.VerifyResult, error) {
	res, err := cbeverifier.Verify(ctx, cbeverifier.Transaction{
		Reference: req.Reference,
		Suffix:    req.AccountSuffix,
		Amount:    req.ExpectedAmount.ETB(),
	}, v.opts...)
	if err != nil {
		return payment.VerifyResult{}, classify(err)
	}
	if res == nil {
		// The library never does this; treating it as retryable rather than
		// panicking keeps a library bug from losing a real payment.
		return payment.VerifyResult{}, errors.New("cbe: verifier returned no result")
	}
	return v.interpret(res), nil
}

// interpret turns a library Result into a verdict. The library's own Status is
// necessary but not sufficient for OutcomeVerified: the payee check runs on top
// of it.
func (v *Verifier) interpret(res *cbeverifier.Result) payment.VerifyResult {
	out := payment.VerifyResult{PayeeCheck: payment.PayeeUnknown, Raw: res}

	if res.Details != nil {
		if amount, err := money.FromETB(res.Details.Amount); err == nil {
			out.Amount = amount
			out.AmountKnown = true
		}
		out.PayeeCheck = payment.ComparePayee(res.Details.ReceiverAccount, v.merchantAccount)
	}

	switch res.Status {
	case cbeverifier.StatusNotFound:
		out.Outcome = payment.OutcomeNotFound
		out.Reasons = []string{"CBE has no record of this transaction reference."}
		return out

	case cbeverifier.StatusMismatch:
		out.Outcome = payment.OutcomeMismatch
		out.Reasons = mismatchReasons(res.Mismatches)
		out.Reasons = append(out.Reasons, payeeReasons(out.PayeeCheck)...)
		return out

	case cbeverifier.StatusValid:
		// The amount and reference agree. Whether the money reached us is a
		// separate question, and only a positive answer confirms a booking.
		if out.PayeeCheck == payment.PayeeMatch {
			out.Outcome = payment.OutcomeVerified
			return out
		}
		out.Outcome = payment.OutcomeMismatch
		out.Reasons = payeeReasons(out.PayeeCheck)
		return out

	default:
		// An unrecognized status is not a pass. A library that grows a fourth
		// outcome must not have it silently confirm bookings.
		out.Outcome = payment.OutcomeMismatch
		out.Reasons = []string{fmt.Sprintf("CBE returned an unrecognized verification status (%q).", res.Status)}
		return out
	}
}

// mismatchReasons renders the library's field comparison for the admin queue.
func mismatchReasons(list []cbeverifier.Mismatch) []string {
	reasons := make([]string, 0, len(list))
	for _, m := range list {
		switch m.Field {
		case "amount":
			reasons = append(reasons, fmt.Sprintf(
				"Amount does not match: the booking expects %v ETB, CBE recorded %v ETB.", m.Provided, m.Official))
		case "reference":
			reasons = append(reasons, fmt.Sprintf(
				"Reference does not match: submitted %v, CBE recorded %v.", m.Provided, m.Official))
		default:
			reasons = append(reasons, fmt.Sprintf(
				"%s does not match: submitted %v, CBE recorded %v.", m.Field, m.Provided, m.Official))
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "CBE reported the receipt as mismatched without naming a field.")
	}
	return reasons
}

// payeeReasons explains a payee verdict. A match adds nothing to say.
func payeeReasons(verdict payment.PayeeVerdict) []string {
	switch verdict {
	case payment.PayeeMismatch:
		return []string{"The transfer was credited to a different account than the platform's."}
	case payment.PayeeInconclusive:
		return []string{"CBE masks the receiving account too heavily to confirm it is the platform's."}
	case payment.PayeeUnknown:
		return []string{"CBE did not report a receiving account, so the payee could not be checked."}
	default:
		return nil
	}
}

// classify splits the library's sentinels into permanent and retryable.
//
// The permanent ones describe the request: no amount of retrying makes a
// malformed reference verifiable, and burning the retry budget on it only
// delays the admin review. The retryable ones describe the trip to CBE, whose
// legacy receipt host is genuinely unreliable — a payment must not be rejected
// because the bank was down for a minute.
func classify(err error) error {
	switch {
	case errors.Is(err, cbeverifier.ErrEmptyReference):
		return payment.Permanent("No transaction reference was submitted.", err)
	case errors.Is(err, cbeverifier.ErrUnsupportedReference):
		return payment.Permanent("That does not look like a CBE transaction reference. Check the SMS or receipt and submit it again.", err)
	case errors.Is(err, cbeverifier.ErrMissingSuffix):
		return payment.Permanent("This reference needs the last digits of the account you paid from, or the full receipt link.", err)
	case errors.Is(err, cbeverifier.ErrInvalidAmount):
		// The amount comes from the booking, so this is our bug, not the
		// payer's. Permanent because retrying re-sends the same amount.
		return payment.Permanent("This booking's amount could not be checked against CBE. Support will look into it.", err)
	default:
		// ErrNetwork, ErrServiceUnavailable, ErrUnexpectedResponse,
		// ErrReceiptParse and anything unforeseen: retry on the backoff curve.
		return err
	}
}
