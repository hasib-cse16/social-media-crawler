-- Extensions the rest of the schema depends on.
--
-- citext gives case-insensitive text, used for users.email. Enforcing that at
-- the database level is the only place it holds: an application-side lower() is
-- one forgotten call away from letting Alice@x.com and alice@x.com become two
-- accounts.
--
-- pgcrypto supplies gen_random_uuid() for the public_id columns. On PostgreSQL
-- 13+ that function is also in core, but requesting the extension keeps this
-- working if the database is older than we assume.
--
-- Both are pinned to `public` rather than installed into whatever schema
-- happens to be first on the search_path. An extension is a database-wide
-- object installed *into* one schema, and CREATE EXTENSION IF NOT EXISTS checks
-- the database, not the schema — so an unqualified create lands the type
-- wherever the first caller's search_path pointed, and every other schema then
-- fails to resolve it. Naming the schema makes the location deliberate instead
-- of dependent on who ran the migration first.

CREATE EXTENSION IF NOT EXISTS citext   WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
