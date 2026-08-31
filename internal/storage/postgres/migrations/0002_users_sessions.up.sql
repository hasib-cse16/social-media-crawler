-- Accounts and server-side sessions.

CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id     uuid        NOT NULL DEFAULT gen_random_uuid(),

    -- citext, so Alice@x.com and alice@x.com cannot become two accounts.
    -- Enforcing case-insensitivity here is the only place it holds: an
    -- application-side lower() is one forgotten call away from a duplicate.
    email         citext      NOT NULL,

    -- The full argon2id encoding, parameters included, so the cost can be
    -- raised later without invalidating existing accounts.
    password_hash text        NOT NULL,

    display_name  text        NOT NULL DEFAULT '',

    -- Display only. Nothing is ever stored in a local time.
    timezone      text        NOT NULL DEFAULT 'UTC',

    status        text        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'suspended')),

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,

    CONSTRAINT users_email_key     UNIQUE (email),
    CONSTRAINT users_public_id_key UNIQUE (public_id),
    CONSTRAINT users_email_shaped  CHECK (email LIKE '%_@_%')
);

CREATE TABLE sessions (
    -- The primary key is the sha256 of the opaque token, never the token. A
    -- database dump therefore does not hand the reader a set of live sessions.
    token_hash   bytea       PRIMARY KEY CHECK (octet_length(token_hash) = 32),

    user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Idle timeout is measured from here; absolute expiry from expires_at.
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,

    user_agent   text        NOT NULL DEFAULT '',
    ip           inet
);

-- "Log out everywhere" and cascade deletes.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- The reaper sweeps by expiry.
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
