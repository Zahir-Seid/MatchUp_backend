//go:build integration

package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for goose only
	"github.com/pressly/goose/v3"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
)

// envURL names the database these tests are allowed to destroy.
//
// It is deliberately not DATABASE_URL: the harness truncates every table, and a
// developer with a production URL exported in their shell should not be one
// mistyped command away from finding that out.
const envURL = "TEST_DATABASE_URL"

// envForce overrides the disposable-database check for the rare case of a
// shared CI database whose name says nothing about what it is for.
const envForce = "TEST_DATABASE_FORCE"

var migrateOnce sync.Once

// Store opens a store against TEST_DATABASE_URL with every migration applied
// and every table empty. The pool is closed when the test finishes.
//
// Migrations run through goose against the same directory the production image
// carries, so a test that passes against a stale schema is not possible: the
// schema under test is the one in the repository.
func Store(t *testing.T) *db.Store {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(envURL))
	if dsn == "" {
		t.Skipf("%s is not set; skipping the tests that need a database", envURL)
	}
	assertDisposable(t, dsn)

	migrateOnce.Do(func() { migrate(t, dsn) })

	store, err := db.Open(context.Background(), db.PoolConfig{
		URL:      dsn,
		MaxConns: 10,
		MinConns: 1,
	})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(store.Close)

	truncate(t, store)
	return store
}

// assertDisposable refuses to run against anything that does not look like a
// throwaway database. The harness truncates every table in the schema, so the
// cost of getting this wrong is somebody's data rather than a failed test.
func assertDisposable(t *testing.T, dsn string) {
	t.Helper()
	if os.Getenv(envForce) == "1" {
		return
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", envURL, err)
	}
	host := u.Hostname()
	name := strings.TrimPrefix(u.Path, "/")

	local := host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		// The compose service name, for a test run inside the network.
		host == "postgres"
	if local || strings.Contains(strings.ToLower(name), "test") {
		return
	}
	t.Fatalf("%s points at %q on host %q, which is neither local nor named as a test database. "+
		"This harness truncates every table. Set %s=1 if that is really what you want.",
		envURL, name, host, envForce)
}

// migrate applies the repository's migrations. It uses database/sql because
// that is goose's interface; nothing else in the project does.
func migrate(t *testing.T, dsn string) {
	t.Helper()

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open migration connection: %v", err)
	}
	defer sqlDB.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	// The migration log is noise in test output; a failure still reports.
	goose.SetLogger(goose.NopLogger())

	if err := goose.Up(sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}

// migrationsDir locates the migrations relative to this file, so the harness
// works from whichever package directory the test happens to run in.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test harness source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

// truncate empties every table except goose's own bookkeeping, in one
// statement so foreign keys never see a half-empty schema. The table list is
// read from the catalog rather than hard-coded, so a new migration cannot leave
// a table quietly seeded between tests.
func truncate(t *testing.T, store *db.Store) {
	t.Helper()
	ctx := context.Background()

	rows, err := store.Pool().Query(ctx, `
		SELECT quote_ident(tablename)
		FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("no tables found; did the migrations run?")
	}

	stmt := fmt.Sprintf("TRUNCATE %s RESTART IDENTITY CASCADE", strings.Join(tables, ", "))
	if _, err := store.Pool().Exec(ctx, stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// User inserts a player with a unique email.
func User(t *testing.T, store *db.Store) db.User {
	t.Helper()
	user, err := store.CreateUser(context.Background(), db.CreateUserParams{
		FullName: "Test Player",
		Email:    fmt.Sprintf("player-%s@example.test", uuid.NewString()),
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// Field inserts an active field.
func Field(t *testing.T, store *db.Store) db.Field {
	t.Helper()
	field, err := store.CreateField(context.Background(), db.CreateFieldParams{
		Name:     "Test Field " + uuid.NewString()[:8],
		Location: "Addis Ababa",
	})
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	return field
}

// GameSpec describes a game to seed. The zero value is a one-off 5-a-side game
// with room for ten players, starting a week out.
type GameSpec struct {
	FieldID  uuid.UUID
	SeriesID *uuid.UUID
	StartsAt time.Time
	Capacity int32
	// PriceSantim defaults to 15000 (150 ETB), which is what the MVP charged.
	PriceSantim int64
}

// Game inserts one game.
func Game(t *testing.T, store *db.Store, spec GameSpec) db.Game {
	t.Helper()
	if spec.StartsAt.IsZero() {
		spec.StartsAt = time.Now().UTC().Add(7 * 24 * time.Hour)
	}
	if spec.Capacity == 0 {
		spec.Capacity = 10
	}
	if spec.PriceSantim == 0 {
		spec.PriceSantim = 15000
	}
	game, err := store.CreateGame(context.Background(), db.CreateGameParams{
		FieldID:              spec.FieldID,
		SeriesID:             spec.SeriesID,
		StartsAt:             spec.StartsAt,
		DurationMinutes:      60,
		Format:               "5_a_side",
		PricePerPlayerSantim: spec.PriceSantim,
		Capacity:             spec.Capacity,
	})
	if err != nil {
		t.Fatalf("create game: %v", err)
	}
	return game
}

// Series seeds count weekly occurrences of one recurring slot, in chronological
// order, and returns them with the shared series id.
func Series(t *testing.T, store *db.Store, spec GameSpec, count int) (uuid.UUID, []db.Game) {
	t.Helper()
	seriesID := uuid.New()
	spec.SeriesID = &seriesID
	if spec.StartsAt.IsZero() {
		spec.StartsAt = time.Now().UTC().Add(7 * 24 * time.Hour)
	}
	games := make([]db.Game, 0, count)
	for i := range count {
		weekly := spec
		weekly.StartsAt = spec.StartsAt.AddDate(0, 0, 7*i)
		games = append(games, Game(t, store, weekly))
	}
	return seriesID, games
}

// SpotsRemaining reads a game's live counter, which is the capacity source of
// truth the reservation function decrements.
func SpotsRemaining(t *testing.T, store *db.Store, gameID uuid.UUID) int32 {
	t.Helper()
	game, err := store.GetGame(context.Background(), gameID)
	if err != nil {
		t.Fatalf("get game: %v", err)
	}
	return game.SpotsRemaining
}

// BackdateHold moves a booking's hold into the past so the expiry sweep sees it
// as lapsed. Tests must not wait out a five-minute hold to check the sweep, and
// the service always stamps the hold from the clock.
func BackdateHold(t *testing.T, store *db.Store, bookingID uuid.UUID) {
	t.Helper()
	tag, err := store.Pool().Exec(context.Background(),
		`UPDATE bookings SET hold_expires_at = now() - interval '1 minute' WHERE id = $1`,
		bookingID)
	if err != nil {
		t.Fatalf("backdate hold: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate hold: %d rows affected", tag.RowsAffected())
	}
}
