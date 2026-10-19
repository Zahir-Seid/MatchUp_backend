package httpx

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Validator accumulates field-level problems so one response can report every
// invalid field instead of making the client fix them one round trip at a time.
type Validator struct {
	fields map[string]string
}

// NewValidator returns an empty validator.
func NewValidator() *Validator { return &Validator{fields: map[string]string{}} }

// Add records a problem for a field. The first problem per field wins, since it
// is usually the most specific.
func (v *Validator) Add(field, problem string) {
	if _, exists := v.fields[field]; !exists {
		v.fields[field] = problem
	}
}

// Check records problem for field when ok is false.
func (v *Validator) Check(ok bool, field, problem string) {
	if !ok {
		v.Add(field, problem)
	}
}

// Required rejects empty and whitespace-only values, returning the trimmed
// value for the caller to use.
func (v *Validator) Required(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		v.Add(field, "required")
	}
	return trimmed
}

// Length enforces an inclusive rune-count range on an already-trimmed value.
func (v *Validator) Length(field, value string, min, max int) {
	if value == "" {
		return
	}
	n := utf8.RuneCountInString(value)
	switch {
	case n < min:
		v.Add(field, "must be at least "+strconv.Itoa(min)+" characters")
	case n > max:
		v.Add(field, "must be at most "+strconv.Itoa(max)+" characters")
	}
}

var (
	// Ethiopian mobile numbers, accepted as 09xxxxxxxx, 07xxxxxxxx or the
	// +251 international form. Phone is contact information only; it is never
	// an identity or a login credential.
	phonePattern = regexp.MustCompile(`^(?:\+251|0)(9|7)\d{8}$`)
	// CBE account suffixes are the eight trailing digits of the payer's
	// account number.
	suffixPattern = regexp.MustCompile(`^\d{8}$`)
	// merchantAccountPattern matches a receiving account number: digits only,
	// long enough to be an account, capped so a pasted sentence cannot become
	// the account the worker checks receipts against.
	merchantAccountPattern = regexp.MustCompile(`^[0-9]{8,20}$`)
	// Transaction references from both providers are alphanumeric tokens;
	// receipt URLs are normalized before this check.
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{4,128}$`)
)

// Email validates an address and returns it lowercased and trimmed.
func (v *Validator) Email(field, value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		v.Add(field, "required")
		return ""
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil || addr.Address != trimmed || !strings.Contains(trimmed, ".") {
		v.Add(field, "must be a valid email address")
		return trimmed
	}
	v.Length(field, trimmed, 3, 254)
	return trimmed
}

// Phone validates an optional Ethiopian mobile number, returning it trimmed.
func (v *Validator) Phone(field, value string) string {
	trimmed := strings.ReplaceAll(strings.TrimSpace(value), " ", "")
	if trimmed == "" {
		return ""
	}
	if !phonePattern.MatchString(trimmed) {
		v.Add(field, "must be an Ethiopian mobile number, e.g. 0912345678")
	}
	return trimmed
}

// AccountSuffix validates a CBE payer account suffix.
func (v *Validator) AccountSuffix(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if !suffixPattern.MatchString(trimmed) {
		v.Add(field, "must be the 8 digits of the paying account after 1000")
	}
	return trimmed
}

// MerchantAccount validates a receiving account number for payment settings.
// Empty is accepted and means "clear the override": the worker falls back to
// the environment bootstrap value.
func (v *Validator) MerchantAccount(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if !merchantAccountPattern.MatchString(trimmed) {
		v.Add(field, "must be 8-20 digits, or empty to fall back to the configured account")
	}
	return trimmed
}

// Reference validates a payment transaction reference.
func (v *Validator) Reference(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		v.Add(field, "required")
		return ""
	}
	if !referencePattern.MatchString(trimmed) {
		v.Add(field, "does not look like a transaction reference")
	}
	return trimmed
}

// UUID parses an identifier supplied in a request body.
func (v *Validator) UUID(field, value string) uuid.UUID {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		v.Add(field, "required")
		return uuid.Nil
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		v.Add(field, "must be a valid id")
		return uuid.Nil
	}
	return id
}

// Range enforces an inclusive integer range.
func (v *Validator) Range(field string, value, min, max int) {
	if value < min || value > max {
		v.Add(field, "must be between "+strconv.Itoa(min)+" and "+strconv.Itoa(max))
	}
}

// OneOf enforces membership in a fixed set of allowed values.
func (v *Validator) OneOf(field, value string, allowed ...string) string {
	trimmed := strings.TrimSpace(value)
	for _, a := range allowed {
		if trimmed == a {
			return trimmed
		}
	}
	v.Add(field, "must be one of: "+strings.Join(allowed, ", "))
	return trimmed
}

// Future requires a timestamp to be later than now.
func (v *Validator) Future(field string, t time.Time) {
	if !t.After(time.Now()) {
		v.Add(field, "must be in the future")
	}
}

// Err returns a validation *Error carrying every recorded problem, or nil when
// the payload is acceptable.
func (v *Validator) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	err := ErrValidation
	for field, problem := range v.fields {
		err = err.WithField(field, problem)
	}
	return err
}
