package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler serves the dashboard API.
type Handler struct {
	svc *Service
}

// NewHandler builds the admin handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts every admin endpoint on r.
//
// Mount this behind httpx.RequireAuth and httpx.RequireRole("admin"). The role
// check is not applied per route here on purpose: a route added later would
// silently inherit no check at all, whereas a missing group-level middleware
// fails closed for everything at once and is caught by the first request.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/payments", func(r chi.Router) {
		r.Method(http.MethodGet, "/review-queue", httpx.Handler(h.listPaymentReviewQueue))
		r.Method(http.MethodPost, "/{id}/resolve", httpx.Handler(h.resolvePayment))
	})

	r.Route("/bookings", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.listBookings))
		r.Method(http.MethodGet, "/{id}", httpx.Handler(h.getBooking))
		r.Method(http.MethodPost, "/{id}/cancel", httpx.Handler(h.cancelBooking))
	})

	r.Route("/fields", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.listFields))
		r.Method(http.MethodPost, "/", httpx.Handler(h.createField))
		r.Method(http.MethodPatch, "/{id}", httpx.Handler(h.updateField))
	})

	r.Route("/games", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.listGames))
		r.Method(http.MethodPost, "/", httpx.Handler(h.createGame))
		r.Method(http.MethodGet, "/{id}", httpx.Handler(h.getGame))
		r.Method(http.MethodPatch, "/{id}", httpx.Handler(h.updateGame))
		// Capacity and cancellation are POSTs rather than PATCH fields: each is
		// a guarded statement with its own conflict cases and its own audited
		// reason, not a value being overwritten.
		r.Method(http.MethodPost, "/{id}/capacity", httpx.Handler(h.adjustGameCapacity))
		r.Method(http.MethodPost, "/{id}/cancel", httpx.Handler(h.cancelGame))
	})

	r.Route("/users", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.listUsers))
		r.Method(http.MethodGet, "/{id}", httpx.Handler(h.getUser))
		r.Method(http.MethodPatch, "/{id}/status", httpx.Handler(h.setUserStatus))
		r.Method(http.MethodPatch, "/{id}/role", httpx.Handler(h.setUserRole))
	})

	r.Route("/reports", func(r chi.Router) {
		r.Method(http.MethodGet, "/revenue", httpx.Handler(h.revenueReport))
		r.Method(http.MethodGet, "/bookings", httpx.Handler(h.bookingVolumeReport))
		r.Method(http.MethodGet, "/slots", httpx.Handler(h.popularSlotsReport))
		r.Method(http.MethodGet, "/payment-methods", httpx.Handler(h.paymentMethodsReport))
		r.Method(http.MethodGet, "/verification", httpx.Handler(h.verificationReport))
		r.Method(http.MethodGet, "/cancellations", httpx.Handler(h.cancellationReport))
	})

	r.Method(http.MethodGet, "/audit-logs", httpx.Handler(h.listAuditLogs))
}

// parsePagination and parseIDParam delegate to the catalog package so every
// list endpoint in the system shares one set of defaults and bounds, and one id
// error. They are wrappers rather than direct calls at each use site so the
// admin handlers read the same as the public ones.
func parsePagination(r *http.Request) (limit, offset int32, err error) {
	return catalog.ParsePagination(r)
}

func parseIDParam(r *http.Request, name string) (uuid.UUID, error) {
	return catalog.ParseIDParam(r, name)
}
