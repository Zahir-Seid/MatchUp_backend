package catalog

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Pagination bounds for list endpoints. The cap stops a client from asking for
// the whole table in one request.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Handler serves the public catalog endpoints.
type Handler struct {
	svc *Service
}

// NewHandler builds the catalog handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the discovery endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Method(http.MethodGet, "/fields", httpx.Handler(h.listFields))
	r.Method(http.MethodGet, "/games", httpx.Handler(h.listGames))
	r.Method(http.MethodGet, "/games/{id}", httpx.Handler(h.getGame))
}

// listGamesResponse wraps the games page.
type listGamesResponse struct {
	Games  []GameView `json:"games"`
	Limit  int32      `json:"limit"`
	Offset int32      `json:"offset"`
} //	@name	GameList

// fieldListResponse wraps the fields list in an object so the response can grow
// a sibling (a total, a cursor) without breaking clients.
type fieldListResponse struct {
	Fields []FieldView `json:"fields"`
} //	@name	FieldList

// listGames lists upcoming games matching the discovery filter (PRD FR7).
//
//	@Summary		List games
//	@Description	Upcoming games, soonest first, with the derived availability the discovery screen renders. Filter values that cannot be parsed are rejected rather than ignored.
//	@Tags			catalog, public
//	@Produce		json
//	@Param			field_id		query		string	false	"Restrict to one field (UUID)"
//	@Param			format			query		string	false	"Game format"	Enums(5_a_side, 7_a_side)
//	@Param			from			query		string	false	"Earliest start time (RFC3339)"
//	@Param			to				query		string	false	"Latest start time (RFC3339)"
//	@Param			only_available	query		bool	false	"Only games with spots left"
//	@Param			limit			query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset			query		int		false	"Rows to skip (default 0)"
//	@Success		200				{object}	catalog.listGamesResponse
//	@Failure		422				{object}	httpx.ErrorEnvelope	"A query parameter could not be parsed"
//	@Failure		429				{object}	httpx.ErrorEnvelope
//	@Failure		500				{object}	httpx.ErrorEnvelope
//	@Router			/games [get]
func (h *Handler) listGames(w http.ResponseWriter, r *http.Request) error {
	filter, err := parseGameFilter(r)
	if err != nil {
		return err
	}
	games, err := h.svc.ListGames(r.Context(), *filter)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, listGamesResponse{
		Games:  games,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
	return nil
}

// getGame returns one game with its field.
//
//	@Summary		Get one game
//	@Description	One game with its field and derived availability. An id that is not a UUID, or that matches no game, is a 404.
//	@Tags			catalog, public
//	@Produce		json
//	@Param			id	path		string	true	"Game id (UUID)"
//	@Success		200	{object}	catalog.GameView
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/games/{id} [get]
func (h *Handler) getGame(w http.ResponseWriter, r *http.Request) error {
	id, err := ParseIDParam(r, "id")
	if err != nil {
		return err
	}
	game, err := h.svc.GetGame(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, game)
	return nil
}

// listFields returns the bookable fields, for the discovery filter.
//
//	@Summary		List fields
//	@Description	The fields a player can filter the discovery screen by.
//	@Tags			catalog, public
//	@Produce		json
//	@Success		200	{object}	catalog.fieldListResponse
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/fields [get]
func (h *Handler) listFields(w http.ResponseWriter, r *http.Request) error {
	fields, err := h.svc.ListFields(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, fieldListResponse{Fields: fields})
	return nil
}

// parseGameFilter reads the discovery query string, rejecting values it cannot
// interpret rather than silently ignoring them: a filter that quietly does
// nothing would show the user games they excluded.
func parseGameFilter(r *http.Request) (*GameFilter, error) {
	q := r.URL.Query()
	v := httpx.NewValidator()
	filter := &GameFilter{}

	if raw := strings.TrimSpace(q.Get("field_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.Add("field_id", "must be a valid id")
		} else {
			filter.FieldID = &id
		}
	}
	if raw := strings.TrimSpace(q.Get("format")); raw != "" {
		format := v.OneOf("format", raw, Format5ASide, Format7ASide)
		filter.Format = &format
	}
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			v.Add("from", "must be an RFC3339 timestamp")
		} else {
			filter.From = &t
		}
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			v.Add("to", "must be an RFC3339 timestamp")
		} else {
			filter.To = &t
		}
	}
	if filter.From != nil && filter.To != nil && filter.To.Before(*filter.From) {
		v.Add("to", "must not be before from")
	}
	if raw := strings.TrimSpace(q.Get("only_available")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			v.Add("only_available", "must be true or false")
		} else {
			filter.OnlyAvailable = b
		}
	}

	limit, offset, err := ParsePagination(r)
	if err != nil {
		return nil, err
	}
	filter.Limit, filter.Offset = limit, offset

	if err := v.Err(); err != nil {
		return nil, err
	}
	return filter, nil
}

// ParsePagination reads limit/offset query parameters with shared defaults and
// bounds. Exported because every list endpoint in the system paginates the same
// way.
func ParsePagination(r *http.Request) (limit, offset int32, err error) {
	q := r.URL.Query()
	v := httpx.NewValidator()
	limit, offset = defaultPageSize, 0

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		switch {
		case convErr != nil:
			v.Add("limit", "must be a number")
		case n < 1 || n > maxPageSize:
			v.Add("limit", "must be between 1 and "+strconv.Itoa(maxPageSize))
		default:
			limit = int32(n)
		}
	}
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		switch {
		case convErr != nil:
			v.Add("offset", "must be a number")
		case n < 0:
			v.Add("offset", "must not be negative")
		default:
			offset = int32(n)
		}
	}
	if err := v.Err(); err != nil {
		return 0, 0, err
	}
	return limit, offset, nil
}

// ParseIDParam reads a UUID path parameter.
func ParseIDParam(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound.WithCause(err)
	}
	return id, nil
}
