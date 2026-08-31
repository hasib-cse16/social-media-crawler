-- Every fetch attempt, successful or not.
--
-- Reliability differs sharply by platform and the scraping providers fail
-- probabilistically. Without this table "is the TikTok provider degrading?" is
-- unanswerable except by grepping logs; with it, it is one query. Retained 14
-- days, which is long enough to see a trend and short enough to stay small.

CREATE TABLE fetch_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    video_id     bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    platform     text        NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    duration_ms  integer     NOT NULL CHECK (duration_ms >= 0),

    status       text        NOT NULL
                 CHECK (status IN ('ok', 'not_found', 'blocked', 'rate_limited',
                                   'timeout', 'error')),

    -- The domain error code, so failures can be grouped without parsing text.
    error_code   text,
    error_detail text
);

-- "Show me this video's recent attempts", for the per-video page.
CREATE INDEX fetch_attempts_video_idx ON fetch_attempts (video_id, started_at DESC);

-- Retention sweeps, and platform-wide health over a window.
CREATE INDEX fetch_attempts_started_idx ON fetch_attempts (started_at);
