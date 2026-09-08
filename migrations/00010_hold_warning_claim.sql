-- +goose Up

-- The expiry-warning notification (PRD FR21) is emitted by the same sweep that
-- expires holds, which runs every 15-20 seconds. Without a claim marker the
-- sweep would re-send the warning on every tick for the whole warning window.
--
-- Marking the row in the same statement that selects it makes the send
-- exactly-once even if two worker instances run concurrently or one restarts
-- mid-tick, and replaces a per-candidate lookup into notifications with an
-- indexed predicate.
ALTER TABLE bookings ADD COLUMN hold_warning_sent_at timestamptz;

CREATE INDEX bookings_hold_warning_pending_idx ON bookings (hold_expires_at)
    WHERE status = 'pending_payment' AND hold_warning_sent_at IS NULL;

-- +goose Down
DROP INDEX bookings_hold_warning_pending_idx;
ALTER TABLE bookings DROP COLUMN hold_warning_sent_at;
