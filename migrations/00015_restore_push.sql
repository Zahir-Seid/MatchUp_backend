-- +goose Up
-- Push is back: the worker talks to FCM again, so a queued 'push' delivery has
-- a sender and a device token addresses a real device. 00013 deleted the rows
-- and dropped the table; only the schema returns. The app re-registers its
-- token on the next launch, and an unsendable delivery had no state worth
-- keeping.
ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_channel_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_channel_check
    CHECK (channel IN ('email', 'push'));

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

-- +goose Down
-- The push channel is rolled back the same way 00013 retired it: queued push
-- deliveries are deleted first, because the narrowed CHECK could not hold
-- them and no process could send them.
DELETE FROM notification_deliveries WHERE channel = 'push';

ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_channel_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_channel_check
    CHECK (channel IN ('email'));

DROP TABLE device_tokens;
