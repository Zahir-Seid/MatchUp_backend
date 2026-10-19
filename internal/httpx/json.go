package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxBodyBytes caps request bodies. Every endpoint in this system posts small
// JSON documents; the cap stops a single request from consuming memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// Decode reads a JSON request body into dst, rejecting anything malformed,
// oversized, or carrying fields the endpoint does not define. Unknown fields are
// an error rather than a silent no-op so a client typo cannot look like a
// successful request that quietly ignored half the payload.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		media := strings.TrimSpace(strings.Split(ct, ";")[0])
		if !strings.EqualFold(media, "application/json") {
			return NewError(http.StatusUnsupportedMediaType, CodeUnsupported,
				"Send this request as application/json.")
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value in the stream means the client sent more than one
	// document; treat it as a malformed request rather than ignoring the rest.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrBadRequest.WithCause(errors.New("request body contained more than one JSON document"))
	}
	return nil
}

func decodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return ErrBadRequest.WithCause(err)
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return ErrValidation.WithField(field, fmt.Sprintf("expected %s", typeErr.Type.String())).WithCause(err)
	case errors.As(err, &maxErr):
		return NewError(http.StatusRequestEntityTooLarge, CodePayloadTooBig,
			"That request was too large.").WithCause(err)
	case errors.Is(err, io.EOF):
		return ErrBadRequest.WithCause(errors.New("request body was empty"))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return ErrValidation.WithField(name, "unrecognized field").WithCause(err)
	default:
		return ErrBadRequest.WithCause(err)
	}
}

// JSON writes a JSON response with the given status. Encoding failures are
// logged; the status line has already been sent by then, so the response is
// simply truncated rather than replaced with an error the client would
// misinterpret.
func JSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if payload == nil || status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		LoggerFrom(r.Context()).Error("write json response failed",
			slog.String("error", err.Error()))
	}
}

// NoContent writes a bodiless success response.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Fail writes err as the error envelope. Client-visible content comes only from
// the *Error; the internal cause is logged and dropped, so a database message
// or a verifier URL can never leak into a response.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := AsError(err)
	requestID := RequestIDFrom(r.Context())
	log := LoggerFrom(r.Context())

	attrs := []any{
		slog.String("code", string(apiErr.Code)),
		slog.Int("status", apiErr.Status),
	}
	if cause := apiErr.Unwrap(); cause != nil {
		attrs = append(attrs, slog.String("cause", cause.Error()))
	}
	if apiErr.Status >= 500 {
		log.Error(apiErr.Message, attrs...)
	} else {
		log.Warn(apiErr.Message, attrs...)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if apiErr.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="matchup"`)
	}
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{Error: ErrorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Fields:    apiErr.Fields,
		RequestID: requestID,
	}})
}

// Handler is a http.HandlerFunc that may return an error, letting handlers use
// `return` for failure paths instead of writing-and-remembering-to-return.
type Handler func(http.ResponseWriter, *http.Request) error

// ServeHTTP renders any returned error through the standard envelope.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		Fail(w, r, err)
	}
}
