-- +goose Up
-- Append-only. Nothing in the application ever issues UPDATE or DELETE here;
-- rows are written alongside the sensitive change they describe, inside the
-- same transaction, so an action and its audit trail commit together.
CREATE TABLE audit_logs (
    id            bigserial   PRIMARY KEY,
    -- NULL for actions taken by the system itself (worker expiry sweeps).
    actor_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    action        text        NOT NULL,
    target_type   text        NOT NULL,
    target_id     text        NOT NULL,
    -- Required for admin overrides; the reason is the point of the record.
    reason        text,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_user_id, created_at DESC);
CREATE INDEX audit_logs_target_idx ON audit_logs (target_type, target_id);

-- +goose Down
DROP TABLE audit_logs;
