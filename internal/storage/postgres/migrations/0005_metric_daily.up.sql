-- The daily rollup. Charts longer than the raw retention window read this, and
-- it is small enough to keep forever: 1,000 videos x 365 days is 365k rows/year.

CREATE TABLE metric_daily (
    video_id           bigint  NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    day                date    NOT NULL,

    first_view_count   bigint CHECK (first_view_count   >= 0),
    last_view_count    bigint CHECK (last_view_count    >= 0),
    last_like_count    bigint CHECK (last_like_count    >= 0),
    last_comment_count bigint CHECK (last_comment_count >= 0),
    last_share_count   bigint CHECK (last_share_count   >= 0),
    last_save_count    bigint CHECK (last_save_count    >= 0),

    -- last - first. Signed, with no check constraint, because counters are NOT
    -- monotonic: TikTok re-runs bot filtering, YouTube purges invalid views,
    -- Meta corrects aggregation. A negative here is a real measurement, not
    -- corrupt data, and everything downstream has to render it as one.
    view_delta         bigint,

    -- How many raw snapshots this row summarises. A day with one sample has a
    -- meaningless delta, and charts use this to say so rather than drawing a
    -- confident flat line over a gap in coverage.
    sample_count       integer NOT NULL CHECK (sample_count > 0),

    PRIMARY KEY (video_id, day)
);

-- Retention and cross-video "top movers" queries scan by day.
CREATE INDEX metric_daily_day_idx ON metric_daily (day);
