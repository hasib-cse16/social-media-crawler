-- Extensions the rest of the schema depends on.
--
-- citext gives case-insensitive text, used for users.email. Enforcing that at
-- the database level is the only place it holds: application-side lowercasing
-- is one forgotten call away from letting Alice@x.com and alice@x.com become
-- two accounts.
--
-- pgcrypto supplies gen_random_uuid() for the public_id columns. On PostgreSQL
-- 13+ that function is also in core, but requesting the extension keeps this
-- working if the database is older than we assume.

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
