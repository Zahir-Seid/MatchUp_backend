package booking

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Handler exposes the booking endpoints. Every route here is owner-scoped: the
// caller's own id comes from the access token, never from the request body.
type Handler struct {
	svc *Service
	// hold throttles the endpoints that reserve capacity. A 5-minute hold is
	// free to create, so without a limit one account could park every spot on a
	// field and release nothing.
	hold func(http.Handler) http.Handler
}

// NewHandler builds the booking handler. holdLimit may be nil.
func NewHandler(svc *Service, holdLimit func(http.Handler) http.Handler) *Handler {
	if holdLimit == nil {
		holdLimit = func(next http.Handler) http.Handler { return next }
	}
	return &Handler{svc: svc, hold: holdLimit}
}

// Routes mounts the booking endpoints. Mount behind httpx.RequireAuth.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/bookings", func(r chi.Router) {
		r.Method(http.MethodGet, "/", httpx.Handler(h.list))
		r.Method(http.MethodPost, "/quote", httpx.Handler(h.quote))
		r.Group(func(r chi.Router) {
			r.Use(h.hold)
			r.Method(http.MethodPost, "/", httpx.Handler(h.create))
		})
		r.Method(http.MethodGet, "/{id}", httpx.Handler(h.get))
	})
	h.mountWatch(r)
}

// quoteRequest is the selection being priced.
type quoteRequest struct {
	GameID      string `json:"game_id"`
	BookingType string `json:"booking_type"`
	SpotCount   int32  `json:"spot_count"`
} //	@name	QuoteRequest

// quote prices a booking without reserving anything (PRD FR11). It is a POST
// because the body carries the whole selection, not because it changes state.
//
//	@Summary		Price a booking
//	@Description	The full price breakdown for a selection — per-player price, subtotal, monthly discount and the games it would cover — without reserving anything.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		booking.quoteRequest	true	"Selection to price"
//	@Success		200		{object}	booking.Quote
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found: no such game"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"game_unavailable or series_incomplete"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings/quote [post]
func (h *Handler) quote(w http.ResponseWriter, r *http.Request) error {
	var req quoteRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	gameID := v.UUID("game_id", req.GameID)
	bookingType := v.OneOf("booking_type", req.BookingType, TypeSingle, TypeMonthly4W)
	v.Range("spot_count", int(req.SpotCount), 1, maxSpotsPerBooking)
	if err := v.Err(); err != nil {
		return err
	}

	quote, err := h.svc.Quote(r.Context(), gameID, bookingType, req.SpotCount)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, quote)
	return nil
}

// participantRequest names one reserved spot. slot_index is optional: callers
// that just list names in order get sequential slots.
type participantRequest struct {
	SlotIndex int32  `json:"slot_index,omitempty"`
	FullName  string `json:"full_name"`
} //	@name	ParticipantInput

// createRequest reserves spots and opens the payment hold.
type createRequest struct {
	GameID      string `json:"game_id"`
	BookingType string `json:"booking_type"`
	SpotCount   int32  `json:"spot_count"`
	// Participants names some or all of the reserved spots (PRD FR10). Spots
	// left unnamed are still reserved.
	Participants []participantRequest `json:"participants,omitempty"`
} //	@name	CreateBookingRequest

// create reserves spots and starts the payment hold (PRD FR9-FR10). It is rate
// limited: a hold is free to create, so without a limit one account could park
// every spot on a field and release nothing.
//
//	@Summary		Create a booking
//	@Description	Reserves the requested spots and returns the booking with its hold countdown. A monthly plan covers the next weekly occurrences of the same slot. Spots are released automatically when the hold lapses unpaid.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		booking.createRequest	true	"Reservation to make"
//	@Success		201		{object}	booking.View
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found: no such game"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"insufficient_capacity, game_unavailable or series_incomplete"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	gameID := v.UUID("game_id", req.GameID)
	bookingType := v.OneOf("booking_type", req.BookingType, TypeSingle, TypeMonthly4W)
	v.Range("spot_count", int(req.SpotCount), 1, maxSpotsPerBooking)

	participants := make([]Participant, 0, len(req.Participants))
	for i, p := range req.Participants {
		name := strings.TrimSpace(p.FullName)
		if name == "" {
			v.Add("participants", "each named spot needs a full name")
			continue
		}
		if len([]rune(name)) > 120 {
			v.Add("participants", "a name may be at most 120 characters")
			continue
		}
		slot := p.SlotIndex
		if slot == 0 {
			// Callers that just list names in order get sequential slots.
			slot = int32(i + 1)
		}
		participants = append(participants, Participant{SlotIndex: slot, FullName: name})
	}
	if err := v.Err(); err != nil {
		return err
	}

	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	view, err := h.svc.Create(r.Context(), CreateInput{
		UserID:       principal.UserID,
		GameID:       gameID,
		BookingType:  bookingType,
		SpotCount:    req.SpotCount,
		Participants: participants,
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, view)
	return nil
}

// listResponse wraps the caller's bookings page.
type listResponse struct {
	Bookings []View `json:"bookings"`
	Limit    int32  `json:"limit"`
	Offset   int32  `json:"offset"`
} //	@name	BookingList

// list returns the caller's own bookings, newest first (PRD FR20).
//
//	@Summary		List my bookings
//	@Description	One page of the caller's bookings, newest first, optionally filtered to a single status.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Produce		json
//	@Param			status	query		string	false	"Filter to one status"	Enums(pending_payment, verifying, confirmed, pending_review, expired, cancelled)
//	@Param			limit	query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset	query		int		false	"Rows to skip (default 0)"
//	@Success		200		{object}	booking.listResponse
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed: unknown status or bad paging"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/bookings [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := catalog.ParsePagination(r)
	if err != nil {
		return err
	}

	var status *string
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		v := httpx.NewValidator()
		// An unknown status is rejected rather than ignored: silently
		// returning everything would look like a filter that did nothing.
		valid := v.OneOf("status", raw, StatusPendingPayment, StatusVerifying,
			StatusConfirmed, StatusPendingReview, StatusExpired, StatusCancelled)
		if err := v.Err(); err != nil {
			return err
		}
		status = &valid
	}

	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	bookings, err := h.svc.List(r.Context(), principal.UserID, status, limit, offset)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, listResponse{Bookings: bookings, Limit: limit, Offset: offset})
	return nil
}

// get returns one of the caller's own bookings in full.
//
//	@Summary		Get a booking
//	@Description	The booking's games, participants and payment attempts. Another account's booking is simply not found.
//	@Tags			bookings, public
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		string	true	"Booking id (UUID)"
//	@Success		200	{object}	booking.View
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/bookings/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := catalog.ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	view, err := h.svc.Get(r.Context(), principal.UserID, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, view)
	return nil
}
