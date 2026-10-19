package payment

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler exposes payment submission.
type Handler struct {
	svc *Service
	// strict throttles submission. Each attempt costs an outbound bank lookup
	// and burns a reference, so it is rate limited like the auth endpoints
	// (SDD 10).
	strict func(http.Handler) http.Handler
}

// NewHandler builds the payment handler. strictLimit may be nil.
func NewHandler(svc *Service, strictLimit func(http.Handler) http.Handler) *Handler {
	if strictLimit == nil {
		strictLimit = func(next http.Handler) http.Handler { return next }
	}
	return &Handler{svc: svc, strict: strictLimit}
}

// Routes mounts payment submission under the booking it pays for. Mount behind
// httpx.RequireAuth.
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.strict)
		r.Method(http.MethodPost, "/bookings/{id}/payment", httpx.Handler(h.submit))
	})
}

// submitRequest is the payment submission body. There is no amount field by
// design: the amount verified against the bank comes from the booking, so a
// client cannot ask us to accept less than the booking costs (PRD FR16).
type submitRequest struct {
	Method string `json:"method"`
	// Reference accepts what the payer has: a pasted CBE receipt link, a bare
	// FT reference, a CBE token, or a Telebirr reference.
	Reference string `json:"transaction_reference"`
	// AccountSuffix is only needed for a bare CBE FT reference.
	AccountSuffix string `json:"account_suffix,omitempty"`
	// IdempotencyKey lets the app retry a submission it is unsure landed
	// without risking a second payment record. The Idempotency-Key header is
	// an accepted alternative.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
} //	@name	PaymentRequest

// submit starts verifying a payment for one of the caller's own bookings.
//
//	@Summary		Submit a payment
//	@Description	Records a bank reference against the booking and checks it with the bank. Verification is asynchronous: 202 means the check has started, while 200 means an identical submission was replayed. Reusing a reference, or the same idempotency key with different contents, answers 409.
//	@Tags			payments, public
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"Booking id (UUID)"
//	@Param			body	body		payment.submitRequest	true	"Payment reference"
//	@Success		200		{object}	payment.SubmitView		"Replay of an earlier submission"
//	@Success		202		{object}	payment.SubmitView		"Verification started"
//	@Failure		400		{object}	httpx.ErrorEnvelope		"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found: no such booking"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"hold_expired, booking_not_pending, payment_reference_reused or idempotency_key_conflict"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed or account_suffix_required"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings/{id}/payment [post]
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	bookingID, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}

	var req submitRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	method := v.OneOf("method", req.Method, MethodCBE, MethodTelebirr)
	// The reference is checked for shape here and for meaning in Canonicalize;
	// this catches the empty and obviously-wrong cases with field-level detail.
	if !isPastedURL(req.Reference) {
		v.Reference("transaction_reference", req.Reference)
	}
	suffix := v.AccountSuffix("account_suffix", req.AccountSuffix)
	// The header is the conventional home for the key; the body field is
	// accepted too so a client that cannot set headers is not locked out.
	key := req.IdempotencyKey
	if header := r.Header.Get("Idempotency-Key"); header != "" {
		key = header
	}
	idempotencyKey := v.Required("idempotency_key", key)
	v.Length("idempotency_key", idempotencyKey, 8, 128)
	if err := v.Err(); err != nil {
		return err
	}

	submission, err := Canonicalize(method, req.Reference, suffix)
	if err != nil {
		return err
	}

	payment, replayed, err := h.svc.Submit(r.Context(), SubmitInput{
		BookingID:      bookingID,
		UserID:         principal.UserID,
		Method:         method,
		Reference:      submission.Reference,
		AccountSuffix:  submission.AccountSuffix,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return err
	}

	// A replay is answered 200 with the original outcome; a fresh submission is
	// 202, since the work it started has not finished.
	status := http.StatusAccepted
	if replayed {
		status = http.StatusOK
	}
	httpx.JSON(w, r, status, toSubmitView(*payment))
	return nil
}

// isPastedURL reports whether the payer pasted a receipt link rather than a
// code. Links contain "://", which the reference pattern rejects, so they skip
// the shape check and go straight to classification.
func isPastedURL(reference string) bool {
	return len(reference) > 8 && (reference[:7] == "http://" || reference[:8] == "https://")
}
