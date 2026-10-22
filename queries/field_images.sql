-- name: CreateFieldImage :one
INSERT INTO field_images (
    uploaded_by, data, thumbnail_data, content_type, width, height,
    byte_size, thumbnail_byte_size, data_sha256, thumbnail_sha256, expires_at
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.arg(expires_at)::timestamptz
WHERE (
    SELECT count(*)
    FROM field_images
    WHERE uploaded_by = $1
      AND field_id IS NULL
      AND expires_at > now()
) < sqlc.arg(max_pending)::bigint
RETURNING id, field_id, uploaded_by, content_type, width, height,
          byte_size, thumbnail_byte_size, data_sha256, thumbnail_sha256,
          position, is_primary, expires_at, created_at;

-- name: CountPendingFieldImages :one
SELECT count(*)
FROM field_images
WHERE uploaded_by = $1
  AND field_id IS NULL
  AND expires_at > now();

-- name: GetStagedFieldImage :one
SELECT id, field_id, uploaded_by, content_type, width, height,
       byte_size, thumbnail_byte_size, data_sha256, thumbnail_sha256,
       position, is_primary, expires_at, created_at
FROM field_images
WHERE id = $1
  AND uploaded_by = $2
  AND field_id IS NULL
  AND expires_at > now();

-- name: DeleteStagedFieldImage :execrows
DELETE FROM field_images
WHERE id = $1
  AND uploaded_by = $2
  AND field_id IS NULL;

-- name: DeleteExpiredStagedFieldImages :execrows
DELETE FROM field_images
WHERE field_id IS NULL
  AND expires_at <= now();

-- name: DeleteFieldImagesNotIn :execrows
DELETE FROM field_images
WHERE field_id = sqlc.arg(field_id)
  AND NOT (id = ANY(sqlc.arg(keep_ids)::uuid[]));

-- A field may have only one primary image. The caller clears the old one
-- before choosing the new one, inside the same transaction.
-- name: ClearFieldImagePrimary :exec
UPDATE field_images
SET is_primary = false
WHERE field_id = $1
  AND is_primary;

-- name: PlaceFieldImage :execrows
UPDATE field_images
SET field_id    = sqlc.arg(field_id),
    position    = sqlc.arg(position),
    is_primary  = false,
    expires_at  = NULL
WHERE id = sqlc.arg(id)
  AND (
      field_id = sqlc.arg(field_id)
      OR (
          field_id IS NULL
          AND uploaded_by = sqlc.arg(uploaded_by)
          AND expires_at > now()
      )
  );

-- name: SetFieldImagePrimary :execrows
UPDATE field_images
SET is_primary = true
WHERE id = $1
  AND field_id = $2;

-- name: ListFieldImages :many
SELECT id, field_id, uploaded_by, content_type, width, height,
       byte_size, thumbnail_byte_size, data_sha256, thumbnail_sha256,
       position, is_primary, expires_at, created_at
FROM field_images
WHERE field_id = $1
ORDER BY position, created_at;

-- name: ListFieldImagesForFields :many
SELECT id, field_id, uploaded_by, content_type, width, height,
       byte_size, thumbnail_byte_size, data_sha256, thumbnail_sha256,
       position, is_primary, expires_at, created_at
FROM field_images
WHERE field_id = ANY(sqlc.arg(field_ids)::uuid[])
ORDER BY field_id, position, created_at;

-- name: GetPublicFieldImage :one
SELECT data, content_type, data_sha256
FROM field_images
WHERE field_id = $1
  AND id = $2;

-- name: GetPublicFieldThumbnail :one
SELECT thumbnail_data, content_type, thumbnail_sha256
FROM field_images
WHERE field_id = $1
  AND id = $2;
