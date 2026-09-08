// Package money represents Ethiopian Birr as an exact integer count of santim
// (1 ETB = 100 santim). Every stored amount, arithmetic operation and
// comparison in the system uses Santim; float64 appears only at the boundary
// with the payment verifier libraries, which speak in decimal ETB.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Santim is an amount in Ethiopian santim. It is the only money type stored in
// the database (bigint) and the only type arithmetic is performed on.
type Santim int64

// SantimPerETB is the number of santim in one Birr.
const SantimPerETB = 100

// Errors returned when text cannot be read as an amount.
var (
	// ErrEmptyAmount means the input contained no digits at all.
	ErrEmptyAmount = errors.New("money: empty amount")
	// ErrUnparseableAmount means digits were present but did not form a
	// number, or the number was not a plain decimal figure.
	ErrUnparseableAmount = errors.New("money: unparseable amount")
	// ErrNegativeAmount means the amount was below zero, which no payment or
	// price may be.
	ErrNegativeAmount = errors.New("money: negative amount")
)

// FromETB converts a decimal ETB value to santim, rounding to the nearest
// santim. Receipts carry two decimal places, so rounding is exact in practice;
// it exists to absorb binary float representation error (14.99 is not exactly
// representable) rather than to change the value.
func FromETB(etb float64) (Santim, error) {
	if math.IsNaN(etb) || math.IsInf(etb, 0) {
		return 0, fmt.Errorf("%w: %v", ErrUnparseableAmount, etb)
	}
	if etb < 0 {
		return 0, fmt.Errorf("%w: %v", ErrNegativeAmount, etb)
	}
	return Santim(math.Round(etb * SantimPerETB)), nil
}

// ETB renders the amount as a decimal ETB float for the verifier libraries,
// which compare after rounding to two places. Nothing in this system performs
// arithmetic on the result.
func (s Santim) ETB() float64 { return float64(s) / SantimPerETB }

// String formats the amount with two decimal places and no thousands
// separators or currency marker, e.g. "1500.00". Suitable for logs and API
// payloads that carry the currency separately.
func (s Santim) String() string {
	neg := s < 0
	v := s
	if neg {
		v = -v
	}
	out := fmt.Sprintf("%d.%02d", int64(v)/SantimPerETB, int64(v)%SantimPerETB)
	if neg {
		return "-" + out
	}
	return out
}

// Format renders the amount for display to a person, with thousands
// separators and the ETB marker, e.g. "1,500.00 ETB".
func (s Santim) Format() string {
	digits := s.String()
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	whole, frac, _ := strings.Cut(digits, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	out := b.String() + "." + frac + " ETB"
	if neg {
		return "-" + out
	}
	return out
}

// Mul multiplies the amount by a non-negative integer factor, such as a spot
// count or a week count.
func (s Santim) Mul(n int) Santim { return s * Santim(n) }

// Half returns half the amount, rounding down so a discount can never exceed
// the intended share.
func (s Santim) Half() Santim { return s / 2 }

// ParseETBText reads an amount out of free-form receipt text. Telebirr returns
// amounts exactly as printed — "1,500.00 Birr", "ETB 1,500.00", sometimes with
// non-breaking spaces — and performs no comparison of its own, so parsing
// here is what makes a Telebirr amount checkable at all.
//
// The parser is deliberately strict about the numeric part: it strips currency
// words, separators and whitespace, then requires what remains to be a plain
// decimal number. Text carrying two separate numbers, or a number in an
// unexpected shape, is rejected rather than guessed at, because a wrong guess
// would confirm a payment that never arrived.
func ParseETBText(text string) (Santim, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r == '.', r == '-', r == '+':
			return r
		default:
			return -1
		}
	}, normalizeAmountText(text))

	if !strings.ContainsAny(cleaned, "0123456789") {
		return 0, fmt.Errorf("%w: %q", ErrEmptyAmount, text)
	}
	value, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrUnparseableAmount, text)
	}
	santim, err := FromETB(value)
	if err != nil {
		return 0, fmt.Errorf("%w (from %q)", err, text)
	}
	return santim, nil
}

// normalizeAmountText removes the decorations Telebirr prints around an amount
// and the thousands separators inside it, leaving a bare number. Any leftover
// letters cause ParseETBText to fail, which is the intent.
func normalizeAmountText(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.NewReplacer(
		" ", " ", // non-breaking space
		"​", "", // zero-width space
		"birr", "",
		"etb", "",
		"ብር", "",
		",", "",
	).Replace(s)
	return strings.TrimSpace(s)
}
