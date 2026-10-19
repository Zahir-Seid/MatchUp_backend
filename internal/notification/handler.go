package notification

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler exposes the in-app feed. Every route is owner-scoped.
type Handler struct {
	svc *Service
}

// NewHandler builds the notification handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the notification endpoints. Mount behind httpx.RequireAuth.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/notifications", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.list))
		r.Method(http.MethodPost, "/read-all", httpx.Handler(h.readAll))
		r.Method(http.MethodPost, "/{id}/read", httpx.Handler(h.read))
	})
}

type listResponse struct {
	Notifications []View `json:"notifications"`
	UnreadCount   int64  `json:"unread_count"`
	Limit         int32  `json:"limit"`
	Offset        int32  `json:"offset"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := catalog.ParsePagination(r)
	if err != nil {
		return err
	}
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}

	unreadOnly := strings.EqualFold(r.URL.Query().Get("unread"), "true")
	views, unread, err := h.svc.List(r.Context(), principal.UserID, unreadOnly, limit, offset)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, listResponse{
		Notifications: views,
		UnreadCount:   unread,
		Limit:         limit,
		Offset:        offset,
	})
	return nil
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) error {
	id, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	if err := h.svc.MarkRead(r.Context(), principal.UserID, id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

type readAllResponse struct {
	MarkedRead int64 `json:"marked_read"`
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	count, err := h.svc.MarkAllRead(r.Context(), principal.UserID)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, readAllResponse{MarkedRead: count})
	return nil
}
