package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// fieldListResponse is the fields page the dashboard renders.
type fieldListResponse struct {
	Fields []AdminFieldView `json:"fields"`
} //	@name	FieldList

// listFields returns every field, active or not (PRD FR25).
//
//	@Summary		List fields
//	@Description	Every field in the catalog, active or retired: the dashboard needs to see what was deactivated in order to bring it back.
//	@Tags			fields
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	admin.fieldListResponse
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
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

// createFieldRequest is the field to add.
type createFieldRequest struct {
	Name     string  `json:"name"`
	Location string  `json:"location"`
	ImageURL *string `json:"image_url"`
} //	@name	CreateFieldRequest

// createField adds a field to the catalog (PRD FR25).
//
//	@Summary		Create a field
//	@Description	Adds a field to the catalog. The image is a link to externally hosted artwork; this service never stores files.
//	@Tags			fields
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		admin.createFieldRequest	true	"Field to create"
//	@Success		201		{object}	admin.AdminFieldView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/fields [post]
func (h *Handler) createField(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	var req createFieldRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	name := v.Required("name", req.Name)
	v.Length("name", name, 2, 120)
	location := v.Required("location", req.Location)
	v.Length("location", location, 2, 200)
	imageURL := validateImageURL(v, req.ImageURL)
	if err := v.Err(); err != nil {
		return err
	}

	field, err := h.svc.CreateField(r.Context(), principal.UserID, FieldInput{
		Name:     name,
		Location: location,
		ImageURL: imageURL,
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, field)
	return nil
}

// updateFieldRequest edits a field. Members left out are unchanged.
type updateFieldRequest struct {
	Name     *string `json:"name"`
	Location *string `json:"location"`
	ImageURL *string `json:"image_url"`
	IsActive *bool   `json:"is_active"`
} //	@name	UpdateFieldRequest

// updateField edits a field or retires it (PRD FR25).
//
//	@Summary		Update a field
//	@Description	A partial update: members left out are unchanged, and an explicit empty image_url clears the image. Deactivating a field only hides it from discovery; its games keep their own schedules.
//	@Tags			fields
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Field id (UUID)"
//	@Param			body	body		admin.updateFieldRequest	true	"Members to change"
//	@Success		200		{object}	admin.AdminFieldView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed, or no changes were given"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/fields/{id} [patch]
func (h *Handler) updateField(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req updateFieldRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	patch := FieldPatch{IsActive: req.IsActive}
	if req.Name != nil {
		name := v.Required("name", *req.Name)
		v.Length("name", name, 2, 120)
		patch.Name = &name
	}
	if req.Location != nil {
		location := v.Required("location", *req.Location)
		v.Length("location", location, 2, 200)
		patch.Location = &location
	}
	if req.ImageURL != nil {
		patch.ImageURL = validateImageURL(v, req.ImageURL)
	}
	if err := v.Err(); err != nil {
		return err
	}
	if patch.Name == nil && patch.Location == nil && patch.ImageURL == nil && patch.IsActive == nil {
		return errNoChanges
	}

	field, err := h.svc.UpdateField(r.Context(), principal.UserID, id, patch)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, field)
	return nil
}

// validateImageURL accepts an absolute http(s) URL or nothing. Images are hosted
// elsewhere -- this system stores a link, never a file -- so a relative path or
// a javascript: scheme is a mistake worth rejecting at the edge, before the
// dashboard renders it.
func validateImageURL(v *httpx.Validator, raw *string) *string {
	if raw == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		// An explicit empty string clears the image, which the nullable column
		// expresses as NULL.
		return nil
	}
	if !strings.HasPrefix(trimmed, "https://") && !strings.HasPrefix(trimmed, "http://") {
		v.Add("image_url", "must be an absolute http or https URL")
		return nil
	}
	v.Length("image_url", trimmed, 8, 500)
	return &trimmed
}

// listGames returns games matching the filter, newest first (PRD FR25).
//
//	@Summary		List games
//	@Description	Games across every field, newest first, with the operational columns the public catalog omits. The time window is half-open: from is inclusive, to exclusive.
//	@Tags			games
//	@Security		BearerAuth
//	@Produce		json
//	@Param			field_id	query		string	false	"Only games on this field (UUID)"
//	@Param			series_id	query		string	false	"Only games in this recurring series (UUID)"
//	@Param			status		query		string	false	"Only games with this status"	Enums(scheduled, cancelled, completed)
//	@Param			format		query		string	false	"Only games in this format"		Enums(5_a_side, 7_a_sided)
//	@Param			from		query		string	false	"Games starting at or after this RFC3339 timestamp"
//	@Param			to			query		string	false	"Games starting before this RFC3339 timestamp"
//	@Param			limit		query		int		false	"Page size, 1-100 (default 20)"
//	@Param			offset		query		int		false	"Rows to skip (default 0)"
//	@Success		200			{object}	admin.Page[admin.AdminGameView]
//	@Failure		401			{object}	httpx.ErrorEnvelope
//	@Failure		403			{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422			{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429			{object}	httpx.ErrorEnvelope
//	@Failure		500			{object}	httpx.ErrorEnvelope
//	@Router			/games [get]
func (h *Handler) listGames(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := parsePagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v := httpx.NewValidator()
	filter := GameFilter{Limit: limit, Offset: offset}

	if raw := strings.TrimSpace(q.Get("field_id")); raw != "" {
		if id, parseErr := uuid.Parse(raw); parseErr != nil {
			v.Add("field_id", "must be a valid id")
		} else {
			filter.FieldID = &id
		}
	}
	if raw := strings.TrimSpace(q.Get("series_id")); raw != "" {
		if id, parseErr := uuid.Parse(raw); parseErr != nil {
			v.Add("series_id", "must be a valid id")
		} else {
			filter.SeriesID = &id
		}
	}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		status := v.OneOf("status", raw,
			catalog.StatusScheduled, catalog.StatusCancelled, catalog.StatusCompleted)
		filter.Status = &status
	}
	if raw := strings.TrimSpace(q.Get("format")); raw != "" {
		format := v.OneOf("format", raw, catalog.Format5ASide, catalog.Format7ASide)
		filter.Format = &format
	}
	filter.From = parseOptionalTime(v, q.Get("from"), "from")
	filter.To = parseOptionalTime(v, q.Get("to"), "to")
	if filter.From != nil && filter.To != nil && !filter.To.After(*filter.From) {
		v.Add("to", "must be after from")
	}
	if err := v.Err(); err != nil {
		return err
	}

	page, err := h.svc.ListGames(r.Context(), filter)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, page)
	return nil
}

func parseOptionalTime(v *httpx.Validator, raw, field string) *time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		v.Add(field, "must be an RFC3339 timestamp")
		return nil
	}
	return &t
}

// getGame returns one game with its field (PRD FR25).
//
//	@Summary		Get a game
//	@Description	One game, including what the slot costs the business and how many spots are already sold.
//	@Tags			games
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		string	true	"Game id (UUID)"
//	@Success		200	{object}	admin.AdminGameView
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/games/{id} [get]
func (h *Handler) getGame(w http.ResponseWriter, r *http.Request) error {
	id, err := parseIDParam(r, "id")
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

// createGameRequest schedules one game or a weekly series.
type createGameRequest struct {
	FieldID  string `json:"field_id"`
	StartsAt string `json:"starts_at"`
	// DurationMinutes defaults to 60, matching the column default.
	DurationMinutes      int32  `json:"duration_minutes"`
	Format               string `json:"format"`
	PricePerPlayerSantim int64  `json:"price_per_player_santim"`
	CostToBusinessSantim *int64 `json:"cost_to_business_santim"`
	Capacity             int32  `json:"capacity"`
	// Occurrences > 1 creates a weekly recurring series, which is what makes
	// the slot bookable monthly.
	Occurrences int32 `json:"occurrences"`
} //	@name	CreateGameRequest

// createGameResponse is what the dashboard gets back: the games that were
// created, and the series id when they belong to a recurring one.
type createGameResponse struct {
	Games    []AdminGameView `json:"games"`
	SeriesID *uuid.UUID      `json:"series_id,omitempty"`
} //	@name	GameCreation

// createGame schedules a game, or a weekly series when occurrences > 1 (PRD FR25).
//
//	@Summary		Create a game
//	@Description	Schedules one game, or a weekly recurring series of them — the whole series in one transaction. A series is what makes a slot bookable as a monthly plan, so occurrences must be 1 or a full plan's worth.
//	@Tags			games
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		admin.createGameRequest	true	"Game or series to schedule"
//	@Success		201		{object}	admin.createGameResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/games [post]
func (h *Handler) createGame(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	var req createGameRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	in := GameInput{
		FieldID:              v.UUID("field_id", req.FieldID),
		Format:               v.OneOf("format", req.Format, catalog.Format5ASide, catalog.Format7ASide),
		PricePerPlayerSantim: req.PricePerPlayerSantim,
		CostToBusinessSantim: req.CostToBusinessSantim,
		Capacity:             req.Capacity,
		Occurrences:          req.Occurrences,
	}

	if startsAt := parseOptionalTime(v, req.StartsAt, "starts_at"); startsAt != nil {
		v.Future("starts_at", *startsAt)
		in.StartsAt = *startsAt
	} else if strings.TrimSpace(req.StartsAt) == "" {
		v.Add("starts_at", "required")
	}

	in.DurationMinutes = req.DurationMinutes
	if in.DurationMinutes == 0 {
		in.DurationMinutes = 60
	}
	v.Range("duration_minutes", int(in.DurationMinutes), minDurationMinutes, maxDurationMinutes)
	v.Range("capacity", int(in.Capacity), 1, maxCapacity)
	v.Range("price_per_player_santim", int(in.PricePerPlayerSantim), 1, maxPriceSantim)
	if in.CostToBusinessSantim != nil {
		v.Range("cost_to_business_santim", int(*in.CostToBusinessSantim), 0, maxPriceSantim*maxCapacity)
	}
	if in.Occurrences == 0 {
		in.Occurrences = 1
	}
	v.Range("occurrences", int(in.Occurrences), 1, maxSeriesOccurrences)
	// A monthly booking needs the whole plan's worth of consecutive
	// occurrences to exist, so a shorter series can never be booked monthly.
	// A run of two or three is almost certainly a mistake rather than a
	// deliberate short season.
	weeks := h.svc.MonthlyWeeks()
	v.Check(in.Occurrences == 1 || in.Occurrences >= weeks, "occurrences",
		"must be 1 for a one-off game, or at least "+strconv.Itoa(int(weeks))+" for a recurring slot")
	if err := v.Err(); err != nil {
		return err
	}

	games, err := h.svc.CreateGame(r.Context(), principal.UserID, in)
	if err != nil {
		return err
	}
	response := createGameResponse{Games: games}
	if len(games) > 0 && games[0].SeriesID != nil {
		response.SeriesID = games[0].SeriesID
	}
	httpx.JSON(w, r, http.StatusCreated, response)
	return nil
}

// updateGameRequest edits a scheduled game. Members left out are unchanged.
type updateGameRequest struct {
	StartsAt             *string `json:"starts_at"`
	DurationMinutes      *int32  `json:"duration_minutes"`
	Format               *string `json:"format"`
	PricePerPlayerSantim *int64  `json:"price_per_player_santim"`
	CostToBusinessSantim *int64  `json:"cost_to_business_santim"`
} //	@name	UpdateGameRequest

// updateGame edits a scheduled game (PRD FR25).
//
//	@Summary		Update a game
//	@Description	A partial update of a scheduled game. A price change applies only to bookings made after it: every booking keeps the price it was sold at. Capacity is not editable here because moving it has to move the free spots with it.
//	@Tags			games
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"Game id (UUID)"
//	@Param			body	body		admin.updateGameRequest	true	"Members to change"
//	@Success		200		{object}	admin.AdminGameView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"game_unavailable: only a scheduled game can be edited"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed, or no changes were given"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/games/{id} [patch]
func (h *Handler) updateGame(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req updateGameRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	patch := GamePatch{
		DurationMinutes:      req.DurationMinutes,
		PricePerPlayerSantim: req.PricePerPlayerSantim,
		CostToBusinessSantim: req.CostToBusinessSantim,
	}
	if req.StartsAt != nil {
		if t := parseOptionalTime(v, *req.StartsAt, "starts_at"); t != nil {
			v.Future("starts_at", *t)
			patch.StartsAt = t
		}
	}
	if req.Format != nil {
		format := v.OneOf("format", *req.Format, catalog.Format5ASide, catalog.Format7ASide)
		patch.Format = &format
	}
	if req.DurationMinutes != nil {
		v.Range("duration_minutes", int(*req.DurationMinutes), minDurationMinutes, maxDurationMinutes)
	}
	if req.PricePerPlayerSantim != nil {
		v.Range("price_per_player_santim", int(*req.PricePerPlayerSantim), 1, maxPriceSantim)
	}
	if req.CostToBusinessSantim != nil {
		v.Range("cost_to_business_santim", int(*req.CostToBusinessSantim), 0, maxPriceSantim*maxCapacity)
	}
	if err := v.Err(); err != nil {
		return err
	}
	if patch.StartsAt == nil && patch.DurationMinutes == nil && patch.Format == nil &&
		patch.PricePerPlayerSantim == nil && patch.CostToBusinessSantim == nil {
		return errNoChanges
	}

	game, err := h.svc.UpdateGame(r.Context(), principal.UserID, id, patch)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, game)
	return nil
}

// capacityRequest adjusts how many spots a game sells.
type capacityRequest struct {
	// Delta is signed: positive adds bookable spots, negative removes free
	// ones. An absolute target would be ambiguous about what to do with the
	// spots already sold.
	Delta  int32  `json:"delta"`
	Reason string `json:"reason"`
} //	@name	AdjustCapacityRequest

// adjustGameCapacity changes a scheduled game's capacity (PRD FR25).
//
//	@Summary		Adjust game capacity
//	@Description	Raises or lowers a scheduled game's capacity by a signed delta, moving the free spots with it. Raising credits the new seats so they become bookable; lowering can only consume spots that are still free. The optional reason is recorded in the audit log.
//	@Tags			games
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"Game id (UUID)"
//	@Param			body	body		admin.capacityRequest	true	"Capacity change"
//	@Success		200		{object}	admin.AdminGameView
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"game_unavailable or insufficient_capacity: not enough free spots"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/games/{id}/capacity [post]
func (h *Handler) adjustGameCapacity(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req capacityRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	v.Check(req.Delta != 0, "delta", "must not be zero")
	v.Check(req.Delta >= -maxCapacityDelta && req.Delta <= maxCapacityDelta,
		"delta", "must be between -"+strconv.Itoa(maxCapacityDelta)+" and "+strconv.Itoa(maxCapacityDelta))
	reason := strings.TrimSpace(req.Reason)
	if reason != "" {
		v.Length("reason", reason, 3, 500)
	}
	if err := v.Err(); err != nil {
		return err
	}

	game, err := h.svc.AdjustCapacity(r.Context(), principal.UserID, id, req.Delta, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, game)
	return nil
}

// cancelGameRequest carries the reason a game is being cancelled.
type cancelGameRequest struct {
	Reason string `json:"reason"`
} //	@name	CancelGameRequest

// cancelGame cancels a scheduled game and tells affected customers (PRD FR25).
//
//	@Summary		Cancel a game
//	@Description	Cancels a scheduled game and notifies everyone holding a live booking on it, with the reason passed through verbatim. Existing bookings are deliberately left alone — a monthly plan spans several games — and the response reports the blast radius so each booking can be settled deliberately.
//	@Tags			games
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"Game id (UUID)"
//	@Param			body	body		admin.cancelGameRequest	true	"Cancellation reason"
//	@Success		200		{object}	admin.GameCancellation
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed JSON body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404		{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"game_unavailable: only a scheduled game can be cancelled"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/games/{id}/cancel [post]
func (h *Handler) cancelGame(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	var req cancelGameRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		return err
	}

	v := httpx.NewValidator()
	// The reason reaches every affected customer, so it is required and is
	// written for them rather than for the internal log.
	reason := v.Required("reason", req.Reason)
	v.Length("reason", reason, 3, 300)
	if err := v.Err(); err != nil {
		return err
	}

	out, err := h.svc.CancelGame(r.Context(), principal.UserID, id, reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}
