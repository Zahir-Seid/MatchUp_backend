package payment

import (
	"net/http"
	"strings"

	"github.com/Zahir-Seid/cbe-verifier/v2/pkg/cbeverifier"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Submission is a payer's reference reduced to the canonical form that gets
// stored and verified.
type Submission struct {
	// Reference is what goes in payments.transaction_reference. For CBE it is
	// the extracted code rather than the pasted URL, so a payer who pastes the
	// receipt link and one who types the bare code cannot both spend the same
	// transaction (FR17).
	Reference string
	// AccountSuffix is the payer's own account suffix, taken from the receipt
	// URL when it was embedded there and from the submitted field otherwise.
	AccountSuffix string
}

// Canonicalize reduces a raw submission to its stored form and rejects input
// the verifier could never act on.
//
// It runs at the edge rather than in the worker because the anti-replay
// constraint is on the stored value: normalizing after the insert would be too
// late to matter.
func Canonicalize(method, rawReference, rawSuffix string) (Submission, error) {
	rawReference = strings.TrimSpace(rawReference)
	rawSuffix = strings.TrimSpace(rawSuffix)

	switch method {
	case MethodCBE:
		return canonicalizeCBE(rawReference, rawSuffix)
	case MethodTelebirr:
		// Telebirr's own client interpolates the reference into a lookup URL
		// exactly as given, so it is stored as printed on the receipt. The
		// case-insensitive unique index is what makes it unrepeatable.
		if rawReference == "" {
			return Submission{}, httpx.ErrValidation.WithField("transaction_reference", "required")
		}
		return Submission{Reference: rawReference}, nil
	default:
		return Submission{}, httpx.ErrValidation.WithField("method", "must be cbe or telebirr")
	}
}

// canonicalizeCBE classifies the reference the way the verifier library will,
// so a submission that cannot be verified is refused while the payer is still
// looking at the screen instead of failing silently minutes later.
func canonicalizeCBE(rawReference, rawSuffix string) (Submission, error) {
	ref := cbeverifier.ClassifyReference(rawReference)

	switch ref.Kind {
	case cbeverifier.RefLegacyURL:
		// The URL carries both halves. Its embedded suffix wins over anything
		// typed into the field: it came from the bank, the field came from a
		// person.
		return Submission{Reference: ref.ID, AccountSuffix: ref.Suffix}, nil

	case cbeverifier.RefNewToken:
		// Modern tokens address the receipt on their own; a suffix is neither
		// needed nor used.
		return Submission{Reference: ref.Token}, nil

	case cbeverifier.RefLegacy:
		if rawSuffix == "" {
			return Submission{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeSuffixRequired,
				"A bare FT reference needs the last 8 digits of the account you paid from, or paste the full receipt link instead.").
				WithField("account_suffix", "required for a bare FT reference")
		}
		return Submission{Reference: ref.ID, AccountSuffix: rawSuffix}, nil

	default:
		return Submission{}, httpx.ErrValidation.WithField("transaction_reference",
			"paste the CBE receipt link, or the FT reference from your SMS")
	}
}
