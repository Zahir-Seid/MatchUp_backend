-- +goose Up
-- "Notify me when a spot frees up" (PRD FR29): a user's subscription to a
-- sold-out game. One row per user and game. A row is deleted in the same
-- transaction that hands its alert to the notifier, so a watch fires exactly
-- once; rows disappear with their user or their game.
CREATE TABLE game_watches (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    game_id    uuid        NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, game_id)
);

-- The alert query runs per released game; the unique key's leading column is
-- user_id, so the game side needs its own index.
CREATE INDEX game_watches_game_id_idx ON game_watches (game_id);

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled', 'booking_cancelled',
                    'payment_accounts_updated', 'game_spot_available'));

-- +goose Down
DELETE FROM notifications WHERE type = 'game_spot_available';
ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled', 'booking_cancelled',
                    'payment_accounts_updated'));
DROP TABLE game_watches;
