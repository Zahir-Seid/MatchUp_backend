package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// UserView is an account as the dashboard shows it. The password hash and the
// OAuth linkage are absent: neither is any use to an admin, and both are worth
// keeping out of a response that a browser caches and a support agent screen
// shares.
type UserView struct {
	ID              uuid.UUID  `json:"id"`
	FullName        string     `json:"full_name"`
	Email           string     `json:"email"`
	Phone           *string    `json:"phone,omitempty"`
	Age             *int32     `json:"age,omitempty"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
} //	@name	UserView

func toUserView(u db.User) UserView {
	return UserView{
		ID:              u.ID,
		FullName:        u.FullName,
		Email:           u.Email,
		Phone:           u.Phone,
		Age:             u.Age,
		Role:            u.Role,
		Status:          u.Status,
		EmailVerifiedAt: u.EmailVerifiedAt,
		CreatedAt:       u.CreatedAt,
	}
}

// UserFilter narrows the account list.
type UserFilter struct {
	Search *string
	Status *string
	Limit  int32
	Offset int32
}

// ListUsers returns accounts newest first (PRD FR29).
func (s *Service) ListUsers(ctx context.Context, f UserFilter) (Page[UserView], error) {
	rows, err := s.store.ListUsers(ctx, db.ListUsersParams{
		Limit:  f.Limit,
		Offset: f.Offset,
		Search: f.Search,
		Status: f.Status,
	})
	if err != nil {
		return Page[UserView]{}, fmt.Errorf("list users: %w", err)
	}
	total, err := s.store.CountUsers(ctx, db.CountUsersParams{
		Search: f.Search,
		Status: f.Status,
	})
	if err != nil {
		return Page[UserView]{}, fmt.Errorf("count users: %w", err)
	}

	views := make([]UserView, 0, len(rows))
	for _, u := range rows {
		views = append(views, toUserView(u))
	}
	return newPage(views, total, f.Limit, f.Offset), nil
}

// GetUser returns one account.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*UserView, error) {
	user, err := s.store.GetUserByID(ctx, id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	view := toUserView(user)
	return &view, nil
}

// SetUserStatus disables or restores an account (PRD FR29).
//
// Disabling does not cancel the account's bookings. It is about future access,
// and silently voiding paid reservations would turn a moderation decision into a
// financial one; the bookings are cancelled deliberately, one at a time, with
// their own recorded reasons.
func (s *Service) SetUserStatus(ctx context.Context, actor, id uuid.UUID, status, reason string) (*UserView, error) {
	if err := s.assertNotSelf(actor, id, "status"); err != nil {
		return nil, err
	}

	var out UserView
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetUserByID(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get user: %w", err)
		}
		if before.Status == status {
			out = toUserView(before)
			return nil
		}

		user, err := q.SetUserStatus(ctx, db.SetUserStatusParams{ID: id, Status: status})
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("set user status: %w", err)
		}
		out = toUserView(user)

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionUserStatusChanged,
			TargetType: audit.TargetUser,
			TargetID:   user.ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"email": user.Email,
				"from":  before.Status,
				"to":    user.Status,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetUserRole grants or revokes admin access (PRD FR29).
//
// Existing access tokens are not revoked. An access token carries the role it
// was minted with and lives for minutes, so a demoted admin keeps admin
// endpoints for the rest of that window. Disabling the account is the immediate
// lever; a role change is an administrative one.
func (s *Service) SetUserRole(ctx context.Context, actor, id uuid.UUID, role, reason string) (*UserView, error) {
	if err := s.assertNotSelf(actor, id, "role"); err != nil {
		return nil, err
	}

	var out UserView
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetUserByID(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get user: %w", err)
		}
		if before.Role == role {
			// The query's role <> $2 guard would return no rows here, which is
			// success rather than a missing user; answering with the account as
			// it stands says the same thing without the ambiguity.
			out = toUserView(before)
			return nil
		}
		if role == auth.RoleAdmin && before.Status != auth.StatusActive {
			return httpx.ErrValidation.WithField("role",
				"only an active account can be made an admin")
		}

		user, err := q.SetUserRole(ctx, db.SetUserRoleParams{ID: id, Role: role})
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("set user role: %w", err)
		}
		out = toUserView(user)

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionUserRoleChanged,
			TargetType: audit.TargetUser,
			TargetID:   user.ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"email": user.Email,
				"from":  before.Role,
				"to":    user.Role,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// assertNotSelf blocks an admin from changing their own role or status. The
// failure mode it prevents is a single-admin deployment locking itself out with
// one request, which nobody left inside can undo.
func (s *Service) assertNotSelf(actor, target uuid.UUID, what string) error {
	if actor == target {
		return httpx.ErrForbidden.WithField(what,
			"you cannot change your own "+what+"; ask another admin")
	}
	return nil
}

// ---- HTTP ----

// listUsers returns accounts newest first (PRD FR29).
//
//	@Summary		List users
//	@Description	One page of accounts, newest first, searchable by name or email and filterable by status.
//	@Tags			users
//	@Security		BearerAuth
//	@Produce		json
//	@Param			status	query		string	false	"Filter to one status"	Enums(active, disabled)
//	@Param			search	query		string	false	"Match the name or email (2-120 characters)"
//	@Param			limit	query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset	query		int		false	"Rows to skip (default 0)"
//	@Success		200		{object}	admin.Page[admin.UserView]
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/users [get]
func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := parsePagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v := httpx.NewValidator()
	filter := UserFilter{Limit: limit, Offset: offset}

	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		status := v.OneOf("status", raw, auth.StatusActive, auth.StatusDisabled)
		filter.Status = &status
	}
	if search := optionalText(q.Get("search")); search != nil {
		v.Length("search", *search, 2, 120)
		filter.Search = search
	}
	if err := v.Err(); err != nil {
		return err
	}

	page, err := h.svc.ListUsers(r.Context(), filter)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, page)
	return nil
}

// getUser returns one account (PRD FR29).
//
//	@Summary		Get a user
//	@Description	One account as the dashboard shows it. The password hash and OAuth linkage are deliberately absent.
//	@Tags			users
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		string	true	"User id (UUID)"
//	@Success		200	{object}	admin.UserView
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/users/{id} [get]
func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) error {
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	user, err := h.svc.GetUser(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, user)
	return nil
}

// userStatusRequest is the status change to apply.
type userStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
} //	@name	UserStatusRequest

// setUserStatus disables or restores an account (PRD FR29).
//
//	@Summary		Change a user's status
//	@Description	Disables or restores an account. Nothing is deleted: erasing an account would orphan its bookings, payments and audit entries, and disabling already stops every use of it. Disabling does not cancel bookings — that is a separate, reasoned action. An admin cannot change their own status.
//	@Tags			users
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"User id (UUID)"
//	@Param			body	body		admin.userStatusRequest	true	"Status change"
//	@Success		200		{object}	admin.UserView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin, or you are changing your own status"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/users/{id}/status [patch]
func (h *Handler) setUserStatus(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req userStatusRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	// The two values the users table allows. There is deliberately no delete:
	// erasing an account would orphan the bookings, payments and audit entries
	// that refer to it, and disabling already stops every use of it.
	status := v.OneOf("status", req.Status, auth.StatusActive, auth.StatusDisabled)
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	user, err := h.svc.SetUserStatus(r.Context(), principal.UserID, id, status, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, user)
	return nil
}

// userRoleRequest is the role change to apply.
type userRoleRequest struct {
	Role   string `json:"role"`
	Reason string `json:"reason"`
} //	@name	UserRoleRequest

// setUserRole grants or revokes admin access (PRD FR29).
//
//	@Summary		Change a user's role
//	@Description	Promotes a player to admin or demotes an admin back. Only an active account can be made an admin, and an admin cannot change their own role. Tokens already issued keep the role they were minted with until they expire; disabling the account is the immediate lever.
//	@Tags			users
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"User id (UUID)"
//	@Param			body	body		admin.userRoleRequest	true	"Role change"
//	@Success		200		{object}	admin.UserView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin, or you are changing your own role"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/users/{id}/role [patch]
func (h *Handler) setUserRole(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req userRoleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	role := v.OneOf("role", req.Role, auth.RolePlayer, auth.RoleAdmin)
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return err
	}

	user, err := h.svc.SetUserRole(r.Context(), principal.UserID, id, role, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, user)
	return nil
}
