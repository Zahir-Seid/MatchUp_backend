-- +goose Up
-- Pricing settings the dashboard can change without a deploy. A single row
-- (pinned by the check on id), so every reader sees the same values the admin
-- last saved. Bookings snapshot their own figures at creation, so an edit never
-- changes what an existing booking owes.
CREATE TABLE booking_settings (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    -- Monthly-plan discount in basis points of the subtotal, 0–99.99%.
    -- 1250 (12.5%) is the launch value: half of one week on a four-week plan.
    -- The cap stays below 100% because a free booking is a pricing bug, not a
    -- promotion; the bookings CHECK (discount < subtotal) enforces the rest.
    monthly_discount_bps integer NOT NULL DEFAULT 1250
        CHECK (monthly_discount_bps BETWEEN 0 AND 9999),
    monthly_discount_label text NOT NULL DEFAULT 'Half a session free on the monthly plan'
        CHECK (char_length(monthly_discount_label) BETWEEN 1 AND 120),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO booking_settings (id) VALUES (true);

-- +goose Down
DROP TABLE booking_settings;
