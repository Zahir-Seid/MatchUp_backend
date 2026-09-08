-- +goose Up
-- pgcrypto provides gen_random_uuid() on Postgres < 13; harmless on newer.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- citext gives case-insensitive email uniqueness without a functional index.
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
