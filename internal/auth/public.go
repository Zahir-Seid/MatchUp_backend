package auth

import (
	"net/http"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// This file holds the identity endpoints that exist only on the public API:
// signup, Google sign-in and password reset. The admin binary intentionally
// mounts none of them (see AdminRoutes), and the file boundary is also what
// `make docs` uses to keep their operations out of the admin specification.

// messageResponse is a one-line acknowledgement. Both password-reset endpoints
// answer with it, and requestReset answers with it whether or not the address
// is registered, so the response cannot be used to enumerate accounts.
type messageResponse struct {
	Message string `json:"message"`
} //	@name	Message

// signupRequest is the account-creation body.
type signupRequest struct {
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Age      *int32 `json:"age,omitempty"`
	Phone    string `json:"phone"`
} //	@name	SignupRequest

// signup registers an account and signs it in (PRD FR1).
//
//	@Summary		Create an account
//	@Description	Registers a player account and returns it with a fresh token pair. Rate limited more strictly than the rest of the API.
//	@Tags			auth, public
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.signupRequest	true	"Account details"
//	@Success		201		{object}	auth.sessionResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"email_in_use: that email already has an account"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: a field needs attention"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/signup [post]
func (h *Handler) signup(w http.ResponseWriter, r *http.Request) error {
	var req signupRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	fullName := v.Required("full_name", req.FullName)
	v.Length("full_name", fullName, 2, 120)
	email := v.Email("email", req.Email)
	v.Check(req.Password != "", "password", "required")
	phone := v.Phone("phone", req.Phone)
	if req.Age != nil {
		// Matches the age CHECK on the users table.
		v.Range("age", int(*req.Age), 10, 120)
	}
	if err := v.Err(); err != nil {
		return err
	}

	user, pair, err := h.svc.Signup(r.Context(), SignupInput{
		FullName:  fullName,
		Email:     email,
		Password:  req.Password,
		Age:       req.Age,
		Phone:     phone,
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusCreated, sessionResponse{User: toUserResponse(*user), Tokens: pair})
	return nil
}

// googleRequest carries the ID token Google Sign-In produced on the device.
type googleRequest struct {
	IDToken string `json:"id_token"`
} //	@name	GoogleSignInRequest

// google exchanges a Google ID token for a MatchUp session, creating the
// account on first sign-in (PRD FR2).
//
//	@Summary		Sign in with Google
//	@Description	Verifies a Google ID token and returns a MatchUp session, registering the account when it is new.
//	@Tags			auth, public
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.googleRequest	true	"Google ID token"
//	@Success		200		{object}	auth.sessionResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope	"invalid_credentials: the Google token was not accepted"
//	@Failure		422		{object}	httpx.ErrorEnvelope
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Failure		503		{object}	httpx.ErrorEnvelope	"service_unavailable: Google sign-in is not configured"
//	@Router			/auth/google [post]
func (h *Handler) google(w http.ResponseWriter, r *http.Request) error {
	var req googleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	v.Check(req.IDToken != "", "id_token", "required")
	if err := v.Err(); err != nil {
		return err
	}

	user, pair, err := h.svc.GoogleSignIn(r.Context(), req.IDToken, r.UserAgent())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, sessionResponse{User: toUserResponse(*user), Tokens: pair})
	return nil
}

// resetRequestBody asks for a password-reset mail.
type resetRequestBody struct {
	Email string `json:"email"`
} //	@name	PasswordResetRequest

// requestReset mails a reset link (PRD FR4).
//
//	@Summary		Request a password reset
//	@Description	Mails a reset link when the address has an account. The response is identical either way, so it cannot be used to discover which emails are registered.
//	@Tags			auth, public
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.resetRequestBody	true	"Account email"
//	@Success		202		{object}	auth.messageResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		422		{object}	httpx.ErrorEnvelope
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/password-reset/request [post]
func (h *Handler) requestReset(w http.ResponseWriter, r *http.Request) error {
	var req resetRequestBody
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	email := v.Email("email", req.Email)
	if err := v.Err(); err != nil {
		return err
	}
	if err := h.svc.RequestPasswordReset(r.Context(), email); err != nil {
		return err
	}
	// Always the same response, whether or not the address is registered.
	httpx.JSON(w, r, http.StatusAccepted, messageResponse{
		Message: "If that address has an account, a reset link is on its way.",
	})
	return nil
}

// resetConfirmBody redeems a reset link.
type resetConfirmBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
} //	@name	PasswordResetConfirmRequest

// confirmReset sets a new password from a reset link.
//
//	@Summary		Confirm a password reset
//	@Description	Sets the account's new password using the token from the reset link, and revokes every existing session.
//	@Tags			auth, public
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.resetConfirmBody	true	"Reset token and new password"
//	@Success		200		{object}	auth.messageResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"token_expired: the link has expired or is not valid"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"conflict: the link has already been used"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: the new password is too weak"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/password-reset/confirm [post]
func (h *Handler) confirmReset(w http.ResponseWriter, r *http.Request) error {
	var req resetConfirmBody
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	v.Check(req.Token != "", "token", "required")
	v.Check(req.Password != "", "password", "required")
	if err := v.Err(); err != nil {
		return err
	}
	if err := h.svc.ResetPassword(r.Context(), req.Token, req.Password); err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, messageResponse{
		Message: "Your password has been changed. Please sign in again.",
	})
	return nil
}
