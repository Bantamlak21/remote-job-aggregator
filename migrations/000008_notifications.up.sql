-- Per-account keyword notification preferences and Telegram delivery.
--
-- A signed-in person picks keywords (from the same phrase vocabulary the role
-- classifier already uses, e.g. "golang", "backend" -- see
-- internal/filtering/relevance.RelevantVocabulary) and links a Telegram chat.
-- The scheduler then DMs them when a newly ingested, open job's title matches
-- one of their keywords.

-- One row per account per chosen keyword. The keyword itself is validated
-- against the vocabulary in Go, not here: the vocabulary lives in
-- profile.json, not the database, and can change without a migration.
CREATE TABLE notification_keywords (
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    keyword    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, keyword)
);

-- One row per requested Telegram link. Single use (used_at),
-- short-lived (expires_at). Same shape as login_tokens, and for the same
-- reason: a signed-in person requests a code, opens Telegram, sends
-- "/start <code>" to the bot, and the bot must map that code back to their
-- account without the code itself being useful to anyone who merely sees it
-- in transit.
CREATE TABLE telegram_link_tokens (
    token_hash BYTEA PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
CREATE INDEX telegram_link_tokens_user_idx ON telegram_link_tokens (user_id);
CREATE INDEX telegram_link_tokens_expires_idx ON telegram_link_tokens (expires_at);

-- The confirmed link: one Telegram chat per account, and chat_id is UNIQUE so
-- one Telegram chat can never end up mapped to two accounts.
CREATE TABLE telegram_links (
    user_id    BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    chat_id    BIGINT NOT NULL UNIQUE,
    linked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency for delivery: a job already notified for an account is never
-- sent twice, and a send that fails leaves no row here, so the next
-- scheduler tick retries it for free.
CREATE TABLE notified_jobs (
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_id      BIGINT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    notified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, job_id)
);
