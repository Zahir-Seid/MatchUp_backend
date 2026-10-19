package booking

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// mountWatch registers the "notify me when a spot frees up" endpoints. They
// live on the game path because that is what a watch is about, but they belong
// to this module: a watch is a promise about capacity coming back.
func (h *Handler) mountWatch(r chi.Router) {
	r.Method(http.MethodGet, "/games/{id}/watch", httpx.Handler(h.watchStatus))
	r.Method(http.MethodPost, "/games/{id}/watch", httpx.Handler(h.watch))
	r.Method(http.MethodDelete, "/games/{id}/watch", httpx.Handler(h.unwatch))
}

// WatchStatus is the caller's watch on one game.
type WatchStatus struct {
	Watching bool `json:"watching"`
} //	@name	WatchStatus

// watchStatus reports whether the caller is watching a game, so the app can
// render the "notify me" button correctly after a restart.
//
//	@Summary		Check my watch on a game
//	@Description	Whether the caller is subscribed to be told when this sold-out game gets a spot back. An id that is not a UUID, or that matches no game, is a 404.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		string	true	"Game id (UUID)"
//	@Success		200	{object}	booking.WatchStatus
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/games/{id}/watch [get]
func (h *Handler) watchStatus(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	watching, err := h.svc.Watching(r.Context(), principal.UserID, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, WatchStatus{Watching: watching})
	return nil
}

// watch subscribes the caller to the next spot freed on a sold-out game (PRD
// FR29). A game that still has spots cannot be watched, and a game that is
// cancelled or has started is refused like any booking would be.
//
//	@Summary		Watch a sold-out game
//	@Description	Subscribes the caller to a push when a spot frees up on this sold-out game. Watching a game that still has spots is refused: there is nothing to wait for. Watching twice is harmless. The alert is not a reservation, so booking stays first come, first served, and the watch is spent once the alert goes out.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Param			id	path	string	true	"Game id (UUID)"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found: no such game"
//	@Failure		409	{object}	httpx.ErrorEnvelope	"game_not_full or game_unavailable"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/games/{id}/watch [post]
func (h *Handler) watch(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Watch(r.Context(), principal.UserID, id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// unwatch stops watching a game. Unwatching a game that was never watched
// still answers 204: the caller's intent was for the alerts to stop, and they
// are stopped either way.
//
//	@Summary		Stop watching a game
//	@Description	Drops the caller's watch on a game, so no spot alert will be sent. Deleting a watch that does not exist still answers 204.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Param			id	path	string	true	"Game id (UUID)"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found: id is not a UUID"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/games/{id}/watch [delete]
func (h *Handler) unwatch(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Unwatch(r.Context(), principal.UserID, id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}
