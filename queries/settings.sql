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

-- The dashboard-editable receiving accounts. One row; see the migration.

-- name: GetPaymentSettings :one
SELECT * FROM payment_settings WHERE id;

-- name: UpdatePaymentSettings :one
UPDATE payment_settings
SET merchant_cbe_account      = sqlc.arg(merchant_cbe_account),
    merchant_telebirr_account = sqlc.arg(merchant_telebirr_account),
    updated_by                = sqlc.narg(updated_by),
    updated_at                = now()
WHERE id
RETURNING *;

-- The other admins to alert when the receiving accounts change: everyone with
-- the admin role except the actor, who already knows.
-- name: ListAdminUserIDs :many
SELECT id FROM users WHERE role = 'admin' AND id <> sqlc.arg(exclude_id)::uuid ORDER BY id;
