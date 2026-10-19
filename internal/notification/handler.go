package notification

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler exposes the in-app feed and device registration. Every route is
// owner-scoped.
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
	r.Route("/devices", func(r chi.Router) {
		r.Method(http.MethodPost, "/", httpx.Handler(h.registerDevice))
		r.Method(http.MethodDelete, "/", httpx.Handler(h.unregisterDevice))
	})
}

// listResponse is one page of the feed.
type listResponse struct {
	Notifications []View `json:"notifications"`
	UnreadCount   int64  `json:"unread_count"`
	Limit         int32  `json:"limit"`
	Offset        int32  `json:"offset"`
} //	@name	NotificationList

// list returns a page of the caller's feed (PRD FR22).
//
//	@Summary		List my notifications
//	@Description	One page of the caller's in-app feed, newest first, with the total unread count.
//	@Tags			notifications, public
//	@Security		BearerAuth
//	@Produce		json
//	@Param			unread	query		bool	false	"Only unread notifications"
//	@Param			limit	query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset	query		int		false	"Rows to skip (default 0)"
//	@Success		200		{object}	notification.listResponse
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: bad paging"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/notifications [get]
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

// read marks one notification as read. Marking an already-read notification,
// or an id that is not theirs, is deliberately not an error.
//
//	@Summary		Mark a notification read
//	@Description	Marks one of the caller's own notifications as read. An id that matches nothing, or one that was already read, still answers 204.
//	@Tags			notifications, public
//	@Security		BearerAuth
//	@Param			id	path	string	true	"Notification id (UUID)"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found: the id is not a UUID"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/notifications/{id}/read [post]
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

// readAllResponse reports how many rows changed.
type readAllResponse struct {
	MarkedRead int64 `json:"marked_read"`
} //	@name	ReadAllResult

// readAll marks the whole feed read (PRD FR22).
//
//	@Summary		Mark all notifications read
//	@Description	Marks every unread notification in the caller's feed as read and reports how many changed.
//	@Tags			notifications, public
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	notification.readAllResponse
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/notifications/read-all [post]
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

// deviceRequest registers or drops one FCM registration token. Unregistration
// ignores the platform: the token alone identifies the device.
type deviceRequest struct {
	FCMToken string `json:"fcm_token"`
	Platform string `json:"platform"`
} //	@name	DeviceRequest

// registerDevice records or refreshes the caller's FCM token.
//
//	@Summary		Register a device
//	@Description	Records or refreshes the FCM registration token for the device the caller is signed in on. The token is unique system-wide: a phone that changes hands moves to the new account rather than delivering one user's notifications to another.
//	@Tags			notifications, public
//	@Security		BearerAuth
//	@Accept			json
//	@Param			body	body	notification.deviceRequest	true	"FCM registration token and platform"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		422	{object}	httpx.ErrorEnvelope	"validation_failed: bad token or platform"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/devices [post]
func (h *Handler) registerDevice(w http.ResponseWriter, r *http.Request) error {
	var req deviceRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	v := httpx.NewValidator()
	token := v.Required("fcm_token", req.FCMToken)
	v.Length("fcm_token", token, 32, 512)
	platform := v.OneOf("platform", req.Platform, "ios", "android")
	if err := v.Err(); err != nil {
		return err
	}

	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	if err := h.svc.RegisterDevice(r.Context(), principal.UserID, token, platform); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// unregisterDevice drops a token at sign-out. The token is the identifier, so
// no ownership check is possible or needed: knowing the token is what a device
// has, and deleting it only stops delivery to that device.
//
//	@Summary		Unregister a device
//	@Description	Stops push delivery to one device, identified by its FCM token. Called at sign-out; deleting a token that matches nothing still answers 204.
//	@Tags			notifications, public
//	@Security		BearerAuth
//	@Accept			json
//	@Param			body	body	notification.deviceRequest	true	"FCM registration token; platform is ignored"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		422	{object}	httpx.ErrorEnvelope	"validation_failed: fcm_token is required"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/devices [delete]
func (h *Handler) unregisterDevice(w http.ResponseWriter, r *http.Request) error {
	var req deviceRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.FCMToken) == "" {
		return httpx.ErrValidation.WithField("fcm_token", "required")
	}
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		return err
	}
	if err := h.svc.UnregisterDevice(r.Context(), strings.TrimSpace(req.FCMToken)); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}
