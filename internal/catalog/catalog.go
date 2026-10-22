// Package catalog serves the read side of fields and games: what the app shows
// on the discovery screen, and the game detail behind a booking.
package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
	"github.com/Zahir-Seid/MatchUp_backend/internal/money"
)

// Game formats offered.
const (
	Format5ASide = "5_a_side"
	Format7ASide = "7_a_side"
)

// Game statuses.
const (
	StatusScheduled = "scheduled"
	StatusCancelled = "cancelled"
	StatusCompleted = "completed"
)

// Service reads the catalog.
type Service struct {
	store *db.Store
	media *media.Service
}

// NewService builds the catalog service. The media service may be omitted in
// tests that do not render field artwork.
func NewService(store *db.Store, mediaSvc ...*media.Service) *Service {
	s := &Service{store: store}
	if len(mediaSvc) > 0 {
		s.media = mediaSvc[0]
	}
	return s
}

// GameFilter is the discovery query (PRD FR7).
type GameFilter struct {
	FieldID       *uuid.UUID
	Format        *string
	From          *time.Time
	To            *time.Time
	OnlyAvailable bool
	Limit         int32
	Offset        int32
}

// GameView is one game as the app renders it, with its field inlined and the
// derived availability the UI needs (FR6).
type GameView struct {
	ID              uuid.UUID  `json:"id"`
	SeriesID        *uuid.UUID `json:"series_id,omitempty"`
	StartsAt        time.Time  `json:"starts_at"`
	EndsAt          time.Time  `json:"ends_at"`
	DurationMinutes int32      `json:"duration_minutes"`
	Format          string     `json:"format"`
	PricePerPlayer  string     `json:"price_per_player"`
	// Santim values travel alongside the formatted strings so the client can
	// compute totals without parsing display text.
	PricePerPlayerSantim int64     `json:"price_per_player_santim"`
	Capacity             int32     `json:"capacity"`
	SpotsRemaining       int32     `json:"spots_remaining"`
	IsFull               bool      `json:"is_full"`
	Status               string    `json:"status"`
	IsRecurring          bool      `json:"is_recurring"`
	Field                FieldView `json:"field"`
} //	@name	Game

// FieldView is a field as the app renders it.
type FieldView struct {
	ID       uuid.UUID     `json:"id"`
	Name     string        `json:"name"`
	Location string        `json:"location"`
	ImageURL *string       `json:"image_url,omitempty"`
	Images   []media.Image `json:"images"`
} //	@name	Field

// ToGameView projects a game row and its field into the API shape. The
// optional gallery is the field's attached images, already in display order.
func ToGameView(game db.Game, field db.Field, galleries ...[]media.Image) GameView {
	var images []media.Image
	if len(galleries) > 0 {
		images = galleries[0]
	}
	return GameView{
		ID:                   game.ID,
		SeriesID:             game.SeriesID,
		StartsAt:             game.StartsAt,
		EndsAt:               game.StartsAt.Add(time.Duration(game.DurationMinutes) * time.Minute),
		DurationMinutes:      game.DurationMinutes,
		Format:               game.Format,
		PricePerPlayer:       money.Santim(game.PricePerPlayerSantim).Format(),
		PricePerPlayerSantim: game.PricePerPlayerSantim,
		Capacity:             game.Capacity,
		SpotsRemaining:       game.SpotsRemaining,
		IsFull:               game.SpotsRemaining <= 0,
		Status:               game.Status,
		IsRecurring:          game.SeriesID != nil,
		Field:                ToFieldView(field, images),
	}
}

// ToFieldView projects a field row into the API shape. The optional gallery is
// the field's attached images, already in display order.
func ToFieldView(field db.Field, galleries ...[]media.Image) FieldView {
	var images []media.Image
	if len(galleries) > 0 {
		images = galleries[0]
	}
	if images == nil {
		images = []media.Image{}
	}
	return FieldView{
		ID:       field.ID,
		Name:     field.Name,
		Location: field.Location,
		ImageURL: media.PrimaryImageURL(images),
		Images:   images,
	}
}

// ListGames returns upcoming games matching the filter.
func (s *Service) ListGames(ctx context.Context, f GameFilter) ([]GameView, error) {
	rows, err := s.store.ListUpcomingGames(ctx, db.ListUpcomingGamesParams{
		Limit:         f.Limit,
		Offset:        f.Offset,
		FieldID:       f.FieldID,
		Format:        f.Format,
		FromTime:      f.From,
		ToTime:        f.To,
		OnlyAvailable: f.OnlyAvailable,
	})
	if err != nil {
		return nil, fmt.Errorf("list upcoming games: %w", err)
	}

	fieldIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		fieldIDs = append(fieldIDs, row.Field.ID)
	}
	galleries, err := s.galleries(ctx, fieldIDs)
	if err != nil {
		return nil, err
	}

	views := make([]GameView, 0, len(rows))
	for _, row := range rows {
		views = append(views, ToGameView(row.Game, row.Field, galleries[row.Field.ID]))
	}
	return views, nil
}

// GetGame returns one game with its field.
func (s *Service) GetGame(ctx context.Context, id uuid.UUID) (*GameView, error) {
	row, err := s.store.GetGameWithField(ctx, id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get game: %w", err)
	}
	galleries, err := s.galleries(ctx, []uuid.UUID{row.Field.ID})
	if err != nil {
		return nil, err
	}
	view := ToGameView(row.Game, row.Field, galleries[row.Field.ID])
	return &view, nil
}

// ListFields returns the bookable fields, for the discovery filter.
func (s *Service) ListFields(ctx context.Context) ([]FieldView, error) {
	rows, err := s.store.ListActiveFields(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active fields: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, field := range rows {
		ids = append(ids, field.ID)
	}
	galleries, err := s.galleries(ctx, ids)
	if err != nil {
		return nil, err
	}
	views := make([]FieldView, 0, len(rows))
	for _, field := range rows {
		views = append(views, ToFieldView(field, galleries[field.ID]))
	}
	return views, nil
}

func (s *Service) galleries(ctx context.Context, fieldIDs []uuid.UUID) (map[uuid.UUID][]media.Image, error) {
	if s.media == nil || len(fieldIDs) == 0 {
		return map[uuid.UUID][]media.Image{}, nil
	}
	return s.media.FieldImageMap(ctx, fieldIDs)
}

// GetImage returns a public display image.
func (s *Service) GetImage(ctx context.Context, fieldID, imageID uuid.UUID) (*media.ImageBytes, error) {
	if s.media == nil {
		return nil, httpx.ErrNotFound
	}
	return s.media.GetPublicImage(ctx, fieldID, imageID)
}

// GetThumbnail returns a public thumbnail.
func (s *Service) GetThumbnail(ctx context.Context, fieldID, imageID uuid.UUID) (*media.ImageBytes, error) {
	if s.media == nil {
		return nil, httpx.ErrNotFound
	}
	return s.media.GetPublicThumbnail(ctx, fieldID, imageID)
}
