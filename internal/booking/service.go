// Package booking owns reservations: pricing a booking, holding the spots it
// needs atomically, and the lifecycle of a booking row afterwards.
package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Booking statuses (PRD FR20).
const (
	StatusPendingPayment = "pending_payment"
	StatusVerifying      = "verifying"
	StatusConfirmed      = "confirmed"
	StatusPendingReview  = "pending_review"
	StatusExpired        = "expired"
	StatusCancelled      = "cancelled"
)

// timeLayout is the timestamp format in API responses.
const timeLayout = time.RFC3339

// maxSpotsPerBooking bounds a single reservation. A game's capacity bounds it
// too, but this stops one request from attempting to take an entire field.
const maxSpotsPerBooking = 14

// Service creates and manages bookings.
type Service struct {
	store *db.Store
	log   *slog.Logger

	holdDuration time.Duration
	weekCount    int32
	now          func() time.Time
}

// Options configures the booking service.
type Options struct {
	Store        *db.Store
	Logger       *slog.Logger
	HoldDuration time.Duration
	WeekCount    int
}

// NewService builds the booking service.
func NewService(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:        opts.Store,
		log:          log,
		holdDuration: opts.HoldDuration,
		weekCount:    int32(opts.WeekCount),
		now:          time.Now,
	}
}

// Participant is an optionally named spot (PRD FR10). Spots without a name are
// still valid; they are simply headcount.
type Participant struct {
	SlotIndex int32
	FullName  string
}

// CreateInput is a booking request.
type CreateInput struct {
	UserID       uuid.UUID
	GameID       uuid.UUID
	BookingType  string
	SpotCount    int32
	Participants []Participant
}

// Quote prices a prospective booking without reserving anything, so the app can
// show the breakdown before the user commits (PRD FR11).
func (s *Service) Quote(ctx context.Context, gameID uuid.UUID, bookingType string, spotCount int32) (*Quote, error) {
	games, err := s.resolveTargetGames(ctx, s.store.Queries, gameID, bookingType)
	if err != nil {
		return nil, err
	}
	if err := s.assertSpotsPlausible(games, spotCount); err != nil {
		return nil, err
	}
	return buildQuote(bookingType, spotCount, games, s.discountPolicy(ctx))
}

// discountPolicy reads the dashboard's monthly pricing settings.
//
// A missing row — an unseeded database, a test harness that truncated the
// table — falls back to the built-in default rather than failing the quote, and
// so does an unreadable one: a settings read must not lose a sale over a value
// that is, in the worst case, the same 12.5% the code hardcoded before the
// dashboard owned it. Both cases are logged loudly enough to notice.
func (s *Service) discountPolicy(ctx context.Context) DiscountPolicy {
	row, err := s.store.GetBookingSettings(ctx)
	if err != nil {
		level := slog.LevelWarn
		if !db.IsNoRows(err) {
			level = slog.LevelError
		}
		s.log.Log(ctx, level, "booking settings unavailable; using the built-in discount default",
			slog.Any("error", err))
		return DefaultDiscountPolicy
	}
	return DiscountPolicy{BPS: row.MonthlyDiscountBps, Label: row.MonthlyDiscountLabel}
}

// Create reserves the spots and records the booking.
//
// Everything happens in one transaction: each covered game's spots are taken by
// a conditional update, and if any single week cannot supply them the whole
// transaction rolls back — no partial monthly booking, no spots stranded in a
// reserved state (Fix 1, PRD FR14).
func (s *Service) Create(ctx context.Context, in CreateInput) (*View, error) {
	games, err := s.resolveTargetGames(ctx, s.store.Queries, in.GameID, in.BookingType)
	if err != nil {
		return nil, err
	}
	if err := s.assertSpotsPlausible(games, in.SpotCount); err != nil {
		return nil, err
	}
	if err := validateParticipants(in.Participants, in.SpotCount); err != nil {
		return nil, err
	}

	quote, err := buildQuote(in.BookingType, in.SpotCount, games, s.discountPolicy(ctx))
	if err != nil {
		return nil, err
	}

	holdExpiry := s.now().Add(s.holdDuration)
	var created db.Booking

	err = s.store.InTx(ctx, func(q *db.Queries) error {
		// Games are always taken in ascending start time. Two monthly bookings
		// overlapping on the same series therefore request the same games in
		// the same order, which is what keeps them from deadlocking on each
		// other's row locks.
		for i, game := range games {
			ok, err := q.ReserveGameSpots(ctx, db.ReserveGameSpotsParams{
				GameID: game.ID,
				Amount: in.SpotCount,
			})
			if err != nil {
				return fmt.Errorf("reserve spots for game %s: %w", game.ID, err)
			}
			if !ok {
				return insufficientCapacity(in.BookingType, i+1, len(games), game, in.SpotCount)
			}
		}

		booking, err := s.insertBooking(ctx, q, in, quote, holdExpiry)
		if err != nil {
			return err
		}

		gameIDs := make([]uuid.UUID, 0, len(games))
		for _, game := range games {
			gameIDs = append(gameIDs, game.ID)
		}
		// week_index is the array ordinality, so it matches the order the
		// spots were reserved in and, later, released in.
		if err := q.CreateBookingGames(ctx, db.CreateBookingGamesParams{
			BookingID: booking.ID,
			GameIds:   gameIDs,
		}); err != nil {
			return fmt.Errorf("link booking games: %w", err)
		}

		if len(in.Participants) > 0 {
			slots := make([]int32, 0, len(in.Participants))
			names := make([]string, 0, len(in.Participants))
			for _, p := range in.Participants {
				slots = append(slots, p.SlotIndex)
				names = append(names, p.FullName)
			}
			if err := q.CreateBookingParticipants(ctx, db.CreateBookingParticipantsParams{
				BookingID:   booking.ID,
				SlotIndexes: slots,
				FullNames:   names,
			}); err != nil {
				return fmt.Errorf("create booking participants: %w", err)
			}
		}

		created = booking
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.Info("booking held",
		slog.String("booking_id", created.ID.String()),
		slog.String("reference_code", created.ReferenceCode),
		slog.String("booking_type", created.BookingType),
		slog.Int("games", len(games)),
		slog.Int64("total_santim", created.TotalSantim))

	return s.viewFor(ctx, created)
}

// insertBooking writes the booking row, retrying on the vanishingly unlikely
// reference-code collision. Any other unique violation is a real fault.
func (s *Service) insertBooking(ctx context.Context, q *db.Queries, in CreateInput, quote *Quote, holdExpiry time.Time) (db.Booking, error) {
	const attempts = 3
	for attempt := range attempts {
		code, err := newReferenceCode()
		if err != nil {
			return db.Booking{}, err
		}
		booking, err := q.CreateBooking(ctx, db.CreateBookingParams{
			UserID:               in.UserID,
			BookingType:          in.BookingType,
			SpotCount:            in.SpotCount,
			WeekCount:            quote.WeekCount,
			PricePerPlayerSantim: quote.PricePerPlayerSantim,
			SubtotalSantim:       quote.SubtotalSantim,
			DiscountSantim:       quote.DiscountSantim,
			TotalSantim:          quote.TotalSantim,
			HoldExpiresAt:        &holdExpiry,
			ReferenceCode:        code,
		})
		if err == nil {
			return booking, nil
		}
		if db.IsUniqueViolation(err, "bookings_reference_code_key") && attempt < attempts-1 {
			s.log.Warn("booking reference code collision; retrying",
				slog.String("reference_code", code))
			continue
		}
		return db.Booking{}, fmt.Errorf("create booking: %w", err)
	}
	return db.Booking{}, errors.New("create booking: exhausted reference code attempts")
}

// resolveTargetGames returns the games a booking will cover, in ascending start
// order.
//
// A single booking covers exactly the requested game. A monthly booking covers
// the next weekCount occurrences of the same recurring slot, which is what makes
// "monthly" mean four reserved sessions rather than one row with a multiplier
// (Fix 1).
func (s *Service) resolveTargetGames(ctx context.Context, q *db.Queries, gameID uuid.UUID, bookingType string) ([]db.Game, error) {
	game, err := q.GetGame(ctx, gameID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get game: %w", err)
	}
	if err := s.assertBookable(game); err != nil {
		return nil, err
	}

	switch bookingType {
	case TypeSingle:
		return []db.Game{game}, nil

	case TypeMonthly4W:
		if game.SeriesID == nil {
			return nil, httpx.NewError(http.StatusConflict, httpx.CodeSeriesIncomplete,
				"This game is a one-off, so it cannot be booked as a monthly plan.")
		}
		games, err := q.ListSeriesOccurrencesFrom(ctx, db.ListSeriesOccurrencesFromParams{
			SeriesID:    game.SeriesID,
			FromTime:    game.StartsAt,
			Occurrences: s.weekCount,
		})
		if err != nil {
			return nil, fmt.Errorf("list series occurrences: %w", err)
		}
		if int32(len(games)) < s.weekCount {
			return nil, httpx.NewError(http.StatusConflict, httpx.CodeSeriesIncomplete,
				fmt.Sprintf("Only %d of the next %d weekly sessions are scheduled yet, so a monthly plan cannot be booked from this date.",
					len(games), s.weekCount))
		}
		// The requested game must be the first occurrence; otherwise the
		// customer would be charged for weeks starting somewhere they did not
		// choose.
		if games[0].ID != game.ID {
			return nil, httpx.NewError(http.StatusConflict, httpx.CodeSeriesIncomplete,
				"That session is no longer the next one in its weekly series. Reload and try again.")
		}
		return games, nil

	default:
		return nil, httpx.ErrValidation.WithField("booking_type",
			fmt.Sprintf("must be %q or %q", TypeSingle, TypeMonthly4W))
	}
}

// assertBookable rejects games that cannot take a booking at all.
func (s *Service) assertBookable(game db.Game) error {
	if game.Status != "scheduled" {
		return httpx.NewError(http.StatusConflict, httpx.CodeGameUnavailable,
			"This game is no longer open for booking.")
	}
	if !game.StartsAt.After(s.now()) {
		return httpx.NewError(http.StatusConflict, httpx.CodeGameUnavailable,
			"This game has already started.")
	}
	return nil
}

// assertSpotsPlausible does the cheap up-front checks. It is advisory only: the
// authoritative capacity decision is the conditional update inside the
// transaction, because anything read here can be taken before the write lands.
func (s *Service) assertSpotsPlausible(games []db.Game, spotCount int32) error {
	if spotCount < 1 || spotCount > maxSpotsPerBooking {
		return httpx.ErrValidation.WithField("spot_count",
			fmt.Sprintf("must be between 1 and %d", maxSpotsPerBooking))
	}
	for i, game := range games {
		if spotCount > game.Capacity {
			return httpx.ErrValidation.WithField("spot_count",
				fmt.Sprintf("this game holds %d players in total", game.Capacity))
		}
		if game.SpotsRemaining < spotCount {
			return insufficientCapacity(typeFor(len(games)), i+1, len(games), game, spotCount)
		}
	}
	return nil
}

func typeFor(weeks int) string {
	if weeks > 1 {
		return TypeMonthly4W
	}
	return TypeSingle
}

// insufficientCapacity builds the rejection, naming the week that failed so a
// monthly booking's failure is actionable rather than opaque.
func insufficientCapacity(bookingType string, weekIndex, weekTotal int, game db.Game, wanted int32) error {
	remaining := game.SpotsRemaining
	if remaining < 0 {
		remaining = 0
	}
	message := fmt.Sprintf("Only %d of %d spots are left for this game.", remaining, wanted)
	if bookingType == TypeMonthly4W {
		message = fmt.Sprintf("Week %d of %d (%s) has only %d of the %d spots you asked for, so the monthly plan cannot be reserved.",
			weekIndex, weekTotal, game.StartsAt.Format("Mon 2 Jan"), remaining, wanted)
	}
	err := httpx.NewError(http.StatusConflict, httpx.CodeInsufficientCapacity, message).
		WithField("game_id", game.ID.String())
	if bookingType == TypeMonthly4W {
		return err.WithField("week_index", fmt.Sprint(weekIndex))
	}
	return err
}

// validateParticipants checks named spots against the reserved count. Slot
// indexes may be sparse — naming spots 1 and 4 of 4 is valid — but they must be
// within range and distinct.
func validateParticipants(participants []Participant, spotCount int32) error {
	if len(participants) == 0 {
		return nil
	}
	if int32(len(participants)) > spotCount {
		return httpx.ErrValidation.WithField("participants",
			fmt.Sprintf("you named %d players but reserved %d spots", len(participants), spotCount))
	}
	seen := make(map[int32]struct{}, len(participants))
	for _, p := range participants {
		if p.SlotIndex < 1 || p.SlotIndex > spotCount {
			return httpx.ErrValidation.WithField("participants",
				fmt.Sprintf("slot %d is outside the %d spots reserved", p.SlotIndex, spotCount))
		}
		if _, dup := seen[p.SlotIndex]; dup {
			return httpx.ErrValidation.WithField("participants",
				fmt.Sprintf("slot %d is named twice", p.SlotIndex))
		}
		seen[p.SlotIndex] = struct{}{}
	}
	return nil
}
