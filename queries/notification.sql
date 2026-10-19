-- name: CreateNotification :one
INSERT INTO notifications (user_id, type, title, body, payload)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateNotificationDeliveries :exec
INSERT INTO notification_deliveries (notification_id, channel)
SELECT sqlc.arg(notification_id), t.channel
FROM unnest(sqlc.arg(channels)::text[]) AS t(channel)
ON CONFLICT (notification_id, channel) DO NOTHING;

-- name: ListNotificationsForUser :many
SELECT * FROM notifications
WHERE user_id = $1
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountUnreadNotifications :one
SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL;

-- name: MarkNotificationRead :one
UPDATE notifications SET read_at = now()
WHERE id = $1 AND user_id = $2 AND read_at IS NULL
RETURNING *;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL;

-- Same claim-and-mark shape as the verification queue, so a delivery retries
-- independently of the state change that queued it.
-- name: ClaimNotificationDeliveries :many
WITH claimable AS (
    SELECT id FROM notification_deliveries
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    FOR UPDATE SKIP LOCKED
    LIMIT sqlc.arg(batch_size)::int
)
UPDATE notification_deliveries d
SET status     = 'processing',
    claimed_at = now(),
    attempts   = d.attempts + 1
FROM claimable c
WHERE d.id = c.id
RETURNING d.*;

-- name: ListDeliveryContexts :many
SELECT d.id           AS delivery_id,
       d.channel,
       sqlc.embed(n),
       u.email        AS user_email,
       u.full_name    AS user_full_name
FROM notification_deliveries d
JOIN notifications n ON n.id = d.notification_id
JOIN users u         ON u.id = n.user_id
WHERE d.id = ANY(sqlc.arg(delivery_ids)::uuid[]);

-- A rejection the provider will repeat for every attempt (a malformed address,
-- a body it refuses) is failed outright rather than retried on the backoff
-- curve, so last_error records the real reason instead of a timeout.
-- name: FailDelivery :exec
UPDATE notification_deliveries
SET status = 'failed', claimed_at = NULL, last_error = sqlc.arg(last_error)
WHERE id = $1;

-- name: MarkDeliverySent :exec
UPDATE notification_deliveries
SET status = 'sent', claimed_at = NULL, last_error = NULL
WHERE id = $1;

-- name: RescheduleDelivery :exec
UPDATE notification_deliveries
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    next_attempt_at = now() + sqlc.arg(backoff)::interval,
    claimed_at      = NULL,
    last_error      = sqlc.arg(last_error)
WHERE id = $1;

-- name: ReclaimStuckDeliveries :execrows
UPDATE notification_deliveries
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    next_attempt_at = now(),
    claimed_at      = NULL,
    last_error      = 'reclaimed after stalled processing'
WHERE status = 'processing' AND claimed_at < now() - sqlc.arg(stale_after)::interval;

-- Transactional email queue. Same claim/retry contract as the other queues.

-- name: EnqueueEmail :one
INSERT INTO email_outbox (user_id, to_address, template, payload)
VALUES (sqlc.narg(user_id), sqlc.arg(to_address), sqlc.arg(template), sqlc.arg(payload))
RETURNING *;

-- name: ClaimEmails :many
WITH claimable AS (
    SELECT id
    FROM email_outbox
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE email_outbox e
SET status = 'processing', claimed_at = now(), attempts = e.attempts + 1
FROM claimable c
WHERE e.id = c.id
RETURNING e.*;

-- Success also scrubs the payload: a sent reset mail must not leave its
-- single-use token sitting in the table.
-- name: MarkEmailSent :exec
UPDATE email_outbox
SET status = 'sent', payload = '{}'::jsonb, claimed_at = NULL, last_error = NULL
WHERE id = sqlc.arg(id);

-- A rejection the provider will repeat for every attempt is failed outright
-- rather than retried on the backoff curve. The payload is scrubbed here too:
-- an undeliverable reset mail still holds a plaintext token, and
-- password_reset_tokens keeps only its hash, so the outbox is the one place it
-- would survive.
-- name: FailEmail :exec
UPDATE email_outbox
SET status     = 'failed',
    payload    = '{}'::jsonb,
    claimed_at = NULL,
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id);

-- name: RescheduleEmail :exec
UPDATE email_outbox
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    payload         = CASE WHEN attempts >= max_attempts THEN '{}'::jsonb ELSE payload END,
    claimed_at      = NULL,
    next_attempt_at = now() + sqlc.arg(backoff)::interval,
    last_error      = sqlc.arg(last_error)
WHERE id = sqlc.arg(id);

-- A claim older than the worker's maximum run time means the process died
-- holding it. Mirrors the other queues: retried unless the attempt budget is
-- already spent.
-- name: ReclaimStuckEmails :execrows
UPDATE email_outbox
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    payload         = CASE WHEN attempts >= max_attempts THEN '{}'::jsonb ELSE payload END,
    next_attempt_at = now(),
    claimed_at      = NULL,
    last_error      = 'reclaimed after stalled processing'
WHERE status = 'processing' AND claimed_at < now() - sqlc.arg(stale_after)::interval;
