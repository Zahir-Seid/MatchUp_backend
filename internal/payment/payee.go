package payment

import (
	"strings"
	"unicode"
)

// PayeeVerdict is the result of checking who received the money.
//
// This check exists because neither verifier library performs it: the CBE
// library compares the amount (and, for legacy references, the reference), and
// Telebirr's returns the receipt as printed. Nothing in either one asks whether
// the money reached *our* account, so a payer could otherwise settle a real
// transfer to somebody else and have it accepted.
type PayeeVerdict string

const (
	// PayeeMatch means the receiving account is ours.
	PayeeMatch PayeeVerdict = "match"
	// PayeeMismatch means it is demonstrably not ours.
	PayeeMismatch PayeeVerdict = "mismatch"
	// PayeeInconclusive means the receipt masks too much of the account to
	// decide. It is deliberately NOT treated as a pass: the payment is flagged
	// for a human, with the spots still held, rather than confirmed on a guess.
	PayeeInconclusive PayeeVerdict = "inconclusive"
	// PayeeUnknown means the receipt carried no receiving account at all.
	PayeeUnknown PayeeVerdict = "unknown"
)

// maskRunes are the characters banks use to redact an account number. CBE's
// receipts use ASCII asterisks; the bullet appears in some rendered PDFs.
const maskRunes = "*•·xX"

// minVisibleDigits is how many digits a masked account must still show before a
// match on them means anything. CBE masks to four trailing digits, which is
// 1-in-10,000 -- weak on its own, which is why a masked match is only ever
// accepted together with the amount check, and why fewer than four is refused
// outright.
const minVisibleDigits = 4

// ComparePayee decides whether reported (the account the bank says received the
// money, possibly masked, e.g. "1000****6789") is ours (the full merchant
// account from configuration).
//
// A masked account is compared on the digits it still shows: every visible digit
// must agree with ours at the same position from its own end. That is what a
// masked receipt can support, no more.
func ComparePayee(reported, ours string) PayeeVerdict {
	reported = normalizeAccount(reported)
	ours = normalizeAccount(ours)

	if reported == "" {
		return PayeeUnknown
	}
	if ours == "" {
		// No merchant account configured to compare against. Config validation
		// makes this unreachable outside local, and refusing to guess is the
		// only safe answer.
		return PayeeInconclusive
	}

	if !isMasked(reported) {
		return compareUnmasked(reported, ours)
	}

	prefix, suffix := visibleEnds(reported)
	if len(prefix)+len(suffix) < minVisibleDigits {
		return PayeeInconclusive
	}
	// The masked form must still be able to describe our account: a reported
	// account longer than ours is a different account, not a redaction of it.
	if len(reported) > len(ours) && len(prefix) > 0 {
		return PayeeMismatch
	}
	if suffix != "" && !strings.HasSuffix(ours, suffix) {
		return PayeeMismatch
	}
	if prefix != "" && !strings.HasPrefix(ours, prefix) {
		return PayeeMismatch
	}
	if suffix == "" {
		// A leading fragment alone ("1000****") is CBE's branch prefix, shared
		// by every account at the bank. It identifies nothing.
		return PayeeInconclusive
	}
	return PayeeMatch
}

// compareUnmasked compares two full account numbers. Telebirr returns the
// credited party in full, and CBE receipts occasionally do.
func compareUnmasked(reported, ours string) PayeeVerdict {
	if reported == ours {
		return PayeeMatch
	}
	// Banks pad and prefix account numbers inconsistently between the receipt
	// and the account as the business writes it down, so a shared tail of
	// sufficient length is accepted. Both must be at least that long for the
	// comparison to mean anything.
	const tail = 8
	if len(reported) >= tail && len(ours) >= tail {
		if reported[len(reported)-tail:] == ours[len(ours)-tail:] {
			return PayeeMatch
		}
		return PayeeMismatch
	}
	// One side is too short to compare on a tail and they are not equal.
	if strings.HasSuffix(ours, reported) || strings.HasSuffix(reported, ours) {
		return PayeeInconclusive
	}
	return PayeeMismatch
}

// normalizeAccount strips the separators receipts print inside account numbers
// and drops everything that is neither a digit nor a mask character.
func normalizeAccount(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case strings.ContainsRune(maskRunes, r):
			// Normalize every mask character to '*' so one representation is
			// enough downstream.
			b.WriteRune('*')
		}
	}
	return b.String()
}

func isMasked(s string) bool { return strings.ContainsRune(s, '*') }

// visibleEnds splits a masked account into the digits before the first mask and
// the digits after the last one. Digits between two masked runs are ignored:
// their position is not pinned to either end, so they cannot be compared safely.
func visibleEnds(masked string) (prefix, suffix string) {
	first := strings.IndexRune(masked, '*')
	last := strings.LastIndex(masked, "*")
	return masked[:first], masked[last+1:]
}
