package booking

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// referenceAlphabet is Crockford base32 without I, L, O and U, so a code read
// aloud or typed from a screenshot cannot be confused between 1/I/L, 0/O, or
// misread as a word. Bookings are quoted by this code in support conversations.
const referenceAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// referenceBodyLength gives 32^8 ≈ 1.1e12 codes. At the volumes in the PRD a
// collision is effectively impossible, and the insert is guarded by a unique
// constraint regardless.
const referenceBodyLength = 8

// referencePrefix marks the code as ours in a payment note or a support thread.
const referencePrefix = "MU-"

// newReferenceCode returns a human-quotable booking reference such as
// "MU-7K3QX9AB".
func newReferenceCode() (string, error) {
	limit := big.NewInt(int64(len(referenceAlphabet)))
	var b strings.Builder
	b.WriteString(referencePrefix)
	for range referenceBodyLength {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate reference code: %w", err)
		}
		b.WriteByte(referenceAlphabet[n.Int64()])
	}
	return b.String(), nil
}
