-- The shared record for one video on one platform, plus each user's tracking of
-- it. A video is fetched once however many people watch it; see docs/dashboard-design.md.

CREATE TABLE videos (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id         uuid        NOT NULL DEFAULT gen_random_uuid(),

    platform          text        NOT NULL
                      CHECK (platform IN ('youtube', 'tiktok', 'meta')),
    platform_video_id text        NOT NULL,
    canonical_url     text        NOT NULL,

    -- Metadata, refreshed on every successful fetch.
    title             text,
    channel_id        text,
    channel_title     text,
    published_at      timestamptz,

    -- Denormalised current values, written in the same transaction as the
    -- snapshot they come from. The dashboard's hot query reads these instead of
    -- reaching into the time series once per tracked video.
    --
    -- Every counter is nullable and none defaults to zero: the platforms omit
    -- counters by not reporting them, and writing 0 would turn a measurement
    -- gap into a false fact.
    latest_view_count    bigint CHECK (latest_view_count    >= 0),
    latest_like_count    bigint CHECK (latest_like_count    >= 0),
    latest_comment_count bigint CHECK (latest_comment_count >= 0),
    latest_share_count   bigint CHECK (latest_share_count   >= 0),
    latest_save_count    bigint CHECK (latest_save_count    >= 0),
    latest_captured_at   timestamptz,

    -- Scheduling state, owned by the poller.
    tracker_count        integer     NOT NULL DEFAULT 0 CHECK (tracker_count >= 0),
    fetch_interval       interval    NOT NULL DEFAULT '6 hours'
                         CHECK (fetch_interval > interval '0'),

    -- NULL means "not scheduled": nobody tracks it, or it has been retired.
    next_fetch_at        timestamptz,

    -- Held by the worker that claimed this row. Expiry is what makes a worker
    -- dying mid-fetch recoverable without any liveness tracking.
    locked_until         timestamptz,

    consecutive_failures integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    last_fetch_at        timestamptz,
    last_fetch_status    text        NOT NULL DEFAULT 'pending'
                         CHECK (last_fetch_status IN
                               ('pending', 'ok', 'not_found', 'blocked', 'error')),
    last_fetch_error     text,

    -- Set only when the platform said, repeatedly and unambiguously, that the
    -- video is gone. History is kept; polling stops.
    unavailable_since    timestamptz,

    first_seen_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT videos_platform_id_key UNIQUE (platform, platform_video_id),
    CONSTRAINT videos_public_id_key   UNIQUE (public_id)
);

-- The poller's claim query. Partial, so the index holds only rows that are
-- actually due however many videos exist.
CREATE INDEX videos_due_idx ON videos (next_fetch_at)
    WHERE next_fetch_at IS NOT NULL AND unavailable_since IS NULL;

CREATE TABLE tracked_videos (
    user_id     bigint      NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    video_id    bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,

    label       text        NOT NULL DEFAULT '',   -- the user's own name for it
    notes       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Untracking archives rather than deletes, so re-adding a video keeps its
    -- history and the label the user gave it.
    archived_at timestamptz,

    PRIMARY KEY (user_id, video_id)
);

-- "Who tracks this video?" — used to recompute tracker_count and to notice when
-- the last tracker leaves.
CREATE INDEX tracked_videos_video_idx ON tracked_videos (video_id)
    WHERE archived_at IS NULL;
