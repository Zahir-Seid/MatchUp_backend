-- +goose Up

-- Transactional email (password reset, email verification) is deliberately
-- kept out of the notifications table: notifications is the in-app feed the
-- mobile client renders, and a password reset link has no place there. The
-- outbox reuses the same claim/retry shape as the other queues so a Resend
-- outage delays a reset mail instead of losing it.
--
-- Security note: a pending row holds the plaintext token needed to complete
-- the action it describes, so payload is scrubbed the moment the send
-- succeeds and the tokens themselves are short-lived. Nothing in this table
-- is readable long enough to be useful to someone who later gains database
-- read access.
CREATE TABLE email_outbox (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Nullable: the recipient may delete their account before the send.
    user_id         uuid        REFERENCES users (id) ON DELETE SET NULL,
    to_address      text        NOT NULL,
    template        text        NOT NULL CHECK (template IN ('password_reset', 'email_verification')),
    -- Template variables, including any single-use token.
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'processing', 'sent', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts    integer     NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX email_outbox_claim_idx ON email_outbox (next_attempt_at) WHERE status = 'pending';
CREATE INDEX email_outbox_stuck_idx ON email_outbox (claimed_at) WHERE status = 'processing';

CREATE TRIGGER email_outbox_set_updated_at
    BEFORE UPDATE ON email_outbox
    FOR EACH ROW EXECUTE FUNCTION fn_set_updated_at();

-- +goose Down
DROP TABLE email_outbox;
