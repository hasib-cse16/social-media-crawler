-- Token buckets for rate limiting.
--
-- In Postgres rather than in each process's memory, because the limit has to
-- hold across replicas: an in-memory bucket lets an attacker get N attempts per
-- replica, and the fleet size is not a security parameter anyone intended to
-- choose. It is a small, hot table — one row per subject being limited — and
-- the rows are disposable, so losing them to a failover costs nothing worse
-- than a few extra allowed attempts.

CREATE TABLE rate_limit_buckets (
    -- What is being limited ('login_email', 'login_ip'), kept separate from the
    -- subject so one subject can be limited by several rules independently.
    scope       text        NOT NULL,
    subject     text        NOT NULL,

    -- real, not integer: a bucket refills continuously, and rounding to whole
    -- tokens on every take would either leak allowance or never refill at all
    -- for slow rates.
    tokens      real        NOT NULL,
    refilled_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (scope, subject)
);

-- Housekeeping sweeps by age.
CREATE INDEX rate_limit_buckets_refilled_idx ON rate_limit_buckets (refilled_at);

-- take_rate_limit_token removes one token from a bucket, refilling it first for
-- the time that has passed.
--
-- The whole read-modify-write is one INSERT ... ON CONFLICT DO UPDATE so that
-- concurrent attempts cannot both read the same "1 token left" and both spend
-- it. The conflict path takes a row lock, which is what serialises them; doing
-- this as SELECT-then-UPDATE in Go would be a race, and the race is exactly the
-- burst an attacker produces.
--
-- Returns whether the take succeeded, how many tokens are left, and — when
-- denied — how long until one is available, so the caller can send a truthful
-- Retry-After rather than a guess.
CREATE OR REPLACE FUNCTION take_rate_limit_token(
    p_scope    text,
    p_subject  text,
    p_capacity real,
    p_refill_per_second real
)
RETURNS TABLE (allowed boolean, remaining real, retry_after double precision)
LANGUAGE plpgsql
AS $$
DECLARE
    left_over real;
    available real;
BEGIN
    INSERT INTO rate_limit_buckets AS b (scope, subject, tokens, refilled_at)
    VALUES (p_scope, p_subject, p_capacity - 1, now())
    ON CONFLICT (scope, subject) DO UPDATE
    SET tokens = least(
            p_capacity,
            b.tokens + extract(epoch FROM now() - b.refilled_at)::real * p_refill_per_second
        ) - 1,
        refilled_at = now()
    WHERE least(
            p_capacity,
            b.tokens + extract(epoch FROM now() - b.refilled_at)::real * p_refill_per_second
        ) >= 1
    RETURNING b.tokens INTO left_over;

    IF FOUND THEN
        RETURN QUERY SELECT true, left_over, 0::double precision;
        RETURN;
    END IF;

    -- Denied. Work out when the bucket next holds a whole token.
    SELECT least(p_capacity, b.tokens + extract(epoch FROM now() - b.refilled_at)::real * p_refill_per_second)
    INTO available
    FROM rate_limit_buckets b
    WHERE b.scope = p_scope AND b.subject = p_subject;

    RETURN QUERY SELECT
        false,
        coalesce(available, 0::real),
        CASE
            WHEN p_refill_per_second <= 0 THEN 0::double precision
            ELSE ((1 - coalesce(available, 0::real)) / p_refill_per_second)::double precision
        END;
END;
$$;
