// Package httpx holds the HTTP plumbing shared by the public and admin APIs:
// the error envelope, JSON encoding/decoding helpers, and middleware.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Clients branch on these
// strings, so they are part of the API contract and must not be renamed once
// released. The registry mirrors SDD Appendix B and extends it with the codes
// the implementation actually needs.
type Code string //	@name	ErrorCode

const (
	// Request-shape and generic failures.
	CodeBadRequest    Code = "bad_request"
	CodeValidation    Code = "validation_failed"
	CodeNotFound      Code = "not_found"
	CodeConflict      Code = "conflict"
	CodeRateLimited   Code = "rate_limited"
	CodeInternal      Code = "internal_error"
	CodeUnavailable   Code = "service_unavailable"
	CodeUnsupported   Code = "unsupported_media_type"
	CodePayloadTooBig Code = "payload_too_large"

	// Authentication and authorization.
	CodeUnauthorized      Code = "unauthorized"
	CodeForbidden         Code = "forbidden"
	CodeInvalidCredential Code = "invalid_credentials"
	CodeEmailInUse        Code = "email_in_use"
	CodeTokenExpired      Code = "token_expired"
	CodeTokenReused       Code = "token_reused"
	CodeAccountDisabled   Code = "account_disabled"

	// Catalog and booking.
	CodeInsufficientCapacity Code = "insufficient_capacity"
	CodeGameUnavailable      Code = "game_unavailable"
	CodeGameNotFull          Code = "game_not_full"
	CodeSeriesIncomplete     Code = "series_incomplete"
	CodeHoldExpired          Code = "hold_expired"
	CodeBookingNotPending    Code = "booking_not_pending"
	CodeBookingNotCancelable Code = "booking_not_cancelable"

	// Payment.
	CodePaymentReferenceReused Code = "payment_reference_reused"
	CodePaymentMismatch        Code = "payment_mismatch"
	CodePaymentNotFound        Code = "payment_not_found"
	CodeSuffixRequired         Code = "account_suffix_required"
	CodeIdempotencyConflict    Code = "idempotency_key_conflict"
	CodeVerifierUnavailable    Code = "verifier_unavailable"
)

// Error is an API-level failure carrying the HTTP status, the stable code, a
// message safe to show a user, optional per-field detail, and an internal cause
// that is logged but never serialized.
type Error struct {
	Status  int
	Code    Code
	Message string
	Fields  map[string]string
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to errors.Is/As without putting it on the
// wire.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying error for logging. The returned value is a
// copy, so package-level sentinels stay immutable.
func (e *Error) WithCause(err error) *Error {
	clone := *e
	clone.cause = err
	return &clone
}

// WithField adds a field-level validation detail.
func (e *Error) WithField(name, problem string) *Error {
	clone := *e
	clone.Fields = make(map[string]string, len(e.Fields)+1)
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	clone.Fields[name] = problem
	return &clone
}

// NewError builds an API error.
func NewError(status int, code Code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Common errors, defined once so wording stays consistent across handlers.
var (
	ErrBadRequest   = NewError(http.StatusBadRequest, CodeBadRequest, "The request could not be understood.")
	ErrValidation   = NewError(http.StatusUnprocessableEntity, CodeValidation, "Some fields need attention.")
	ErrUnauthorized = NewError(http.StatusUnauthorized, CodeUnauthorized, "Sign in to continue.")
	ErrForbidden    = NewError(http.StatusForbidden, CodeForbidden, "You do not have access to this.")
	ErrNotFound     = NewError(http.StatusNotFound, CodeNotFound, "Not found.")
	ErrRateLimited  = NewError(http.StatusTooManyRequests, CodeRateLimited, "Too many requests. Please slow down.")
	ErrInternal     = NewError(http.StatusInternalServerError, CodeInternal, "Something went wrong on our side.")
	ErrUnavailable  = NewError(http.StatusServiceUnavailable, CodeUnavailable, "Temporarily unavailable. Please try again shortly.")
)

// ErrorEnvelope is the wire format for every failure: {"error":{...}}. It is
// exported because the generated API documentation references it.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
} //	@name	ErrorEnvelope

// ErrorBody is the shape inside the error envelope. Fields is present only for
// validation failures; RequestID echoes the X-Request-ID header when there is
// one, so a user can quote it in a report.
type ErrorBody struct {
	Code      Code              `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
} //	@name	ErrorBody

// AsError extracts an *Error from an error chain, falling back to a generic
// internal error. Handlers can therefore return domain errors directly and let
// the writer decide what the client sees.
func AsError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return ErrInternal.WithCause(err)
}
