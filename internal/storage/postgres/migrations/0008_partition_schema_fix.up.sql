-- Fix ensure_metric_snapshot_partition to work in a schema other than the one
-- first on the search_path.
--
-- The original checked for an existing partition with to_regclass on a bare
-- name, which resolves against the whole search_path. So a partition of the
-- same name in *any* visible schema satisfied the check, the function returned
-- without creating one beside its own parent, and the next insert failed with
-- "no partition of relation found for row" — while the function had just
-- reported success.
--
-- That is not only a test-isolation problem. Any deployment whose application
-- schema is not the first entry on its search_path, or that runs two databases'
-- worth of schemas side by side, would hit it the moment the month rolled over.
--
-- The fix resolves the parent's own namespace and both checks and creates
-- there, so the partition always lands beside the table it belongs to,
-- whatever the caller's search_path happens to be.
--
-- The calls at the end repair databases created before this migration: the
-- three partitions migration 0004 believed it had created may not exist.

CREATE OR REPLACE FUNCTION ensure_metric_snapshot_partition(target timestamptz)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
    start_at     date := date_trunc('month', target AT TIME ZONE 'UTC')::date;
    end_at       date := (date_trunc('month', target AT TIME ZONE 'UTC') + interval '1 month')::date;
    part         text := 'metric_snapshots_' || to_char(start_at, 'YYYY_MM');
    parent_schema text;
BEGIN
    -- Where the parent actually lives, rather than wherever the caller's
    -- search_path would have put a new table.
    SELECT n.nspname INTO parent_schema
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.oid = 'metric_snapshots'::regclass;

    IF to_regclass(quote_ident(parent_schema) || '.' || quote_ident(part)) IS NULL THEN
        BEGIN
            EXECUTE format(
                'CREATE TABLE %I.%I PARTITION OF %I.metric_snapshots FOR VALUES FROM (%L) TO (%L)',
                parent_schema, part, parent_schema, start_at, end_at);
        EXCEPTION WHEN duplicate_table THEN
            -- Another session created it between the check and here.
            NULL;
        END;
    END IF;

    RETURN part;
END;
$$;

SELECT ensure_metric_snapshot_partition(now());
SELECT ensure_metric_snapshot_partition(now() + interval '1 month');
SELECT ensure_metric_snapshot_partition(now() + interval '2 months');
