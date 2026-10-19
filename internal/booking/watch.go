package booking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/notification"
)

// Watch subscribes a user to the next spot that frees up on a sold-out game
// (PRD FR29). The alert is a heads-up, not a reservation: booking stays
// first-come, and the watch is spent when the alert goes out.
//
// Only a full game can be watched. A stale screen that still offers "notify
// me" on a game with spots is told to book instead, and a cancelled or started
// game is refused exactly as a booking would be. Watching twice is a no-op.
func (s *Service) Watch(ctx context.Context, userID, gameID uuid.UUID) error {
	game, err := s.store.GetGame(ctx, gameID)
	if err != nil {
		if db.IsNoRows(err) {
			return httpx.ErrNotFound.WithCause(err)
		}
		return fmt.Errorf("get game: %w", err)
	}
	if err := s.assertBookable(game); err != nil {
		return err
	}
	if game.SpotsRemaining > 0 {
		return httpx.NewError(http.StatusConflict, httpx.CodeGameNotFull,
			"This game still has spots open, so there is nothing to wait for. Book it instead.")
	}
	if err := s.store.CreateGameWatch(ctx, db.CreateGameWatchParams{
		UserID: userID,
		GameID: gameID,
	}); err != nil {
		return fmt.Errorf("create game watch: %w", err)
	}
	return nil
}

// Unwatch drops a watch. Success is reported whether or not one existed: the
// caller's intent is "stop telling me about this game", and a stale button
// press that finds nothing to delete has achieved exactly that.
func (s *Service) Unwatch(ctx context.Context, userID, gameID uuid.UUID) error {
	if err := s.store.DeleteGameWatch(ctx, db.DeleteGameWatchParams{
		UserID: userID,
		GameID: gameID,
	}); err != nil {
		return fmt.Errorf("delete game watch: %w", err)
	}
	return nil
}

// Watching reports whether the caller holds a watch on a game, so the app can
// render the button correctly after a restart.
func (s *Service) Watching(ctx context.Context, userID, gameID uuid.UUID) (bool, error) {
	if _, err := s.store.GetGame(ctx, gameID); err != nil {
		if db.IsNoRows(err) {
			return false, httpx.ErrNotFound.WithCause(err)
		}
		return false, fmt.Errorf("get game: %w", err)
	}
	watching, err := s.store.GetGameWatch(ctx, db.GetGameWatchParams{
		UserID: userID,
		GameID: gameID,
	})
	if err != nil {
		return false, fmt.Errorf("get game watch: %w", err)
	}
	return watching, nil
}

// NotifySpotsAvailable enqueues "a spot opened up" to everyone watching the
// given games and spends their watches. It runs inside the transaction that
// freed (or added) the spots, so the release and the alerts commit together: a
// failed alert rolls the release back rather than losing the message the
// release made true (SDD 9.1).
//
// Callers pass every game a release touched. Which of them are worth an alert
// is decided in the query: only a scheduled, future game that now has spots.
// A nil notifier (tests) skips the whole pass.
func NotifySpotsAvailable(ctx context.Context, q *db.Queries, notifier Notifier, gameIDs []uuid.UUID) error {
	if notifier == nil || len(gameIDs) == 0 {
		return nil
	}
	watchers, err := q.ListWatchersForAvailableGames(ctx, gameIDs)
	if err != nil {
		return fmt.Errorf("list watchers for games: %w", err)
	}
	if len(watchers) == 0 {
		return nil
	}
	spent := make([]uuid.UUID, 0, len(watchers))
	for _, w := range watchers {
		if _, err := notifier.Enqueue(ctx, q, notification.Request{
			UserID: w.UserID,
			Type:   notification.TypeGameSpotAvailable,
			Payload: map[string]any{
				"game_id":    w.GameID.String(),
				"field_name": w.FieldName,
				"starts_at":  w.StartsAt.UTC().Format(timeLayout),
			},
		}); err != nil {
			return fmt.Errorf("notify watcher %s of game %s: %w", w.UserID, w.GameID, err)
		}
		spent = append(spent, w.ID)
	}
	if err := q.DeleteGameWatches(ctx, spent); err != nil {
		return fmt.Errorf("spend game watches: %w", err)
	}
	return nil
}
