-- +goose Up
-- The receiving accounts the platform verifies payments against, editable from
-- the dashboard. Seeded empty on purpose: the real account numbers stay in the
-- environment (and out of the migration files) until an admin saves one here.
-- Empty means "inherit the environment value".
CREATE TABLE payment_settings (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    merchant_cbe_account text NOT NULL DEFAULT ''
        CHECK (merchant_cbe_account = '' OR merchant_cbe_account ~ '^[0-9]{8,20}$'),
    merchant_telebirr_account text NOT NULL DEFAULT ''
        CHECK (merchant_telebirr_account = '' OR merchant_telebirr_account ~ '^[0-9]{8,20}$'),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO payment_settings (id) VALUES (true);

-- A changed receiving account redirects where every future receipt is checked,
-- so it needs its own notification type: the admins are told when it happens.
ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled', 'booking_cancelled',
                    'payment_accounts_updated'));

-- +goose Down
DELETE FROM notifications WHERE type = 'payment_accounts_updated';
ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled', 'booking_cancelled'));

DROP TABLE payment_settings;
