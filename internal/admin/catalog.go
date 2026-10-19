package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
)

// Bounds on what an admin may schedule. These are sanity limits, not product
// rules: they stop a typo from creating a ten-year recurrence or a game priced
// in the millions, either of which is far more likely than the real thing.
const (
	maxSeriesOccurrences = 52
	minDurationMinutes   = 15
	maxDurationMinutes   = 300
	maxCapacity          = 60
	maxPriceSantim       = 10_000_00 // 10,000 ETB per player
	maxCapacityDelta     = 60
)

// AdminFieldView is a field with the operational columns the public view omits.
type AdminFieldView struct {
	catalog.FieldView
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
} //	@name	AdminFieldView

func toAdminFieldView(f db.Field) AdminFieldView {
	return AdminFieldView{
		FieldView: catalog.ToFieldView(f),
		IsActive:  f.IsActive,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}

// ListFields returns every field, active or not: an admin managing the catalog
// needs to see what was retired in order to bring it back.
func (s *Service) ListFields(ctx context.Context) ([]AdminFieldView, error) {
	rows, err := s.store.ListAllFields(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all fields: %w", err)
	}
	views := make([]AdminFieldView, 0, len(rows))
	for _, f := range rows {
		views = append(views, toAdminFieldView(f))
	}
	return views, nil
}

// FieldInput creates a field.
type FieldInput struct {
	Name     string
	Location string
	ImageURL *string
}

// CreateField adds a field to the catalog (PRD FR25).
func (s *Service) CreateField(ctx context.Context, actor uuid.UUID, in FieldInput) (*AdminFieldView, error) {
	var out AdminFieldView
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		field, err := q.CreateField(ctx, db.CreateFieldParams{
			Name:     in.Name,
			Location: in.Location,
			ImageUrl: in.ImageURL,
		})
		if err != nil {
			return fmt.Errorf("create field: %w", err)
		}
		out = toAdminFieldView(field)
		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionFieldCreated,
			TargetType: audit.TargetField,
			TargetID:   field.ID.String(),
			Metadata: map[string]any{
				"name":     field.Name,
				"location": field.Location,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FieldPatch updates a field. A nil member means "leave alone", which is what
// the COALESCE in the query expresses.
type FieldPatch struct {
	Name     *string
	Location *string
	ImageURL *string
	IsActive *bool
}

// UpdateField edits a field. Deactivating one does not touch its games: the
// flag only hides the field from discovery, and cancelling scheduled games is a
// separate, deliberate act with its own notifications.
func (s *Service) UpdateField(ctx context.Context, actor, id uuid.UUID, patch FieldPatch) (*AdminFieldView, error) {
	var out AdminFieldView
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetField(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get field: %w", err)
		}

		field, err := q.UpdateField(ctx, db.UpdateFieldParams{
			ID:       id,
			Name:     patch.Name,
			Location: patch.Location,
			ImageUrl: patch.ImageURL,
			IsActive: patch.IsActive,
		})
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("update field: %w", err)
		}
		out = toAdminFieldView(field)

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionFieldUpdated,
			TargetType: audit.TargetField,
			TargetID:   field.ID.String(),
			Metadata: map[string]any{
				"changed": changedFields(map[string][2]any{
					"name":      {before.Name, field.Name},
					"location":  {before.Location, field.Location},
					"image_url": {derefText(before.ImageUrl), derefText(field.ImageUrl)},
					"is_active": {before.IsActive, field.IsActive},
				}),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// changedFields keeps an audit entry to what actually moved. A diff of every
// column on every edit buries the one field that changed.
func changedFields(candidates map[string][2]any) map[string]any {
	changed := make(map[string]any, len(candidates))
	for name, pair := range candidates {
		if pair[0] != pair[1] {
			changed[name] = map[string]any{"from": pair[0], "to": pair[1]}
		}
	}
	return changed
}

func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GameFilter narrows the admin game list.
type GameFilter struct {
	FieldID  *uuid.UUID
	Status   *string
	Format   *string
	SeriesID *uuid.UUID
	From     *time.Time
	To       *time.Time
	Limit    int32
	Offset   int32
}

// AdminGameView is a game with the columns the public view withholds: what the
// slot costs the business, and how many spots are actually sold.
type AdminGameView struct {
	catalog.GameView
	SpotsTaken           int32     `json:"spots_taken"`
	CostToBusiness       *string   `json:"cost_to_business,omitempty"`
	CostToBusinessSantim *int64    `json:"cost_to_business_santim,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
} //	@name	AdminGameView

func toAdminGameView(g db.Game, f db.Field) AdminGameView {
	view := AdminGameView{
		GameView:             catalog.ToGameView(g, f),
		SpotsTaken:           g.Capacity - g.SpotsRemaining,
		CostToBusinessSantim: g.CostToBusinessSantim,
		CreatedAt:            g.CreatedAt,
		UpdatedAt:            g.UpdatedAt,
	}
	if g.CostToBusinessSantim != nil {
		formatted := money.Santim(*g.CostToBusinessSantim).Format()
		view.CostToBusiness = &formatted
	}
	return view
}

// ListGames returns games matching the filter, newest first.
func (s *Service) ListGames(ctx context.Context, f GameFilter) (Page[AdminGameView], error) {
	rows, err := s.store.ListGamesForAdmin(ctx, db.ListGamesForAdminParams{
		Limit:    f.Limit,
		Offset:   f.Offset,
		FieldID:  f.FieldID,
		Status:   f.Status,
		Format:   f.Format,
		SeriesID: f.SeriesID,
		FromTime: f.From,
		ToTime:   f.To,
	})
	if err != nil {
		return Page[AdminGameView]{}, fmt.Errorf("list games for admin: %w", err)
	}
	total, err := s.store.CountGamesForAdmin(ctx, db.CountGamesForAdminParams{
		FieldID:  f.FieldID,
		Status:   f.Status,
		Format:   f.Format,
		SeriesID: f.SeriesID,
		FromTime: f.From,
		ToTime:   f.To,
	})
	if err != nil {
		return Page[AdminGameView]{}, fmt.Errorf("count games for admin: %w", err)
	}

	items := make([]AdminGameView, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAdminGameView(row.Game, row.Field))
	}
	return newPage(items, total, f.Limit, f.Offset), nil
}

// GetGame returns one game with its field.
func (s *Service) GetGame(ctx context.Context, id uuid.UUID) (*AdminGameView, error) {
	row, err := s.store.GetGameWithField(ctx, id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get game: %w", err)
	}
	view := toAdminGameView(row.Game, row.Field)
	return &view, nil
}

// GameInput creates one game or one recurring series.
type GameInput struct {
	FieldID              uuid.UUID
	StartsAt             time.Time
	DurationMinutes      int32
	Format               string
	PricePerPlayerSantim int64
	CostToBusinessSantim *int64
	Capacity             int32
	// Occurrences is 1 for a one-off game. Anything higher creates that many
	// weekly games sharing one series_id, which is the only thing that makes a
	// slot "recurring" and therefore bookable monthly.
	Occurrences int32
}

// CreateGame adds a game, or a weekly series when Occurrences > 1 (PRD FR25).
//
// The whole series is one transaction. A partially created recurrence is worse
// than none: a monthly booking walks the series forward looking for four
// occurrences, and would silently fall short.
func (s *Service) CreateGame(ctx context.Context, actor uuid.UUID, in GameInput) ([]AdminGameView, error) {
	occurrences := in.Occurrences
	if occurrences <= 0 {
		occurrences = 1
	}

	var seriesID *uuid.UUID
	if occurrences > 1 {
		id := uuid.New()
		seriesID = &id
	}

	created := make([]db.Game, 0, occurrences)
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		field, err := q.GetField(ctx, in.FieldID)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrValidation.WithField("field_id", "no such field").WithCause(err)
			}
			return fmt.Errorf("get field: %w", err)
		}
		if !field.IsActive {
			// Scheduling onto a retired field would put games on the schedule
			// that discovery never shows, so nobody could book them.
			return httpx.ErrValidation.WithField("field_id", "that field is not active")
		}

		created = created[:0]
		for i := int32(0); i < occurrences; i++ {
			// Weekly recurrence is done by adding 7 days rather than 168 hours
			// so a DST shift would move the wall-clock time consistently.
			// Ethiopia does not observe DST, so today the two agree; the
			// calendar form stays correct if that ever changes.
			startsAt := in.StartsAt.AddDate(0, 0, int(7*i))
			game, err := q.CreateGame(ctx, db.CreateGameParams{
				FieldID:              in.FieldID,
				SeriesID:             seriesID,
				StartsAt:             startsAt,
				DurationMinutes:      in.DurationMinutes,
				Format:               in.Format,
				PricePerPlayerSantim: in.PricePerPlayerSantim,
				CostToBusinessSantim: in.CostToBusinessSantim,
				Capacity:             in.Capacity,
			})
			if err != nil {
				return fmt.Errorf("create game %d of %d: %w", i+1, occurrences, err)
			}
			created = append(created, game)

			if err := s.record(ctx, q, audit.Entry{
				ActorID:    &actor,
				Action:     audit.ActionGameCreated,
				TargetType: audit.TargetGame,
				TargetID:   game.ID.String(),
				Metadata: map[string]any{
					"field_id":                field.ID.String(),
					"field_name":              field.Name,
					"starts_at":               startsAt.UTC().Format(time.RFC3339),
					"format":                  game.Format,
					"capacity":                game.Capacity,
					"price_per_player_santim": game.PricePerPlayerSantim,
					"series_id":               seriesText(seriesID),
					"week_index":              i,
				},
			}); err != nil {
				return err
			}
		}

		if seriesID == nil {
			return nil
		}
		// The series gets its own entry alongside the per-game ones, so the
		// trail records the single act the admin performed as well as its
		// effects.
		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionSeriesCreated,
			TargetType: audit.TargetSeries,
			TargetID:   seriesID.String(),
			Metadata: map[string]any{
				"field_id":    field.ID.String(),
				"field_name":  field.Name,
				"first_start": in.StartsAt.UTC().Format(time.RFC3339),
				"last_start":  in.StartsAt.AddDate(0, 0, int(7*(occurrences-1))).UTC().Format(time.RFC3339),
				"occurrences": occurrences,
				"format":      in.Format,
				"capacity":    in.Capacity,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	// The field is the same for every game and was already validated above.
	field, err := s.store.GetField(ctx, in.FieldID)
	if err != nil {
		return nil, fmt.Errorf("get field for response: %w", err)
	}
	views := make([]AdminGameView, 0, len(created))
	for _, g := range created {
		views = append(views, toAdminGameView(g, field))
	}
	return views, nil
}

func seriesText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// GamePatch edits a scheduled game. Capacity is deliberately absent: changing
// it has to move spots_remaining by the same delta, which is a separate
// conditional statement rather than a COALESCE.
type GamePatch struct {
	StartsAt             *time.Time
	DurationMinutes      *int32
	Format               *string
	PricePerPlayerSantim *int64
	CostToBusinessSantim *int64
}

// UpdateGame edits a scheduled game (PRD FR25).
//
// A price change applies only to bookings made after it: every booking stores
// the price it was sold at, so nothing already paid for is repriced.
func (s *Service) UpdateGame(ctx context.Context, actor, id uuid.UUID, patch GamePatch) (*AdminGameView, error) {
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetGame(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get game: %w", err)
		}

		game, err := q.UpdateGame(ctx, db.UpdateGameParams{
			ID:                   id,
			StartsAt:             patch.StartsAt,
			DurationMinutes:      patch.DurationMinutes,
			Format:               patch.Format,
			PricePerPlayerSantim: patch.PricePerPlayerSantim,
			CostToBusinessSantim: patch.CostToBusinessSantim,
		})
		if err != nil {
			if db.IsNoRows(err) {
				// The query guards on status = 'scheduled'.
				return httpx.NewError(http.StatusConflict, httpx.CodeGameUnavailable,
					"Only a scheduled game can be edited.").WithCause(err)
			}
			return fmt.Errorf("update game: %w", err)
		}
		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionGameUpdated,
			TargetType: audit.TargetGame,
			TargetID:   game.ID.String(),
			Metadata: map[string]any{
				"spots_taken": game.Capacity - game.SpotsRemaining,
				"changed": changedFields(map[string][2]any{
					"starts_at":               {before.StartsAt.UTC().Format(time.RFC3339), game.StartsAt.UTC().Format(time.RFC3339)},
					"duration_minutes":        {before.DurationMinutes, game.DurationMinutes},
					"format":                  {before.Format, game.Format},
					"price_per_player_santim": {before.PricePerPlayerSantim, game.PricePerPlayerSantim},
					"cost_to_business_santim": {derefInt(before.CostToBusinessSantim), derefInt(game.CostToBusinessSantim)},
				}),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	// The patch response needs the field, which the update query does not
	// return; one extra read keeps the shape identical to the list endpoint.
	return s.GetGame(ctx, id)
}

func derefInt(v *int64) int64 {
	if v == nil {
		return -1
	}
	return *v
}

// AdjustCapacity changes a game's capacity, moving spots_remaining by the same
// delta (PRD FR25).
//
// Raising capacity credits the new seats so they become bookable. Lowering it
// can only consume seats that are still free -- the guarded query returns no row
// when the reduction would cut into sold spots, which would otherwise oversell
// the game.
func (s *Service) AdjustCapacity(ctx context.Context, actor, id uuid.UUID, delta int32, reason string) (*AdminGameView, error) {
	if delta == 0 {
		return nil, httpx.ErrValidation.WithField("delta", "must not be zero")
	}

	err := s.store.InTx(ctx, func(q *db.Queries) error {
		before, err := q.GetGame(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound.WithCause(err)
			}
			return fmt.Errorf("get game: %w", err)
		}

		var game db.Game
		if delta > 0 {
			game, err = q.IncreaseGameCapacity(ctx, db.IncreaseGameCapacityParams{ID: id, Delta: delta})
		} else {
			game, err = q.DecreaseGameCapacity(ctx, db.DecreaseGameCapacityParams{ID: id, Delta: -delta})
		}
		if err != nil {
			if db.IsNoRows(err) {
				if before.Status != catalog.StatusScheduled {
					return httpx.NewError(http.StatusConflict, httpx.CodeGameUnavailable,
						"Only a scheduled game can have its capacity changed.").WithCause(err)
				}
				return httpx.NewError(http.StatusConflict, httpx.CodeInsufficientCapacity,
					fmt.Sprintf("Only %d of the %d spots are still free, so capacity cannot be reduced by that much.",
						before.SpotsRemaining, before.Capacity)).WithCause(err)
			}
			return fmt.Errorf("adjust game capacity: %w", err)
		}

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionGameCapacityChanged,
			TargetType: audit.TargetGame,
			TargetID:   game.ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"delta":                delta,
				"capacity_from":        before.Capacity,
				"capacity_to":          game.Capacity,
				"spots_remaining_from": before.SpotsRemaining,
				"spots_remaining_to":   game.SpotsRemaining,
				"spots_taken":          game.Capacity - game.SpotsRemaining,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetGame(ctx, id)
}

// GameCancellation reports what cancelling a game touched.
type GameCancellation struct {
	GameID uuid.UUID `json:"game_id"`
	Status string    `json:"status"`
	// AffectedBookings is how many live bookings covered this game. Their
	// owners have been notified; the bookings themselves are untouched.
	AffectedBookings int `json:"affected_bookings"`
	// AffectedSpots is the number of player spots across those bookings.
	AffectedSpots int32 `json:"affected_spots"`
	// PaidBookings is how many of them were already paid for, which is the
	// number a person now has to settle a refund with.
	PaidBookings int `json:"paid_bookings"`
} //	@name	GameCancellation

// CancelGame cancels a scheduled game and tells everyone holding a booking on
// it (PRD FR25).
//
// The bookings are deliberately not cancelled. A monthly booking covers four
// games, and cancelling the whole thing because one week fell through is not a
// decision this endpoint gets to make; neither is the refund, which the PRD
// leaves to the business. The response reports the blast radius so the admin
// can act on each booking deliberately, through the booking endpoint that
// records a reason.
func (s *Service) CancelGame(ctx context.Context, actor, id uuid.UUID, reason string) (*GameCancellation, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, httpx.ErrValidation.WithField("reason", "required")
	}

	var out GameCancellation
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		// Read the affected bookings before the cancellation: the listing
		// filters on the booking's status, not the game's, so the order does
		// not change the result -- but reading first keeps the two statements
		// describing the same instant even if that ever changes.
		affected, err := q.ListLiveBookingsForGame(ctx, id)
		if err != nil {
			return fmt.Errorf("list live bookings for game: %w", err)
		}

		game, err := q.CancelGame(ctx, id)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NewError(http.StatusConflict, httpx.CodeGameUnavailable,
					"Only a scheduled game can be cancelled.").WithCause(err)
			}
			return fmt.Errorf("cancel game: %w", err)
		}

		out = GameCancellation{
			GameID:           game.ID,
			Status:           game.Status,
			AffectedBookings: len(affected),
		}
		for _, b := range affected {
			out.AffectedSpots += b.SpotCount
			if b.ConfirmedAt != nil {
				out.PaidBookings++
			}
			if err := s.notify(ctx, q, b.UserID, notification.TypeGameCancelled, map[string]any{
				"booking_id":     b.ID.String(),
				"reference_code": b.ReferenceCode,
				"game_id":        game.ID.String(),
				"starts_at":      game.StartsAt.UTC().Format(time.RFC3339),
				"reason":         reason,
			}); err != nil {
				return err
			}
		}

		return s.record(ctx, q, audit.Entry{
			ActorID:    &actor,
			Action:     audit.ActionGameCancelled,
			TargetType: audit.TargetGame,
			TargetID:   game.ID.String(),
			Reason:     reason,
			Metadata: map[string]any{
				"starts_at":         game.StartsAt.UTC().Format(time.RFC3339),
				"series_id":         seriesText(game.SeriesID),
				"spots_taken":       game.Capacity - game.SpotsRemaining,
				"affected_bookings": out.AffectedBookings,
				"affected_spots":    out.AffectedSpots,
				"paid_bookings":     out.PaidBookings,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// errNoChanges rejects an empty patch body. A PATCH that changes nothing is a
// client bug, and answering 200 to it hides the bug.
var errNoChanges = httpx.ErrValidation.WithField("body", "no changes were given")
