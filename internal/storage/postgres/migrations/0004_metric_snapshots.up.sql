-- The raw time series, partitioned by month.
--
-- Partitioning is an up-front cost taken deliberately, and the reason is
-- retention. Raw snapshots live 30 days and are rolled up into metric_daily
-- after that, so expiring a month has to happen every month forever. As a
-- partition that is DROP TABLE: instant, no bloat, no vacuum. Unpartitioned it
-- is a DELETE of millions of rows that leaves the table bloated and generates
-- enough WAL to be noticed. Converting a large table to partitioned later is a
-- migration nobody enjoys, so we pay now.
--
-- There is deliberately no DEFAULT partition. A default would stop inserts
-- failing when a partition is missing, but it also makes creating the real
-- partition later fail until the misplaced rows are moved by hand -- trading a
-- loud, immediately fixable error for a quiet one that is worse. Instead
-- partitions are created ahead of time, and the writer creates one on demand if
-- it ever finds itself without one.

CREATE TABLE metric_snapshots (
    video_id      bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    captured_at   timestamptz NOT NULL,

    view_count    bigint CHECK (view_count    >= 0),
    like_count    bigint CHECK (like_count    >= 0),
    comment_count bigint CHECK (comment_count >= 0),
    share_count   bigint CHECK (share_count   >= 0),
    save_count    bigint CHECK (save_count    >= 0),

    -- Also exactly the index the history chart wants: every snapshot for one
    -- video between two times is a single range scan.
    PRIMARY KEY (video_id, captured_at)
) PARTITION BY RANGE (captured_at);

-- BRIN rather than btree: the rollup and retention jobs scan by time, rows
-- arrive in time order, and BRIN costs a fraction of the space.
CREATE INDEX metric_snapshots_captured_brin
    ON metric_snapshots USING brin (captured_at);

-- ensure_metric_snapshot_partition creates the partition covering `target`'s
-- month if it does not exist, and returns its name.
--
-- It lives in SQL rather than Go so that creation is a single atomic statement
-- callable from anywhere, including from the writer's retry path. The exception
-- handler covers the race between the existence check and the CREATE, which two
-- workers crossing a month boundary at the same moment will otherwise lose.
CREATE OR REPLACE FUNCTION ensure_metric_snapshot_partition(target timestamptz)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
    start_at date := date_trunc('month', target AT TIME ZONE 'UTC')::date;
    end_at   date := (date_trunc('month', target AT TIME ZONE 'UTC') + interval '1 month')::date;
    part     text := 'metric_snapshots_' || to_char(start_at, 'YYYY_MM');
BEGIN
    IF to_regclass(part) IS NULL THEN
        BEGIN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF metric_snapshots FOR VALUES FROM (%L) TO (%L)',
                part, start_at, end_at);
        EXCEPTION WHEN duplicate_table THEN
            -- Another session created it between the check and here.
            NULL;
        END;
    END IF;
    RETURN part;
END;
$$;

-- The current month plus two ahead, so a deployment that never runs the
-- housekeeping job still has somewhere to write for a while.
SELECT ensure_metric_snapshot_partition(now());
SELECT ensure_metric_snapshot_partition(now() + interval '1 month');
SELECT ensure_metric_snapshot_partition(now() + interval '2 months');
