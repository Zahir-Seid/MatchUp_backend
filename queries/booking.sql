-- The capacity primitive. Returns false when the game cannot give up
-- p_amount spots, which the caller turns into a rollback (SDD 9.1).
-- name: ReserveGameSpots :one
SELECT fn_reserve_game_spots(sqlc.arg(game_id), sqlc.arg(amount)::int) AS reserved;

-- name: ReleaseGameSpots :one
SELECT fn_release_game_spots(sqlc.arg(game_id), sqlc.arg(amount)::int) AS released;

-- name: CreateBooking :one
INSERT INTO bookings (
    user_id, booking_type, spot_count, week_count, price_per_player_santim,
    subtotal_santim, discount_santim, total_santim, hold_expires_at, reference_code
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- week_index comes from the array's own ordinality, so the caller passes the
-- target games already sorted by starts_at and the stored order cannot drift
-- from the order the spots were reserved in.
-- name: CreateBookingGames :exec
INSERT INTO booking_games (booking_id, game_id, week_index)
SELECT sqlc.arg(booking_id), t.game_id, t.week_index::int
FROM unnest(sqlc.arg(game_ids)::uuid[]) WITH ORDINALITY AS t(game_id, week_index);

-- Named spots can be sparse (slots 1 and 4 named, 2 and 3 left as headcount),
-- so slot_index is supplied rather than derived; the two arrays are zipped by
-- position.
-- name: CreateBookingParticipants :exec
INSERT INTO booking_participants (booking_id, slot_index, full_name)
SELECT sqlc.arg(booking_id), s.slot_index, n.full_name
FROM unnest(sqlc.arg(slot_indexes)::int[]) WITH ORDINALITY AS s(slot_index, pos)
JOIN unnest(sqlc.arg(full_names)::text[]) WITH ORDINALITY AS n(full_name, pos)
  ON n.pos = s.pos;

-- name: GetBooking :one
SELECT * FROM bookings WHERE id = $1;

-- name: GetBookingForUser :one
SELECT * FROM bookings WHERE id = $1 AND user_id = $2;

-- name: ListBookingsForUser :many
SELECT * FROM bookings
WHERE user_id = $1
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- Every covered game for a set of bookings in one round trip, so "My Bookings"
-- never fans out into a query per booking. Also serves the single-booking
-- detail view with a one-element array.
-- name: ListBookingGamesForBookings :many
SELECT bg.booking_id, bg.week_index, sqlc.embed(g), sqlc.embed(f)
FROM booking_games bg
JOIN games g ON g.id = bg.game_id
JOIN fields f ON f.id = g.field_id
WHERE bg.booking_id = ANY(sqlc.arg(booking_ids)::uuid[])
ORDER BY bg.booking_id, bg.week_index;

-- name: ListBookingParticipantsForBookings :many
SELECT * FROM booking_participants
WHERE booking_id = ANY(sqlc.arg(booking_ids)::uuid[])
ORDER BY booking_id, slot_index;

-- name: ListBookingParticipants :many
SELECT * FROM booking_participants WHERE booking_id = $1 ORDER BY slot_index;

-- name: SetBookingConfirmed :one
UPDATE bookings
SET status = 'confirmed', confirmed_at = now(), hold_expires_at = NULL
WHERE id = $1 AND status IN ('verifying', 'pending_review')
RETURNING *;

-- A flagged payment parks the booking for a human. The hold is cleared: the
-- spots stay reserved (they are not released) but the 5-minute clock stops
-- applying, since the sweep only ever looks at 'pending_payment'.
-- name: SetBookingPendingReview :one
UPDATE bookings
SET status = 'pending_review', hold_expires_at = NULL
WHERE id = $1 AND status = 'verifying'
RETURNING *;

-- Part A of the expiry/verification race fix. A booking is expired only when
-- its hold has run out AND no verification job is live for it. Spots are
-- returned to every game the booking covered in the same statement, so the
-- release can never be lost between two round trips; the SUM matters because
-- several expiring bookings can share one game.
-- name: ExpireHeldBookings :many
WITH expired AS (
    UPDATE bookings b
    SET status = 'expired', hold_expires_at = NULL
    WHERE b.status = 'pending_payment'
      AND b.hold_expires_at < now()
      AND NOT EXISTS (
          SELECT 1
          FROM payments p
          JOIN payment_verification_jobs j ON j.payment_id = p.id
          WHERE p.booking_id = b.id
            AND j.status IN ('pending', 'processing')
      )
    RETURNING b.id, b.user_id, b.spot_count, b.reference_code
),
per_game AS (
    SELECT bg.game_id, SUM(e.spot_count)::int AS spots
    FROM expired e
    JOIN booking_games bg ON bg.booking_id = e.id
    GROUP BY bg.game_id
),
released AS (
    UPDATE games g
    SET spots_remaining = g.spots_remaining + pg.spots,
        updated_at      = now()
    FROM per_game pg
    WHERE g.id = pg.game_id
    RETURNING g.id
)
SELECT e.id, e.user_id, e.reference_code, (SELECT count(*) FROM released) AS games_credited
FROM expired e;

-- Bookings whose hold is about to run out, for the "finish your payment"
-- nudge. Notified at most once, tracked by the caller's notification rows.
-- Claims the bookings whose hold is about to lapse and marks them warned in
-- the same statement, so the warning is sent once no matter how often the
-- sweep runs or how many workers are running it.
-- name: ClaimBookingsNearingExpiry :many
WITH claimable AS (
    SELECT b.id
    FROM bookings b
    WHERE b.status = 'pending_payment'
      AND b.hold_warning_sent_at IS NULL
      AND b.hold_expires_at BETWEEN now() AND now() + sqlc.arg(warn_before)::interval
    ORDER BY b.hold_expires_at
    LIMIT sqlc.arg(max_rows)::int
    FOR UPDATE SKIP LOCKED
)
UPDATE bookings b
SET hold_warning_sent_at = now()
FROM claimable c
WHERE b.id = c.id
RETURNING b.id, b.user_id, b.reference_code, b.hold_expires_at, b.total_santim;

-- Admin cancellation (PRD FR27). Releases spots for any booking that was
-- still holding them, using the same aggregate-then-credit shape as expiry.
-- name: CancelBooking :many
WITH cancelled AS (
    UPDATE bookings b
    SET status = 'cancelled', cancelled_at = now(), hold_expires_at = NULL
    WHERE b.id = sqlc.arg(booking_id)
      AND b.status IN ('pending_payment', 'verifying', 'pending_review', 'confirmed')
    RETURNING b.id, b.user_id, b.spot_count, b.reference_code, b.status
),
per_game AS (
    SELECT bg.game_id, SUM(c.spot_count)::int AS spots
    FROM cancelled c
    JOIN booking_games bg ON bg.booking_id = c.id
    JOIN games g2 ON g2.id = bg.game_id
    WHERE g2.status = 'scheduled'
    GROUP BY bg.game_id
),
released AS (
    UPDATE games g
    SET spots_remaining = g.spots_remaining + pg.spots,
        updated_at      = now()
    FROM per_game pg
    WHERE g.id = pg.game_id
    RETURNING g.id
)
SELECT c.id, c.user_id, c.reference_code, (SELECT count(*) FROM released) AS games_credited
FROM cancelled c;

-- Who is affected when a game is cancelled: every booking still holding or
-- holding onto spots on it. Expired and already-cancelled bookings are left
-- out -- they have no claim on the game and their owners do not need telling.
-- name: ListLiveBookingsForGame :many
SELECT DISTINCT b.id, b.user_id, b.reference_code, b.status, b.spot_count,
       b.total_santim, b.confirmed_at
FROM bookings b
JOIN booking_games bg ON bg.booking_id = b.id
WHERE bg.game_id = $1
  AND b.status IN ('pending_payment', 'verifying', 'pending_review', 'confirmed')
ORDER BY b.reference_code;

-- name: GetBookingWithUser :one
SELECT sqlc.embed(b), u.full_name AS user_name, u.email AS user_email, u.phone AS user_phone
FROM bookings b
JOIN users u ON u.id = b.user_id
WHERE b.id = $1;
