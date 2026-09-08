-- +goose Up
CREATE TABLE fields (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (length(btrim(name)) > 0),
    location   text        NOT NULL CHECK (length(btrim(location)) > 0),
    -- A plain static URL pasted by an admin, not an upload (SDD 11.3).
    image_url  text,
    is_active  boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE games (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    field_id                uuid        NOT NULL REFERENCES fields (id) ON DELETE RESTRICT,
    -- Shared by every weekly occurrence of one recurring slot. NULL for
    -- one-off games. Monthly bookings resolve their four target games by
    -- walking this column forward in starts_at order.
    series_id               uuid,
    starts_at               timestamptz NOT NULL,
    duration_minutes        integer     NOT NULL DEFAULT 60 CHECK (duration_minutes > 0),
    format                  text        NOT NULL CHECK (format IN ('5_a_side', '7_a_side')),
    -- All money is stored in santim (1 ETB = 100 santim) as an exact integer.
    -- Floating point is used only at the verifier boundary, which takes ETB.
    price_per_player_santim bigint      NOT NULL CHECK (price_per_player_santim > 0),
    -- What this slot costs the business, for real-margin reporting (PRD 3).
    -- Nullable because tracking it is still an open product decision (PRD 17).
    cost_to_business_santim bigint      CHECK (cost_to_business_santim IS NULL OR cost_to_business_santim >= 0),
    capacity                integer     NOT NULL CHECK (capacity > 0),
    -- Denormalized live counter. This column, not a COUNT(*) over bookings,
    -- is the capacity source of truth: fn_reserve_game_spots decrements it
    -- conditionally so overbooking is impossible (SDD 9.1).
    spots_remaining         integer     NOT NULL CHECK (spots_remaining >= 0),
    status                  text        NOT NULL DEFAULT 'scheduled'
                                        CHECK (status IN ('scheduled', 'cancelled', 'completed')),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT games_spots_within_capacity CHECK (spots_remaining <= capacity)
);

-- Discovery and filtering by field/date (SDD 13.1).
CREATE INDEX games_field_id_starts_at_idx ON games (field_id, starts_at);
-- Resolving the next N occurrences of a recurring slot for monthly bookings.
CREATE INDEX games_series_id_starts_at_idx ON games (series_id, starts_at) WHERE series_id IS NOT NULL;
-- The default public listing: upcoming, still-scheduled games.
CREATE INDEX games_open_starts_at_idx ON games (starts_at) WHERE status = 'scheduled';
-- One occurrence per series per instant; guards double-created recurrences.
CREATE UNIQUE INDEX games_series_slot_uniq ON games (series_id, starts_at) WHERE series_id IS NOT NULL;

-- +goose Down
DROP TABLE games;
DROP TABLE fields;
