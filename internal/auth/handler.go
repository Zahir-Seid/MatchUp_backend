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
}

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
}

type signupRequest struct {
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Age      *int32 `json:"age"`
	Phone    string `json:"phone"`
}

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

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

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

type googleRequest struct {
	IDToken string `json:"id_token"`
}

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

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

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

type resetRequestBody struct {
	Email string `json:"email"`
}

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
	httpx.JSON(w, r, http.StatusAccepted, map[string]string{
		"message": "If that address has an account, a reset link is on its way.",
	})
	return nil
}

type resetConfirmBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

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
	httpx.JSON(w, r, http.StatusOK, map[string]string{
		"message": "Your password has been changed. Please sign in again.",
	})
	return nil
}

type changePasswordBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

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
