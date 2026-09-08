package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is deliberately above the library default. Sign-in is not a hot
// path here, and the extra work is what makes an offline attack on a leaked
// hash table impractical.
const bcryptCost = 12

// Password policy bounds. The upper bound exists because bcrypt silently
// ignores input beyond 72 bytes: without it, two different long passwords
// could authenticate the same account.
const (
	minPasswordLength = 10
	maxPasswordBytes  = 72
)

// ErrWeakPassword is returned when a password fails policy. The message is
// safe to show the user.
type ErrWeakPassword struct{ Reason string }

func (e *ErrWeakPassword) Error() string { return "weak password: " + e.Reason }

// hashPassword applies the policy and returns a bcrypt hash.
func hashPassword(plain string) (string, error) {
	if err := checkPasswordStrength(plain); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// verifyPassword compares a candidate against a stored hash in constant time.
func verifyPassword(hash, candidate string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(candidate)) == nil
}

// checkPasswordStrength enforces length and character variety. It rejects
// passwords bcrypt would truncate, and requires a mix that rules out the
// single-dictionary-word case without demanding an unusable pattern.
func checkPasswordStrength(plain string) error {
	switch {
	case len([]rune(plain)) < minPasswordLength:
		return &ErrWeakPassword{Reason: fmt.Sprintf("must be at least %d characters", minPasswordLength)}
	case len(plain) > maxPasswordBytes:
		return &ErrWeakPassword{Reason: fmt.Sprintf("must be at most %d bytes", maxPasswordBytes)}
	case strings.TrimSpace(plain) == "":
		return &ErrWeakPassword{Reason: "must not be only whitespace"}
	}

	var hasLetter, hasNonLetter bool
	for _, r := range plain {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		default:
			hasNonLetter = true
		}
	}
	if !hasLetter || !hasNonLetter {
		return &ErrWeakPassword{Reason: "must combine letters with at least one number or symbol"}
	}
	return nil
}

// AsWeakPassword extracts a policy failure from an error chain.
func AsWeakPassword(err error) (*ErrWeakPassword, bool) {
	var weak *ErrWeakPassword
	ok := errors.As(err, &weak)
	return weak, ok
}
