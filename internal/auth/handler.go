package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler exposes the identity endpoints.
type Handler struct {
	svc *Service
	// strict is applied to the endpoints that create accounts, verify
	// credentials or send mail. PRD FR5a leans on rate limiting to offset the
	// absence of a mandatory email-verification gate.
	strict func(http.Handler) http.Handler
}

// NewHandler builds the identity handler. strictLimit may be nil, in which case
// only the global limiter applies.
func NewHandler(svc *Service, strictLimit func(http.Handler) http.Handler) *Handler {
	if strictLimit == nil {
		strictLimit = func(next http.Handler) http.Handler { return next }
	}
	return &Handler{svc: svc, strict: strictLimit}
}

// Routes mounts the public identity endpoints under /auth.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.strict)
			r.Method(http.MethodPost, "/signup", httpx.Handler(h.signup))
			r.Method(http.MethodPost, "/login", httpx.Handler(h.login))
			r.Method(http.MethodPost, "/google", httpx.Handler(h.google))
			r.Method(http.MethodPost, "/password-reset/request", httpx.Handler(h.requestReset))
			r.Method(http.MethodPost, "/password-reset/confirm", httpx.Handler(h.confirmReset))
		})
		r.Method(http.MethodPost, "/refresh", httpx.Handler(h.refresh))
		r.Method(http.MethodPost, "/logout", httpx.Handler(h.logout))
	})
}

// AdminRoutes mounts the identity endpoints the dashboard needs, and only
// those.
//
// Signup and Google sign-in are deliberately absent: on this API they would be
// a way to create an account against the admin trust domain from the outside.
// Admin accounts are made by promoting an existing player (PRD FR29), which is
// an audited action taken by someone already inside.
//
// Password reset is absent too, because the reset link points at the mobile
// app's deep link and there is no admin equivalent. An admin who is locked out
// is restored by another admin, which is the same shape as every other
// privileged change here.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.strict)
			// Sign-in on this binary additionally requires the admin role: the
			// service is built with RequireAdminRole, so a player's correct
			// credentials get 403 here rather than a usable admin token.
			r.Method(http.MethodPost, "/login", httpx.Handler(h.login))
		})
		r.Method(http.MethodPost, "/refresh", httpx.Handler(h.refresh))
		r.Method(http.MethodPost, "/logout", httpx.Handler(h.logout))
	})
}

// AdminAuthedRoutes mounts the signed-in self-service endpoints for the
// dashboard. Mount behind httpx.RequireAuth.
func (h *Handler) AdminAuthedRoutes(r chi.Router) {
	r.Method(http.MethodGet, "/me", httpx.Handler(h.me))
	r.Method(http.MethodPost, "/auth/password/change", httpx.Handler(h.changePassword))
	r.Method(http.MethodPost, "/auth/logout-all", httpx.Handler(h.logoutAll))
}

// AuthedRoutes mounts the endpoints that require a signed-in caller. Mount
// behind httpx.RequireAuth.
func (h *Handler) AuthedRoutes(r chi.Router) {
	r.Method(http.MethodGet, "/me", httpx.Handler(h.me))
	r.Method(http.MethodPost, "/auth/password/change", httpx.Handler(h.changePassword))
	r.Method(http.MethodPost, "/auth/logout-all", httpx.Handler(h.logoutAll))
}

// userResponse is the public shape of an account. The password hash and
// internal timestamps are not part of it.
type userResponse struct {
	ID            string  `json:"id"`
	FullName      string  `json:"full_name"`
	Email         string  `json:"email"`
	Age           *int32  `json:"age,omitempty"`
	Phone         *string `json:"phone,omitempty"`
	Role          string  `json:"role"`
	EmailVerified bool    `json:"email_verified"`
	// HasPassword lets the app hide "change password" for Google-only
	// accounts rather than offering an action that cannot succeed.
	HasPassword bool `json:"has_password"`
} //	@name	User

func toUserResponse(u db.User) userResponse {
	return userResponse{
		ID:            u.ID.String(),
		FullName:      u.FullName,
		Email:         u.Email,
		Age:           u.Age,
		Phone:         u.Phone,
		Role:          u.Role,
		EmailVerified: u.EmailVerifiedAt != nil,
		HasPassword:   u.PasswordHash != nil,
	}
}

type sessionResponse struct {
	User   userResponse `json:"user"`
	Tokens *TokenPair   `json:"tokens"`
} //	@name	Session

// loginRequest is the credential body. The same shape signs in to the public
// API and, with an admin account, to the dashboard.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
} //	@name	LoginRequest

// login authenticates with email and password.
//
//	@Summary		Sign in
//	@Description	Signs in with an email and password and returns a fresh token pair. On the admin API the account must additionally be an admin: correct player credentials are refused with 403.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.loginRequest	true	"Credentials"
//	@Success		200		{object}	auth.sessionResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope	"invalid_credentials: wrong email or password"
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account may not sign in here"
//	@Failure		422		{object}	httpx.ErrorEnvelope
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/login [post]
func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var req loginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	email := v.Email("email", req.Email)
	v.Check(req.Password != "", "password", "required")
	if err := v.Err(); err != nil {
		return err
	}

	user, pair, err := h.svc.Login(r.Context(), email, req.Password, r.UserAgent())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, sessionResponse{User: toUserResponse(*user), Tokens: pair})
	return nil
}

// refreshRequest carries the refresh token being exchanged or revoked.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
} //	@name	RefreshRequest

// refresh rotates a session (PRD FR3).
//
//	@Summary		Refresh a session
//	@Description	Exchanges a refresh token for a new token pair. Refresh tokens are single-use: replaying one that was already rotated revokes the whole session family and answers 401.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.refreshRequest	true	"Refresh token"
//	@Success		200		{object}	auth.TokenPair
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope	"token_expired or token_reused"
//	@Failure		422		{object}	httpx.ErrorEnvelope
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/refresh [post]
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	if req.RefreshToken == "" {
		return httpx.ErrValidation.WithField("refresh_token", "required")
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken, r.UserAgent())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, pair)
	return nil
}

// logout revokes the session a refresh token belongs to.
//
//	@Summary		Sign out
//	@Description	Revokes the session behind the refresh token. Revoking a session that is already dead is not an error.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body	auth.refreshRequest	true	"Refresh token"
//	@Success		204
//	@Failure		400	{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		422	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/auth/logout [post]
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	if req.RefreshToken == "" {
		return httpx.ErrValidation.WithField("refresh_token", "required")
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// logoutAll revokes every session the account has (PRD FR3).
//
//	@Summary		Sign out everywhere
//	@Description	Revokes every refresh token issued to the caller, on every device.
//	@Tags			account
//	@Security		BearerAuth
//	@Produce		json
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/auth/logout-all [post]
func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	if err := h.svc.LogoutAll(r.Context(), principal.UserID); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// changePasswordBody is the self-service password change.
type changePasswordBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
} //	@name	ChangePasswordRequest

// changePassword replaces the caller's password.
//
//	@Summary		Change password
//	@Description	Sets a new password and returns a fresh token pair. Other sessions are revoked; the returned pair is the one that keeps working. 409 when the account signs in with Google and has no password to change.
//	@Tags			account
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		auth.changePasswordBody	true	"Current and new password"
//	@Success		200		{object}	auth.TokenPair
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		409		{object}	httpx.ErrorEnvelope	"conflict: this account has no password yet"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: current password incorrect, or the new one is too weak"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/auth/password/change [post]
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	var req changePasswordBody
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	v.Check(req.CurrentPassword != "", "current_password", "required")
	v.Check(req.NewPassword != "", "new_password", "required")
	if err := v.Err(); err != nil {
		return err
	}

	pair, err := h.svc.ChangePassword(r.Context(), principal.UserID, req.CurrentPassword, req.NewPassword, r.UserAgent())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, pair)
	return nil
}

// me returns the signed-in account.
//
//	@Summary		Get my account
//	@Description	The caller's own account, as the app renders it.
//	@Tags			account
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	auth.userResponse
//	@Failure		401	{object}	httpx.ErrorEnvelope	"The token is invalid, or the account no longer exists"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/me [get]
func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	user, err := h.svc.store.GetUserByID(r.Context(), principal.UserID)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrUnauthorized
		}
		return err
	}
	httpx.JSON(w, r, http.StatusOK, toUserResponse(user))
	return nil
}
