-- Idempotent replay: a resubmission carrying a key this user already used
-- returns the original payment instead of creating a second one.
-- name: GetPaymentByIdempotencyKey :one
SELECT * FROM payments WHERE user_id = $1 AND idempotency_key = $2;

-- Part B of the expiry/verification race fix, and the concurrent-submission
-- guard in one statement. The UPDATE is the gate: exactly one transaction can
-- move a booking out of 'pending_payment', so a second submission -- or one
-- arriving after the sweep already expired the booking -- inserts nothing and
-- returns zero rows. expected_amount_santim is read from the booking row, so
-- the client cannot influence the amount that gets verified (PRD FR16).
-- name: BeginPaymentVerification :one
WITH claimed AS (
    UPDATE bookings b
    SET status = 'verifying'
    WHERE b.id = sqlc.arg(booking_id)
      AND b.user_id = sqlc.arg(user_id)
      AND b.status = 'pending_payment'
    RETURNING b.id AS claimed_booking_id, b.total_santim AS claimed_total_santim
)
INSERT INTO payments (
    booking_id, user_id, method, transaction_reference,
    account_suffix, idempotency_key, expected_amount_santim
)
SELECT c.claimed_booking_id, sqlc.arg(user_id), sqlc.arg(method),
       sqlc.arg(transaction_reference), sqlc.narg(account_suffix),
       sqlc.arg(idempotency_key), c.claimed_total_santim
FROM claimed c
RETURNING *;

-- name: CreateVerificationJob :one
-- The retry budget is written onto the job rather than left to the column
-- default: how many times a bank lookup is worth repeating is an operational
-- judgement, and a job already queued keeps the budget it was created with, so
-- changing the setting cannot strand or resurrect work that is already in
-- flight.
INSERT INTO payment_verification_jobs (payment_id, max_attempts)
VALUES ($1, sqlc.arg(max_attempts)::int)
RETURNING *;

-- name: GetPayment :one
SELECT * FROM payments WHERE id = $1;

-- name: ListPaymentsForBooking :many
SELECT * FROM payments WHERE booking_id = $1 ORDER BY created_at DESC;

-- Claim and mark in a single statement so no window exists where a row is
-- selected but not yet owned. SKIP LOCKED lets several workers claim disjoint
-- batches (SDD 9.2). attempts increments at claim time, not on failure, so a
-- worker that dies mid-call still burns an attempt and a poison job cannot
-- spin forever.
-- name: ClaimVerificationJobs :many
WITH claimable AS (
    SELECT id FROM payment_verification_jobs
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    FOR UPDATE SKIP LOCKED
    LIMIT sqlc.arg(batch_size)::int
)
UPDATE payment_verification_jobs j
SET status     = 'processing',
    claimed_at = now(),
    attempts   = j.attempts + 1
FROM claimable c
WHERE j.id = c.id
RETURNING j.*;

-- Everything the worker needs to run one verification, in one round trip.
-- name: ListVerificationContexts :many
SELECT p.id            AS payment_id,
       p.booking_id,
       p.user_id,
       p.method,
       p.transaction_reference,
       p.account_suffix,
       p.expected_amount_santim,
       b.reference_code
FROM payments p
JOIN bookings b ON b.id = p.booking_id
WHERE p.id = ANY(sqlc.arg(payment_ids)::uuid[]);

-- name: CompleteVerificationJob :exec
UPDATE payment_verification_jobs
SET status = 'completed', claimed_at = NULL, last_error = NULL
WHERE id = $1;

-- A transport failure goes back into the queue with backoff. Once attempts
-- reach max_attempts the job fails permanently; the caller is responsible for
-- parking the booking in the admin review queue so it never lands in a void.
-- name: RescheduleVerificationJob :one
UPDATE payment_verification_jobs
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    next_attempt_at = now() + sqlc.arg(backoff)::interval,
    claimed_at      = NULL,
    last_error      = sqlc.arg(last_error)
WHERE id = $1
RETURNING *;

-- Crash recovery: a claim older than the worker's maximum run time means the
-- process died holding it. Returned to 'pending' for another attempt
-- (SDD appendix C).
-- name: ReclaimStuckVerificationJobs :many
UPDATE payment_verification_jobs
SET status          = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    next_attempt_at = now(),
    claimed_at      = NULL,
    last_error      = 'reclaimed after stalled processing'
WHERE status = 'processing'
  AND claimed_at < now() - sqlc.arg(stale_after)::interval
RETURNING *;

-- name: SetPaymentOutcome :one
UPDATE payments
SET status                 = sqlc.arg(status),
    verified_amount_santim = sqlc.narg(verified_amount_santim),
    raw_result             = sqlc.narg(raw_result)
WHERE id = sqlc.arg(id) AND status = 'pending'
RETURNING *;

-- The admin review queue (PRD FR26, SDD 7.5).
-- name: ListPaymentReviewQueue :many
SELECT sqlc.embed(p), b.reference_code, b.total_santim, b.booking_type,
       u.full_name AS payer_name, u.email AS payer_email
FROM payments p
JOIN bookings b ON b.id = p.booking_id
JOIN users u    ON u.id = p.user_id
WHERE p.status IN ('mismatch', 'not_found')
ORDER BY p.created_at
LIMIT $1 OFFSET $2;

-- name: CountPaymentReviewQueue :one
SELECT count(*) FROM payments WHERE status IN ('mismatch', 'not_found');

-- Admin approve/reject. A reason is mandatory -- the record exists to explain
-- the override, not merely to note that one happened (PRD FR26).
-- name: ResolvePayment :one
UPDATE payments
SET status            = sqlc.arg(status),
    resolved_by       = sqlc.arg(resolved_by),
    resolved_at       = now(),
    resolution_reason = sqlc.arg(resolution_reason)
WHERE id = sqlc.arg(id)
  AND status IN ('mismatch', 'not_found')
  AND sqlc.arg(status)::text IN ('approved', 'rejected')
RETURNING *;
