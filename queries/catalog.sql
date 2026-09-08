-- name: CreateField :one
INSERT INTO fields (name, location, image_url)
VALUES ($1, $2, $3)
RETURNING *;

-- name: UpdateField :one
UPDATE fields
SET name      = COALESCE(sqlc.narg(name), name),
    location  = COALESCE(sqlc.narg(location), location),
    image_url = COALESCE(sqlc.narg(image_url), image_url),
    is_active = COALESCE(sqlc.narg(is_active), is_active)
WHERE id = $1
RETURNING *;

-- name: GetField :one
SELECT * FROM fields WHERE id = $1;

-- name: ListActiveFields :many
SELECT * FROM fields WHERE is_active ORDER BY name;

-- name: ListAllFields :many
SELECT * FROM fields ORDER BY name;

-- name: CreateGame :one
INSERT INTO games (
    field_id, series_id, starts_at, duration_minutes, format,
    price_per_player_santim, cost_to_business_santim, capacity, spots_remaining
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING *;

-- name: GetGame :one
SELECT * FROM games WHERE id = $1;

-- name: GetGameWithField :one
SELECT sqlc.embed(g), sqlc.embed(f)
FROM games g JOIN fields f ON f.id = g.field_id
WHERE g.id = $1;

-- The public discovery query (PRD FR6, FR7). Only scheduled games in the
-- future are listed; the caller pages with limit/offset.
-- name: ListUpcomingGames :many
SELECT sqlc.embed(g), sqlc.embed(f)
FROM games g
JOIN fields f ON f.id = g.field_id
WHERE g.status = 'scheduled'
  AND g.starts_at > now()
  AND (sqlc.narg(field_id)::uuid IS NULL OR g.field_id = sqlc.narg(field_id))
  AND (sqlc.narg(format)::text IS NULL OR g.format = sqlc.narg(format))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR g.starts_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR g.starts_at < sqlc.narg(to_time))
  AND (NOT sqlc.arg(only_available)::boolean OR g.spots_remaining > 0)
ORDER BY g.starts_at
LIMIT $1 OFFSET $2;

-- name: UpdateGame :one
UPDATE games
SET starts_at               = COALESCE(sqlc.narg(starts_at), starts_at),
    duration_minutes        = COALESCE(sqlc.narg(duration_minutes), duration_minutes),
    format                  = COALESCE(sqlc.narg(format), format),
    price_per_player_santim = COALESCE(sqlc.narg(price_per_player_santim), price_per_player_santim),
    cost_to_business_santim = COALESCE(sqlc.narg(cost_to_business_santim), cost_to_business_santim)
WHERE id = $1 AND status = 'scheduled'
RETURNING *;

-- Raising capacity must credit the same delta to spots_remaining, or the
-- new seats would exist on paper but never become bookable.
-- name: IncreaseGameCapacity :one
UPDATE games
SET capacity        = capacity + sqlc.arg(delta)::int,
    spots_remaining = spots_remaining + sqlc.arg(delta)::int
WHERE id = $1 AND status = 'scheduled' AND sqlc.arg(delta)::int > 0
RETURNING *;

-- Lowering capacity can only consume spots that are still free.
-- name: DecreaseGameCapacity :one
UPDATE games
SET capacity        = capacity - sqlc.arg(delta)::int,
    spots_remaining = spots_remaining - sqlc.arg(delta)::int
WHERE id = $1 AND status = 'scheduled'
  AND sqlc.arg(delta)::int > 0
  AND spots_remaining >= sqlc.arg(delta)::int
RETURNING *;

-- name: CancelGame :one
UPDATE games SET status = 'cancelled' WHERE id = $1 AND status = 'scheduled' RETURNING *;

-- The four target games of a monthly booking: the chosen occurrence plus the
-- next three in the same recurring series, in chronological order. Returned
-- in the exact order the caller must reserve them in (see booking.sql).
-- name: ListSeriesOccurrencesFrom :many
SELECT * FROM games
WHERE series_id = $1
  AND starts_at >= sqlc.arg(from_time)::timestamptz
  AND status = 'scheduled'
ORDER BY starts_at
LIMIT sqlc.arg(occurrences)::int;

-- The admin game list. Unlike the public query this shows cancelled and past
-- games too: an admin managing a schedule needs to see what was cancelled and
-- what already happened, not only what is still on sale.
-- name: ListGamesForAdmin :many
SELECT sqlc.embed(g), sqlc.embed(f)
FROM games g
JOIN fields f ON f.id = g.field_id
WHERE (sqlc.narg(field_id)::uuid IS NULL OR g.field_id = sqlc.narg(field_id))
  AND (sqlc.narg(status)::text IS NULL OR g.status = sqlc.narg(status))
  AND (sqlc.narg(format)::text IS NULL OR g.format = sqlc.narg(format))
  AND (sqlc.narg(series_id)::uuid IS NULL OR g.series_id = sqlc.narg(series_id))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR g.starts_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR g.starts_at < sqlc.narg(to_time))
ORDER BY g.starts_at DESC
LIMIT $1 OFFSET $2;

-- name: CountGamesForAdmin :one
SELECT count(*)
FROM games g
WHERE (sqlc.narg(field_id)::uuid IS NULL OR g.field_id = sqlc.narg(field_id))
  AND (sqlc.narg(status)::text IS NULL OR g.status = sqlc.narg(status))
  AND (sqlc.narg(format)::text IS NULL OR g.format = sqlc.narg(format))
  AND (sqlc.narg(series_id)::uuid IS NULL OR g.series_id = sqlc.narg(series_id))
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR g.starts_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR g.starts_at < sqlc.narg(to_time));
