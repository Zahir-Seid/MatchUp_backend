-- +goose Up

-- +goose StatementBegin
-- The one primitive that enforces capacity. Both booking types call it: a
-- single booking calls it once, a monthly booking calls it four times inside
-- the same transaction. Returns false when the game lacks p_amount free
-- spots, which the caller turns into a rollback -- never a partial
-- reservation (SDD 9.1).
--
-- The conditional UPDATE is the whole mechanism: Postgres serializes two
-- concurrent updates of the same games row, so the loser re-evaluates
-- spots_remaining >= p_amount against the winner's committed value and
-- matches zero rows. There is no read-then-write window to lose.
CREATE OR REPLACE FUNCTION fn_reserve_game_spots(p_game_id uuid, p_amount integer)
RETURNS boolean AS $$
DECLARE
    v_updated integer;
BEGIN
    IF p_amount <= 0 THEN
        RAISE EXCEPTION 'fn_reserve_game_spots: amount must be positive, got %', p_amount;
    END IF;

    UPDATE games
    SET spots_remaining = spots_remaining - p_amount,
        updated_at      = now()
    WHERE id = p_game_id
      AND status = 'scheduled'
      AND spots_remaining >= p_amount;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    RETURN v_updated = 1;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
-- Returns spots to a game when a hold expires or a booking is cancelled.
-- Refuses to push spots_remaining above capacity, so a double release is a
-- false return rather than corrupted inventory.
CREATE OR REPLACE FUNCTION fn_release_game_spots(p_game_id uuid, p_amount integer)
RETURNS boolean AS $$
DECLARE
    v_updated integer;
BEGIN
    IF p_amount <= 0 THEN
        RAISE EXCEPTION 'fn_release_game_spots: amount must be positive, got %', p_amount;
    END IF;

    UPDATE games
    SET spots_remaining = spots_remaining + p_amount,
        updated_at      = now()
    WHERE id = p_game_id
      AND spots_remaining + p_amount <= capacity;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    RETURN v_updated = 1;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_set_updated_at()
RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER fields_set_updated_at BEFORE UPDATE ON fields
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER games_set_updated_at BEFORE UPDATE ON games
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER bookings_set_updated_at BEFORE UPDATE ON bookings
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER payments_set_updated_at BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER payment_verification_jobs_set_updated_at BEFORE UPDATE ON payment_verification_jobs
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();
CREATE TRIGGER notification_deliveries_set_updated_at BEFORE UPDATE ON notification_deliveries
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();

-- +goose Down
DROP TRIGGER notification_deliveries_set_updated_at ON notification_deliveries;
DROP TRIGGER payment_verification_jobs_set_updated_at ON payment_verification_jobs;
DROP TRIGGER payments_set_updated_at ON payments;
DROP TRIGGER bookings_set_updated_at ON bookings;
DROP TRIGGER games_set_updated_at ON games;
DROP TRIGGER fields_set_updated_at ON fields;
DROP TRIGGER users_set_updated_at ON users;
DROP FUNCTION fn_set_updated_at();
DROP FUNCTION fn_release_game_spots(uuid, integer);
DROP FUNCTION fn_reserve_game_spots(uuid, integer);
