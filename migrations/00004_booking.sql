-- +goose Up
CREATE TABLE bookings (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    booking_type            text        NOT NULL CHECK (booking_type IN ('single', 'monthly_4wk')),
    -- Number of players this booking reserves in EVERY game it covers.
    spot_count              integer     NOT NULL CHECK (spot_count > 0),
    -- How many weekly occurrences this booking covers: 1 for single, 4 for
    -- monthly. Redundant with booking_type today, explicit for reporting.
    week_count              integer     NOT NULL CHECK (week_count > 0),
    -- Price snapshots, in santim, frozen at booking time so a later admin
    -- price edit never retroactively changes what was charged.
    price_per_player_santim bigint      NOT NULL CHECK (price_per_player_santim > 0),
    subtotal_santim         bigint      NOT NULL CHECK (subtotal_santim > 0),
    discount_santim         bigint      NOT NULL DEFAULT 0 CHECK (discount_santim >= 0),
    -- The single server-computed figure any payment is verified against.
    total_santim            bigint      NOT NULL CHECK (total_santim > 0),
    status                  text        NOT NULL DEFAULT 'pending_payment'
                                        CHECK (status IN ('pending_payment', 'verifying', 'confirmed',
                                                          'pending_review', 'expired', 'cancelled')),
    -- Set while the 5-minute hold is live; cleared once the booking reaches
    -- a terminal state.
    hold_expires_at         timestamptz,
    -- Short human-readable code shown in "My Bookings" and quoted to support.
    reference_code          text        NOT NULL UNIQUE,
    confirmed_at            timestamptz,
    cancelled_at            timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT bookings_total_is_subtotal_less_discount
        CHECK (total_santim = subtotal_santim - discount_santim),
    CONSTRAINT bookings_discount_not_above_subtotal
        CHECK (discount_santim < subtotal_santim),
    CONSTRAINT bookings_week_count_matches_type
        CHECK ((booking_type = 'single' AND week_count = 1)
            OR (booking_type = 'monthly_4wk' AND week_count = 4))
);

-- "My Bookings", newest first (SDD 13.1).
CREATE INDEX bookings_user_id_created_at_idx ON bookings (user_id, created_at DESC);
-- The hold-expiry sweep only ever scans live holds, so the index is partial.
CREATE INDEX bookings_hold_expiry_idx ON bookings (hold_expires_at)
    WHERE status = 'pending_payment';

-- Which games a booking reserves: exactly one row for a single booking,
-- four for a monthly one. This join table is what makes the monthly type
-- work without a second reservation code path.
CREATE TABLE booking_games (
    booking_id uuid    NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    game_id    uuid    NOT NULL REFERENCES games (id) ON DELETE RESTRICT,
    -- 1-based position within the booking, ordered by the game's starts_at.
    -- Also the order in which spots are reserved and released, which is what
    -- keeps concurrent monthly bookings from deadlocking against each other.
    week_index integer NOT NULL CHECK (week_index > 0),
    PRIMARY KEY (booking_id, game_id),
    UNIQUE (booking_id, week_index)
);

CREATE INDEX booking_games_game_id_idx ON booking_games (game_id);

-- Named teammates. Only named spots get a row; an unnamed spot is simply
-- absent, so headcount-only bookings store nothing here (PRD FR10).
CREATE TABLE booking_participants (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id uuid        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    slot_index integer     NOT NULL CHECK (slot_index > 0),
    full_name  text        NOT NULL CHECK (length(btrim(full_name)) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (booking_id, slot_index)
);

-- +goose Down
DROP TABLE booking_participants;
DROP TABLE booking_games;
DROP TABLE bookings;
