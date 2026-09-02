-- One row per lookup a user performed.
--
-- Append-only by design. A lookup records what a platform reported at a moment
-- in time; re-checking the same URL inserts another row rather than updating
-- this one, so a user's list reads as a history of questions asked and the
-- numbers stay comparable against the timestamp beside them.
--
-- There is deliberately no schedule, no lock and no partitioning here: nothing
-- refreshes these rows in the background, so none of that machinery has a job.

CREATE TABLE lookups (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- The id shown in URLs. Never the bigint: a sequential id in a path tells
    -- any user how many lookups everyone else has made.
    public_id  uuid        NOT NULL DEFAULT gen_random_uuid() UNIQUE,

    user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    platform   text        NOT NULL CHECK (platform IN ('youtube', 'meta', 'tiktok')),
    video_id   text        NOT NULL,
    url        text        NOT NULL,

    title        text        NOT NULL DEFAULT '',
    published_at timestamptz,

    channel_id          text NOT NULL DEFAULT '',
    channel_title       text NOT NULL DEFAULT '',
    channel_url         text NOT NULL DEFAULT '',
    channel_email       text NOT NULL DEFAULT '',
    channel_description text NOT NULL DEFAULT '',

    -- bigint because Postgres has no unsigned type; NULL means the platform
    -- does not report this counter, which is not the same fact as zero.
    view_count    bigint CHECK (view_count    >= 0),
    like_count    bigint CHECK (like_count    >= 0),
    comment_count bigint CHECK (comment_count >= 0),
    share_count   bigint CHECK (share_count   >= 0),
    save_count    bigint CHECK (save_count    >= 0),

    looked_up_at timestamptz NOT NULL DEFAULT now()
);

-- The dashboard's only query: this user's lookups, newest first.
CREATE INDEX lookups_user_recent_idx ON lookups (user_id, looked_up_at DESC);
