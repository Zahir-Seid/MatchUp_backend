-- +goose Up
CREATE TABLE users (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name        text        NOT NULL CHECK (length(btrim(full_name)) > 0),
    email            citext      NOT NULL UNIQUE,
    -- NULL for accounts created through Google Sign-In that never set a password.
    password_hash    text,
    age              integer     CHECK (age IS NULL OR (age BETWEEN 10 AND 120)),
    -- Contact-only field (PRD FR1); never an authentication factor.
    phone            text,
    role             text        NOT NULL DEFAULT 'player' CHECK (role IN ('player', 'admin')),
    status           text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- Non-blocking: there is no email-verification gate at signup (PRD FR5a).
    email_verified_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oauth_identities (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text        NOT NULL CHECK (provider IN ('google')),
    -- Google's stable 'sub' claim, not the email: emails can be reassigned.
    provider_subject text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subject),
    UNIQUE (user_id, provider)
);

CREATE TABLE refresh_tokens (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the opaque token; the plaintext never touches the database.
    token_hash  bytea       NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    -- Set when this token was rotated out, so a replayed old token can be
    -- detected as reuse rather than merely "revoked".
    rotated_to  uuid        REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    user_agent  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);

CREATE TABLE password_reset_tokens (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX password_reset_tokens_user_id_idx ON password_reset_tokens (user_id);

-- +goose Down
DROP TABLE password_reset_tokens;
DROP TABLE refresh_tokens;
DROP TABLE oauth_identities;
DROP TABLE users;
