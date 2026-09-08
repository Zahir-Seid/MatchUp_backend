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

type listGamesResponse struct {
	Games  []GameView `json:"games"`
	Limit  int32      `json:"limit"`
	Offset int32      `json:"offset"`
}

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

func (h *Handler) listFields(w http.ResponseWriter, r *http.Request) error {
	fields, err := h.svc.ListFields(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"fields": fields})
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
