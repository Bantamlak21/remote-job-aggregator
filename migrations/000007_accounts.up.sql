-- Passwordless accounts and job bookmarks.
--
-- A person signs in by opening a single-use link sent to their email. No password
-- exists. Only hashes of secrets are stored: a database leak must not let anyone
-- sign in, so login tokens and session tokens are kept as SHA-256 digests (the
-- tokens themselves are 256 random bits, so a fast hash is enough).

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Lower-case, ASCII-only address (no internationalized domain, punycode or not).
    -- The CHECK keeps two spellings of one
    -- mailbox from becoming two accounts even if a code path forgets to normalize.
    -- Printable ASCII only, no spaces: the application refuses anything else, and the
    -- database refuses it too, so a code path that skips normalization cannot store it.
    email         TEXT NOT NULL UNIQUE
                  CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254 AND email ~ '^[!-~]+$'),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per emailed link. Single use (used_at), short-lived (expires_at).
-- email is not a foreign key: the account is created when the link is used, so
-- a login request never reveals whether the address already has an account.
CREATE TABLE login_tokens (
    token_hash BYTEA PRIMARY KEY CHECK (length(token_hash) = 32),
    email      TEXT NOT NULL CHECK (email = lower(email) AND email ~ '^[!-~]+$'),
    -- The address with its aliases folded (dots and +tags removed for Gmail, +tags
    -- everywhere else): what the per-address rate limit counts, so a+1@x and a+2@x
    -- share one budget. The mailed address stays exactly the one that was typed.
    throttle_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
-- Counting recent requests per address (the per-address rate limit) and per site
-- (the global limit), and deleting an account's links.
CREATE INDEX login_tokens_throttle_created_idx ON login_tokens (throttle_key, created_at);
CREATE INDEX login_tokens_created_idx ON login_tokens (created_at);
CREATE INDEX login_tokens_email_idx ON login_tokens (email);
CREATE INDEX login_tokens_expires_idx ON login_tokens (expires_at);

CREATE TABLE sessions (
    token_hash   BYTEA PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- A saved job. It survives the job closing (the person still wants to see that
-- it closed), and goes when the account or the job row goes.
CREATE TABLE bookmarks (
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_id     BIGINT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, job_id)
);
CREATE INDEX bookmarks_user_created_idx ON bookmarks (user_id, created_at DESC);
