package auth

import (
	"net/http"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// emailVerificationConfirmBody redeems a verification link.
type emailVerificationConfirmBody struct {
	Token string `json:"token"`
} //	@name	EmailVerificationConfirmRequest

// requestEmailVerification queues a confirmation link for the signed-in
// caller. It is idempotent: an address that is already verified gets the same
// answer without another mail.
//
//	@Summary		Request email verification
//	@Description	Queues a single-use verification link for the caller's address. Does nothing when the address is already verified.
//	@Tags			account, public
//	@Security		BearerAuth
//	@Produce		json
//	@Success		202	{object}	auth.messageResponse
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/auth/email-verification/request [post]
func (h *Handler) requestEmailVerification(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	if err := h.svc.RequestEmailVerification(r.Context(), principal.UserID); err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusAccepted, messageResponse{
		Message: "If your address still needs confirming, a verification link is on its way.",
	})
	return nil
}

// confirmEmailVerification marks the caller's address verified from a
// single-use link.
//
//	@Summary		Confirm an email address
//	@Description	Marks the account's email address verified using the token from the confirmation link.
//	@Tags			auth, public
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.emailVerificationConfirmBody	true	"Verification token"
//	@Success		200		{object}	auth.messageResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"token_expired: the link has expired or is not valid"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"conflict: the link has already been used"
//	@Failure		422		{object}	httpx.ErrorEnvelope
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/email-verification/confirm [post]
func (h *Handler) confirmEmailVerification(w http.ResponseWriter, r *http.Request) error {
	var req emailVerificationConfirmBody
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	v.Check(req.Token != "", "token", "required")
	if err := v.Err(); err != nil {
		return err
	}
	if err := h.svc.ConfirmEmailVerification(r.Context(), req.Token); err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, messageResponse{
		Message: "Your email address has been confirmed.",
	})
	return nil
}
