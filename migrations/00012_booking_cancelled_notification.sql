-- +goose Up
-- An admin cancelling a paid booking (PRD FR27) has to tell the customer, and
-- none of the existing types says it: 'hold_expired' is about a hold running
-- out on its own, and 'payment_rejected' is about the money. A cancellation
-- decided by a person needs its own copy and its own type so the app can route
-- it and the customer is not told the wrong story.
ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled', 'booking_cancelled'));

-- +goose Down
DELETE FROM notifications WHERE type = 'booking_cancelled';
ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
    CHECK (type IN ('booking_confirmed', 'payment_under_review',
                    'payment_rejected', 'hold_expiring',
                    'hold_expired', 'game_reminder',
                    'game_cancelled'));
