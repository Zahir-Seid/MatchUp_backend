-- The dashboard-editable booking policy. One row; see the migration.

-- name: GetBookingSettings :one
SELECT * FROM booking_settings WHERE id;

-- name: UpdateBookingSettings :one
UPDATE booking_settings
SET monthly_discount_bps   = sqlc.arg(monthly_discount_bps),
    monthly_discount_label = sqlc.arg(monthly_discount_label),
    updated_by             = sqlc.narg(updated_by),
    updated_at             = now()
WHERE id
RETURNING *;
