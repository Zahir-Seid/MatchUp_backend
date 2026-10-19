-- +goose Up
-- Push notifications are gone: FCM is no longer wired into the worker, so a
-- queued push delivery could never be sent and a device token addresses nothing.
-- The feed and email are the only surfaces now.
--
-- Unconditional, not guarded: these rows are debris once the channel is
-- retired, and there is no state worth preserving in them.
DELETE FROM notification_deliveries WHERE channel = 'push';

ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_channel_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_channel_check
    CHECK (channel IN ('email'));

DROP TABLE device_tokens;

-- +goose Down
-- The schema is restored, but the rows the Up deleted are not: an unsendable
-- push delivery has no state worth restoring, and a device token is
-- re-registered by the app on its next launch anyway.
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_channel_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_channel_check
    CHECK (channel IN ('push', 'email'));

CREATE TABLE device_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    fcm_token    text        NOT NULL UNIQUE,
    platform     text        NOT NULL CHECK (platform IN ('ios', 'android')),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX device_tokens_user_id_idx ON device_tokens (user_id);
