-- +goose Up
CREATE TABLE notifications (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       text        NOT NULL CHECK (type IN ('booking_confirmed', 'payment_under_review',
                                                    'payment_rejected', 'hold_expiring',
                                                    'hold_expired', 'game_reminder',
                                                    'game_cancelled')),
    title      text        NOT NULL,
    body       text        NOT NULL,
    -- Deep-link data for the mobile app (booking id, game id, ...).
    payload    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The in-app feed (PRD FR22), newest first.
CREATE INDEX notifications_user_id_created_at_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

-- FCM registration tokens. One row per device; pruned when FCM reports a
-- token as unregistered (SDD 11.2).
CREATE TABLE device_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    fcm_token    text        NOT NULL UNIQUE,
    platform     text        NOT NULL CHECK (platform IN ('ios', 'android')),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX device_tokens_user_id_idx ON device_tokens (user_id);

-- Outbound dispatch attempts, one row per channel per notification. Kept
-- separate from notifications so push and email retry independently and a
-- failure on either never blocks the state change that produced it
-- (SDD 8.5). Claimed with the same SKIP LOCKED pattern as payment jobs.
CREATE TABLE notification_deliveries (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id uuid        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    channel         text        NOT NULL CHECK (channel IN ('push', 'email')),
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'processing', 'sent', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts    integer     NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (notification_id, channel)
);

CREATE INDEX notification_deliveries_claim_idx ON notification_deliveries (next_attempt_at)
    WHERE status = 'pending';
CREATE INDEX notification_deliveries_stuck_idx ON notification_deliveries (claimed_at)
    WHERE status = 'processing';

-- +goose Down
DROP TABLE notification_deliveries;
DROP TABLE device_tokens;
DROP TABLE notifications;
