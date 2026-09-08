-- Revenue and, where cost data exists, real margin per field (PRD FR28, 3).
-- Revenue is counted once per booking and attributed to the field of its
-- first covered game; cost is summed across every game the booking covers,
-- so a monthly booking's four weeks all contribute. cost_complete reports
-- whether every covered game had a cost recorded -- the dashboard must not
-- present margin as fact when it is only partially known.
-- name: RevenueByField :many
WITH confirmed AS (
    SELECT b.id, b.total_santim, b.spot_count,
           (SELECT bg.game_id FROM booking_games bg
            WHERE bg.booking_id = b.id ORDER BY bg.week_index LIMIT 1) AS first_game_id
    FROM bookings b
    WHERE b.status = 'confirmed'
      AND b.confirmed_at >= sqlc.arg(from_time)::timestamptz
      AND b.confirmed_at <  sqlc.arg(to_time)::timestamptz
),
costs AS (
    SELECT bg.booking_id,
           SUM(g.cost_to_business_santim * c.spot_count)::bigint AS cost_santim,
           bool_and(g.cost_to_business_santim IS NOT NULL)       AS cost_complete
    FROM confirmed c
    JOIN booking_games bg ON bg.booking_id = c.id
    JOIN games g          ON g.id = bg.game_id
    GROUP BY bg.booking_id
)
SELECT f.id                                        AS field_id,
       f.name                                      AS field_name,
       count(*)                                    AS bookings,
       SUM(c.spot_count)::bigint                   AS spots_sold,
       SUM(c.total_santim)::bigint                 AS revenue_santim,
       SUM(COALESCE(ct.cost_santim, 0))::bigint    AS cost_santim,
       bool_and(COALESCE(ct.cost_complete, false)) AS cost_complete
FROM confirmed c
JOIN games g0   ON g0.id = c.first_game_id
JOIN fields f   ON f.id = g0.field_id
LEFT JOIN costs ct ON ct.booking_id = c.id
GROUP BY f.id, f.name
ORDER BY revenue_santim DESC;

-- name: BookingVolumeByDay :many
SELECT date_trunc('day', b.created_at)::date AS day,
       count(*)                              AS bookings_created,
       count(*) FILTER (WHERE b.status = 'confirmed')      AS confirmed,
       count(*) FILTER (WHERE b.status = 'expired')        AS expired,
       count(*) FILTER (WHERE b.status = 'cancelled')      AS cancelled,
       count(*) FILTER (WHERE b.status = 'pending_review') AS pending_review,
       COALESCE(SUM(b.total_santim) FILTER (WHERE b.status = 'confirmed'), 0)::bigint
           AS confirmed_revenue_santim
FROM bookings b
WHERE b.created_at >= sqlc.arg(from_time)::timestamptz
  AND b.created_at <  sqlc.arg(to_time)::timestamptz
GROUP BY day
ORDER BY day;

-- Which weekday/hour slots actually sell (PRD FR28, J8).
-- name: PopularSlots :many
SELECT EXTRACT(ISODOW FROM g.starts_at)::int AS iso_day_of_week,
       EXTRACT(HOUR   FROM g.starts_at)::int AS hour_of_day,
       count(DISTINCT b.id)                  AS bookings,
       SUM(b.spot_count)::bigint             AS spots_sold
FROM bookings b
JOIN booking_games bg ON bg.booking_id = b.id
JOIN games g          ON g.id = bg.game_id
WHERE b.status = 'confirmed'
  AND g.starts_at >= sqlc.arg(from_time)::timestamptz
  AND g.starts_at <  sqlc.arg(to_time)::timestamptz
GROUP BY iso_day_of_week, hour_of_day
ORDER BY spots_sold DESC;

-- name: PaymentMethodBreakdown :many
SELECT p.method,
       count(*)                                                        AS submissions,
       count(*) FILTER (WHERE p.status = 'verified')                    AS auto_verified,
       count(*) FILTER (WHERE p.status = 'approved')                    AS approved_by_admin,
       count(*) FILTER (WHERE p.status = 'rejected')                    AS rejected,
       count(*) FILTER (WHERE p.status IN ('mismatch', 'not_found'))    AS awaiting_review,
       COALESCE(SUM(p.expected_amount_santim)
                FILTER (WHERE p.status IN ('verified', 'approved')), 0)::bigint AS collected_santim
FROM payments p
WHERE p.created_at >= sqlc.arg(from_time)::timestamptz
  AND p.created_at <  sqlc.arg(to_time)::timestamptz
GROUP BY p.method
ORDER BY p.method;

-- The headline operational metric (PRD 15): what share of payments cleared
-- without a human touching them.
-- name: VerificationOutcomeSummary :one
SELECT count(*)                                                     AS total,
       count(*) FILTER (WHERE status = 'verified')                   AS auto_verified,
       count(*) FILTER (WHERE status IN ('mismatch', 'not_found',
                                         'approved', 'rejected'))    AS needed_review,
       count(*) FILTER (WHERE status = 'pending')                    AS still_pending
FROM payments
WHERE created_at >= sqlc.arg(from_time)::timestamptz
  AND created_at <  sqlc.arg(to_time)::timestamptz;

-- name: ListBookingsForAdmin :many
SELECT sqlc.embed(b), u.full_name AS user_name, u.email AS user_email,
       (SELECT count(*) FROM payments p WHERE p.booking_id = b.id) AS payment_count
FROM bookings b
JOIN users u ON u.id = b.user_id
WHERE (sqlc.narg(status)::text IS NULL OR b.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL
       OR b.reference_code ILIKE '%' || sqlc.narg(search) || '%'
       OR u.email::text ILIKE '%' || sqlc.narg(search) || '%'
       OR u.full_name ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY b.created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountBookingsForAdmin :one
SELECT count(*)
FROM bookings b
JOIN users u ON u.id = b.user_id
WHERE (sqlc.narg(status)::text IS NULL OR b.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL
       OR b.reference_code ILIKE '%' || sqlc.narg(search) || '%'
       OR u.email::text ILIKE '%' || sqlc.narg(search) || '%'
       OR u.full_name ILIKE '%' || sqlc.narg(search) || '%');

-- Cancellation and no-show pressure per field (PRD FR28). "No-show" has no
-- attendance record to draw on -- nobody marks players present -- so the
-- closest honest measure is a booking that was confirmed and paid for and
-- then cancelled after the fact, which is what abandoned_after_confirm counts.
-- The dashboard must label it as that rather than as attendance.
-- name: CancellationRates :many
WITH scoped AS (
    SELECT b.id, b.status, b.spot_count, b.total_santim, b.confirmed_at, b.cancelled_at,
           (SELECT bg.game_id FROM booking_games bg
            WHERE bg.booking_id = b.id ORDER BY bg.week_index LIMIT 1) AS first_game_id
    FROM bookings b
    WHERE b.created_at >= sqlc.arg(from_time)::timestamptz
      AND b.created_at <  sqlc.arg(to_time)::timestamptz
)
SELECT f.id                                                        AS field_id,
       f.name                                                      AS field_name,
       count(*)                                                    AS bookings,
       count(*) FILTER (WHERE s.status = 'cancelled')               AS cancelled,
       count(*) FILTER (WHERE s.status = 'expired')                 AS expired,
       count(*) FILTER (WHERE s.status = 'confirmed')               AS confirmed,
       count(*) FILTER (WHERE s.status = 'cancelled'
                          AND s.confirmed_at IS NOT NULL)           AS abandoned_after_confirm,
       COALESCE(SUM(s.spot_count) FILTER (WHERE s.status = 'cancelled'), 0)::bigint
                                                                   AS spots_returned,
       COALESCE(SUM(s.total_santim) FILTER (WHERE s.status = 'cancelled'
                                             AND s.confirmed_at IS NOT NULL), 0)::bigint
                                                                   AS refundable_santim
FROM scoped s
JOIN games g ON g.id = s.first_game_id
JOIN fields f ON f.id = g.field_id
GROUP BY f.id, f.name
ORDER BY cancelled DESC, f.name;
