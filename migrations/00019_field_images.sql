-- +goose Up
-- Images are stored in the database rather than on a CDN for the MVP. The
-- bytes are already re-encoded and stripped of metadata when they arrive here,
-- never the uploaded file as received.
ALTER TABLE fields DROP COLUMN image_url;

CREATE TABLE field_images (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL means the row is a staged upload: it has been validated and
    -- re-encoded, but no field owns it yet, so it is invisible to the public
    -- API and expires unless a field claims it.
    field_id            uuid        REFERENCES fields (id) ON DELETE CASCADE,
    -- The admin who uploaded it. Kept after an account is deleted so the image
    -- does not vanish with the person who happened to upload it.
    uploaded_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    data                bytea       NOT NULL,
    thumbnail_data      bytea       NOT NULL,
    content_type        text        NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png')),
    width               integer     NOT NULL CHECK (width > 0),
    height              integer     NOT NULL CHECK (height > 0),
    byte_size           integer     NOT NULL CHECK (byte_size > 0),
    thumbnail_byte_size integer     NOT NULL CHECK (thumbnail_byte_size > 0),
    data_sha256         text        NOT NULL CHECK (length(data_sha256) = 64),
    thumbnail_sha256    text        NOT NULL CHECK (length(thumbnail_sha256) = 64),
    -- Display order within the field, 0-based.
    position            smallint    NOT NULL DEFAULT 0 CHECK (position BETWEEN 0 AND 4),
    is_primary          boolean     NOT NULL DEFAULT false,
    -- Staged rows carry a deadline; attached rows never do.
    expires_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT field_images_placement CHECK (
        (field_id IS NULL AND is_primary = false AND expires_at IS NOT NULL)
        OR
        (field_id IS NOT NULL AND expires_at IS NULL)
    )
);

-- The gallery read: one field's images in display order.
CREATE INDEX field_images_field_position_idx
    ON field_images (field_id, position)
    WHERE field_id IS NOT NULL;

-- One primary image per field. A field has no image columns of its own, so
-- this is what lets the public field view derive image_url.
CREATE UNIQUE INDEX field_images_one_primary_idx
    ON field_images (field_id)
    WHERE field_id IS NOT NULL AND is_primary;

-- The worker's expiry sweep only looks at staged rows.
CREATE INDEX field_images_staged_expiry_idx
    ON field_images (expires_at)
    WHERE field_id IS NULL;

-- +goose Down
DROP TABLE field_images;
ALTER TABLE fields ADD COLUMN image_url text;
