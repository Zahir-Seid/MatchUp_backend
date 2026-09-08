-- +goose Up
CREATE TABLE payments (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id              uuid        NOT NULL REFERENCES bookings (id) ON DELETE RESTRICT,
    -- Denormalized from the booking so the idempotency key can be scoped per
    -- user without a join on the hot submission path.
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    method                  text        NOT NULL CHECK (method IN ('cbe', 'telebirr')),
    -- Whatever the payer pasted: a bare CBE FT-reference, a full CBE receipt
    -- URL, a modern CBE token, or a Telebirr reference.
    transaction_reference   text        NOT NULL CHECK (length(btrim(transaction_reference)) > 0),
    -- Trailing digits of the PAYER's own CBE account, needed only to address
    -- the legacy PDF receipt service for a bare FT-reference. Absent for
    -- pasted receipt URLs (which embed it), modern CBE tokens, and Telebirr.
    account_suffix          text        CHECK (account_suffix IS NULL OR account_suffix ~ '^[0-9]{8}$'),
    -- Client-supplied replay guard: a retried submission with the same key
    -- returns the original outcome instead of creating a second payment.
    idempotency_key         text        NOT NULL CHECK (length(btrim(idempotency_key)) > 0),
    -- Snapshot of bookings.total_santim. The amount checked against the
    -- bank is always this, never a client-supplied figure (PRD FR16).
    expected_amount_santim  bigint      NOT NULL CHECK (expected_amount_santim > 0),
    -- What the bank actually reported, once known.
    verified_amount_santim  bigint,
    status                  text        NOT NULL DEFAULT 'pending'
                                        CHECK (status IN ('pending', 'verified', 'mismatch',
                                                          'not_found', 'approved', 'rejected')),
    -- The verifier's own result, kept verbatim so an admin reviewing a
    -- flagged payment sees what the bank said, not our summary of it (PRD J7).
    raw_result              jsonb,
    -- Admin resolution of a flagged payment (PRD FR26).
    resolved_by             uuid        REFERENCES users (id) ON DELETE SET NULL,
    resolved_at             timestamptz,
    resolution_reason       text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- Anti-replay: one transaction reference is spendable exactly once
    -- system-wide (PRD FR17). This is the constraint, not an app-level check.
    CONSTRAINT payments_transaction_reference_key UNIQUE (transaction_reference),
    CONSTRAINT payments_user_idempotency_key UNIQUE (user_id, idempotency_key),
    CONSTRAINT payments_resolution_is_complete
        CHECK ((resolved_at IS NULL AND resolved_by IS NULL AND resolution_reason IS NULL)
            OR (resolved_at IS NOT NULL AND resolution_reason IS NOT NULL))
);

CREATE INDEX payments_booking_id_idx ON payments (booking_id);
-- The admin review queue: flagged payments awaiting a human decision.
CREATE INDEX payments_review_queue_idx ON payments (created_at)
    WHERE status IN ('mismatch', 'not_found');

CREATE TABLE payment_verification_jobs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id      uuid        NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts    integer     NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- When the current claim was taken. A row still 'processing' well past
    -- this is a crashed worker and gets swept back to 'pending'.
    claimed_at      timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- One job per payment: re-submitting means a new payment row, never a
    -- second job racing the first over the same reference.
    CONSTRAINT payment_verification_jobs_payment_id_key UNIQUE (payment_id)
);

-- The worker's claiming query (SDD 13.1).
CREATE INDEX payment_verification_jobs_claim_idx ON payment_verification_jobs (next_attempt_at)
    WHERE status = 'pending';
-- The stuck-job reconciliation sweep.
CREATE INDEX payment_verification_jobs_stuck_idx ON payment_verification_jobs (claimed_at)
    WHERE status = 'processing';

-- +goose Down
DROP TABLE payment_verification_jobs;
DROP TABLE payments;
