# Dashboard design: per-user video tracking on PostgreSQL

**Status:** proposed, awaiting approval
**Scope:** turn the stateless stats API into a multi-user dashboard that tracks
view counts for YouTube, TikTok and Meta videos over time.

This document is the plan. Nothing here is built yet.

---

## 1. What we are building

Today the service answers one question, statelessly: *given this URL, what are
its counters right now?*

The dashboard adds three things on top:

1. **Users.** Email + password accounts with server-side sessions.
2. **Tracking.** Each user keeps a list of videos they care about.
3. **History.** A background poller refreshes tracked videos on a schedule and
   records a time series, so the dashboard can show growth, not just a number.

The existing `GET /v1/stats` endpoint stays exactly as it is — ad-hoc lookups
with no account required are still useful, and they are what the poller calls
internally.

### The shape of the thing

```
                  ┌──────────────┐
 browser ────────►│  web/  (SSR) │──┐
                  └──────────────┘  │   ┌──────────────┐    ┌────────────┐
                  ┌──────────────┐  ├──►│  tracking/   │───►│  storage/  │
 curl / scripts──►│  api/  (JSON)│──┘   │ (app layer)  │    │  postgres  │
                  └──────────────┘      └──────────────┘    └─────┬──────┘
                                                                  │
                  ┌──────────────┐      ┌──────────────┐          │
                  │  poller/     │─────►│  provider/*  │          │
                  │ (scheduler)  │      │ yt/tt/meta   │          │
                  └──────┬───────┘      └──────────────┘          │
                         └─────────────────────────────────────────┘
                                    claims work with SKIP LOCKED
```

---

## 2. Data model

### 2.1 The central modelling decision

**A video is shared; the tracking of it is per-user.**

If two users both track the same TikTok video, we fetch it once and store one
time series. `tracked_videos` is a join table carrying the per-user bits (label,
when they added it), and `videos` + `metric_snapshots` carry the shared truth.

The alternative — a snapshot row per user per video — duplicates every counter
by the number of watchers and multiplies our upstream fetch load by the same
factor. Since upstream quota (YouTube) and anti-bot exposure (TikTok, Meta) are
the genuinely scarce resources here, sharing is not just tidier, it is the
difference between polling 500 videos and polling 5,000.

### 2.2 Entity relationship

```
users ──< sessions
  │
  └──< tracked_videos >── videos ──< metric_snapshots   (raw, partitioned monthly)
                            │     └─< metric_daily       (rollup, kept forever)
                            └─────< fetch_attempts       (audit, 14d retention)
```

### 2.3 Counters are nullable, always

The domain already models counters as `*uint64` so that "the platform does not
report this" (nil) stays distinguishable from "genuinely zero". That property
must survive into the database, so **every counter column is nullable and we
never default one to `0`**. An Instagram photo post has no view count; a
YouTube video with likes hidden has no like count. Writing `0` there would turn
a measurement gap into a false fact, and every chart downstream would inherit
the lie.

---

## 3. Schema

PostgreSQL 16+. All timestamps are `timestamptz`, all stored in UTC.

### 3.1 Conventions

- **Identifiers.** `bigint GENERATED ALWAYS AS IDENTITY` for internal keys, plus
  a public `uuid` on user-facing rows so ids in URLs are not enumerable.
- **Platform.** `text` with a `CHECK` constraint rather than a Postgres `enum`.
  Enums cannot drop a value and `ALTER TYPE ... ADD VALUE` cannot run in the
  same transaction that then uses it, which makes migrations awkward. A check
  constraint is a one-line migration to change, and Go already validates.
- **Soft delete.** `archived_at timestamptz` rather than a boolean, so we know
  when.

### 3.2 `users`

```sql
CREATE TABLE users (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id       uuid        NOT NULL DEFAULT gen_random_uuid(),
    email           citext      NOT NULL,
    password_hash   text        NOT NULL,   -- argon2id, encoded with its params
    display_name    text        NOT NULL DEFAULT '',
    timezone        text        NOT NULL DEFAULT 'UTC',
    status          text        NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_login_at   timestamptz,

    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_public_id_key UNIQUE (public_id)
);
```

`citext` (from the `citext` extension) makes email comparison case-insensitive
at the database level, which is the only place it can be enforced reliably —
`Alice@x.com` and `alice@x.com` are the same account, and a unique index on
`lower(email)` achieves the same thing but forces every query to remember the
`lower()`.

`timezone` is display-only. Nothing is ever *stored* in a local time.

### 3.3 `sessions`

```sql
CREATE TABLE sessions (
    token_hash   bytea       PRIMARY KEY,        -- sha256 of the opaque token
    user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,           -- absolute expiry
    user_agent   text        NOT NULL DEFAULT '',
    ip           inet
);

CREATE INDEX sessions_user_id_idx  ON sessions (user_id);
CREATE INDEX sessions_expires_idx  ON sessions (expires_at);
```

**Only the hash is stored.** A database dump therefore does not hand the reader
a set of live sessions. The token itself exists in exactly two places: the
user's cookie and the memory of the request that minted it.

Two expiries: `expires_at` is absolute (30 days) and `last_seen_at` drives idle
timeout (7 days). A reaper deletes rows past either bound on the same ticker
that already reaps the in-process cache.

### 3.4 `videos`

The shared record for one video on one platform.

```sql
CREATE TABLE videos (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id          uuid        NOT NULL DEFAULT gen_random_uuid(),

    platform           text        NOT NULL
                       CHECK (platform IN ('youtube', 'tiktok', 'meta')),
    platform_video_id  text        NOT NULL,
    canonical_url      text        NOT NULL,

    -- Metadata, refreshed on every successful fetch.
    title              text,
    channel_id         text,
    channel_title      text,
    published_at       timestamptz,

    -- Denormalised current values; see §3.5 for why these live here.
    latest_view_count     bigint,
    latest_like_count     bigint,
    latest_comment_count  bigint,
    latest_share_count    bigint,
    latest_save_count     bigint,
    latest_captured_at    timestamptz,

    -- Scheduling state; see §6.
    tracker_count        integer     NOT NULL DEFAULT 0,
    fetch_interval       interval    NOT NULL DEFAULT '6 hours',
    next_fetch_at        timestamptz,
    locked_until         timestamptz,
    consecutive_failures integer     NOT NULL DEFAULT 0,
    last_fetch_at        timestamptz,
    last_fetch_status    text        NOT NULL DEFAULT 'pending'
                         CHECK (last_fetch_status IN
                               ('pending','ok','not_found','blocked','error')),
    last_fetch_error     text,
    unavailable_since    timestamptz,   -- set when the platform says it is gone

    first_seen_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT videos_platform_id_key UNIQUE (platform, platform_video_id),
    CONSTRAINT videos_public_id_key   UNIQUE (public_id)
);

-- The poller's claim query. Partial, so it stays tiny however many videos
-- exist: only rows actually due are in the index.
CREATE INDEX videos_due_idx ON videos (next_fetch_at)
    WHERE next_fetch_at IS NOT NULL AND unavailable_since IS NULL;
```

`(platform, platform_video_id)` is the natural key and the deduplication point.
Two users pasting `youtu.be/X` and `youtube.com/watch?v=X` must land on one row,
which is why the URL parsers we already have run *before* the upsert.

### 3.5 Why `latest_*` is denormalised onto `videos`

The dashboard's hot query is "list my 40 videos with their current counts". The
normalised way to answer it is `DISTINCT ON (video_id) ... ORDER BY video_id,
captured_at DESC` against the snapshot table — correct, and fine at first, but
it degrades as the time series grows because it must reach into a large, mostly
irrelevant table once per tracked video.

Instead the writer updates `videos.latest_*` in the **same transaction** as the
snapshot insert. The cost is five columns of duplication and the discipline of
one transaction; the benefit is that the dashboard's main query never touches
the time series at all. It cannot drift, because there is no code path that
inserts a snapshot without the update — that is enforced by putting both
statements in one repository method (§7.2).

### 3.6 `tracked_videos`

```sql
CREATE TABLE tracked_videos (
    user_id     bigint      NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    video_id    bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    label       text        NOT NULL DEFAULT '',   -- user's own name for it
    notes       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,

    PRIMARY KEY (user_id, video_id)
);

-- Reverse lookup: "who tracks this video?", used when recomputing the
-- fetch interval and when the last tracker leaves.
CREATE INDEX tracked_videos_video_idx ON tracked_videos (video_id)
    WHERE archived_at IS NULL;
```

The composite primary key gives us the uniqueness constraint ("you cannot track
the same video twice") and the covering index for the dashboard list in one
object.

### 3.7 `metric_snapshots` — the time series

```sql
CREATE TABLE metric_snapshots (
    video_id      bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    captured_at   timestamptz NOT NULL,
    view_count    bigint,
    like_count    bigint,
    comment_count bigint,
    share_count   bigint,
    save_count    bigint,

    PRIMARY KEY (video_id, captured_at)
) PARTITION BY RANGE (captured_at);
```

**Partitioned by month from day one.** This is a deliberate up-front cost. The
reason is retention: raw snapshots are kept 30 days and rolled up into
`metric_daily` after that. Dropping a month is `DROP TABLE
metric_snapshots_2026_07` — instant, no bloat, no vacuum. The unpartitioned
alternative is a monthly `DELETE` of several million rows, which is slow, leaves
the table bloated, and generates enough WAL to be noticed. Converting a large
table to partitioned later is a migration nobody enjoys, so we pay now.

Partitions are created two months ahead by the scheduler (§6.5), so a missing
partition can never cause an insert to fail in production.

The primary key `(video_id, captured_at)` is also exactly the index the history
chart wants: "all snapshots for video X between two times" is one range scan.

A **BRIN** index on `captured_at` supports the rollup and retention jobs at a
fraction of the size of a btree:

```sql
CREATE INDEX metric_snapshots_captured_brin
    ON metric_snapshots USING brin (captured_at);
```

#### Do we store a snapshot when nothing changed?

Yes. "We checked at 14:00 and it was still 220,500" is information — it tells a
chart the line is flat rather than that we lost coverage, and it is what lets us
distinguish a stalled video from a stalled poller. At a 6-hour interval that is
4 rows per video per day: 1,000 tracked videos is 1.5M rows a year raw, of which
only 30 days is ever held at full resolution.

### 3.8 `metric_daily` — the rollup

```sql
CREATE TABLE metric_daily (
    video_id        bigint NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    day             date   NOT NULL,
    first_view_count bigint,
    last_view_count  bigint,
    last_like_count     bigint,
    last_comment_count  bigint,
    last_share_count    bigint,
    last_save_count     bigint,
    view_delta      bigint,      -- last - first; MAY BE NEGATIVE, see below
    sample_count    integer NOT NULL,

    PRIMARY KEY (video_id, day)
);
```

Charts longer than 30 days read this table. It is small enough to keep forever:
1,000 videos × 365 days is 365k rows a year.

> **Counters are not monotonic.** Platforms revise view counts downward —
> TikTok re-runs bot filtering, YouTube purges invalid views, Meta corrects
> aggregation. `view_delta` is therefore a plain `bigint`, signed, with no check
> constraint, and every chart and "views gained" figure must render a negative
> gracefully rather than treating it as corrupt data. Assuming monotonicity here
> is the single easiest way to end up with a dashboard that occasionally shows
> nonsense.

### 3.9 `fetch_attempts` — the audit trail

```sql
CREATE TABLE fetch_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    video_id     bigint      NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    platform     text        NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    duration_ms  integer     NOT NULL,
    status       text        NOT NULL
                 CHECK (status IN ('ok','not_found','blocked','rate_limited',
                                   'timeout','error')),
    error_code   text,
    error_detail text
);

CREATE INDEX fetch_attempts_video_idx   ON fetch_attempts (video_id, started_at DESC);
CREATE INDEX fetch_attempts_started_idx ON fetch_attempts (started_at);
```

We already know from the provider work that reliability differs sharply by
platform and that TikTok and Meta fail probabilistically. Without this table,
"is the TikTok provider degrading?" is unanswerable except by grepping logs.
With it, it is one query. Retained 14 days.

### 3.10 `schema_migrations`

```sql
CREATE TABLE schema_migrations (
    version    integer     PRIMARY KEY,
    name       text        NOT NULL,
    checksum   text        NOT NULL,     -- sha256 of the file, detects edits
    applied_at timestamptz NOT NULL DEFAULT now()
);
```

---

## 4. Migrations

Plain `.sql` files under `internal/storage/postgres/migrations/`, embedded with
`go:embed`, applied by ~120 lines of Go at boot. No third-party migration tool —
it would be the largest dependency in the project and we need perhaps a tenth of
what it does.

```
migrations/
  0001_extensions.up.sql        -- citext, pgcrypto
  0002_users_sessions.up.sql
  0003_videos.up.sql
  0004_metric_snapshots.up.sql  -- partitioned parent + first partitions
  0005_metric_daily.up.sql
  0006_fetch_attempts.up.sql
```

Rules the runner enforces:

- **One transaction per migration.** A half-applied migration is not a state we
  ever need to reason about.
- **An advisory lock around the whole run** (`pg_advisory_lock(hashtext('socialstats_migrate'))`).
  Multiple replicas booting simultaneously is the normal case on a rolling
  deploy; without the lock they race to apply the same file.
- **Checksums.** Editing an already-applied migration is refused at boot rather
  than silently ignored, because the alternative is environments that disagree
  about what the schema is.
- **Forward-only.** No `.down.sql`. Rolling a schema back in production is
  almost always the wrong move; the recovery path is a new forward migration.
  This is a deliberate simplification and it should be argued with if you
  disagree.

`MIGRATE_ON_BOOT` (default `true`) can be turned off for deployments that run
migrations as a separate step, with `make migrate` for that case.

---

## 5. Authentication

### 5.1 Passwords

**argon2id** via `golang.org/x/crypto/argon2`, not bcrypt: it is memory-hard, so
a GPU attacker gets much less leverage, and it is what current guidance points
at. Parameters, stored encoded in the hash string so they can be raised later
without invalidating existing accounts:

| Parameter | Value |
|-----------|-------|
| time      | 3 |
| memory    | 64 MiB |
| threads   | 4 |
| salt      | 16 random bytes, per password |
| key       | 32 bytes |

Encoded as the standard `$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>`. On a
successful login with an out-of-date parameter set, the password is re-hashed
with the current one — the only moment we hold the plaintext.

64 MiB per hash is a real cost: it means login is deliberately expensive and a
burst of logins is memory pressure. That is the trade, and §5.4's rate limit is
what stops it being a denial-of-service vector.

### 5.2 Sessions

- Token: 32 bytes from `crypto/rand`, base64url — 256 bits, unguessable.
- Stored as `sha256(token)`; see §3.3.
- Cookie: `HttpOnly`, `SameSite=Lax`, `Secure` (off only when
  `APP_ENV=development`), `Path=/`.
- Rotated on login and on privilege change; the old row is deleted, so a stolen
  pre-login token is useless after the user signs in.
- Logout deletes the row. "Log out everywhere" deletes all rows for the user —
  which server-side sessions give us for free and JWTs would not.

### 5.3 CSRF

Cookie-authenticated form posts need CSRF protection. Double-submit: a random
token in a `__Host-csrf` cookie and in a hidden form field, compared on every
unsafe method. `SameSite=Lax` already blocks the common cases; the token covers
the rest. JSON endpoints called with a bearer session token instead of a cookie
skip the check, since the attack does not apply there.

### 5.4 Abuse resistance

- Login attempts rate-limited per email **and** per IP (5 per 15 minutes,
  token bucket in Postgres so it holds across replicas).
- Registration and login return the **same** error text for "no such user" and
  "wrong password", so the endpoint is not an account-existence oracle.
- Passwords: minimum 12 characters, checked against a small embedded list of
  the most common ones. No composition rules — they push users toward
  `Password1!` and buy nothing.

---

## 6. The poller

### 6.1 Claiming work

Postgres is the queue. No Redis, no separate broker.

```sql
UPDATE videos v
SET locked_until = now() + interval '5 minutes'
FROM (
    SELECT id FROM videos
    WHERE next_fetch_at <= now()
      AND (locked_until IS NULL OR locked_until < now())
      AND unavailable_since IS NULL
      AND platform = $1
    ORDER BY next_fetch_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
) due
WHERE v.id = due.id
RETURNING v.*;
```

`FOR UPDATE SKIP LOCKED` is the whole story for horizontal scaling: run five
replicas and they will divide the work between them with no coordination, no
leader election, and no risk of two workers fetching the same video. The
`locked_until` column covers the case where a worker dies mid-fetch — the lock
expires and the video becomes claimable again.

### 6.2 Batching YouTube

`videos.list` accepts **up to 50 ids in one call for one quota unit**. Fetching
50 videos one at a time costs 50 units; batched, it costs 1. Against a
10,000-unit daily quota that is the difference between 10,000 and 500,000 video
refreshes a day.

This does not fit the current `Provider` interface, which is one URL in, one
result out. The proposal is an **optional** capability that the poller
feature-detects:

```go
// BatchProvider is implemented by providers whose upstream can resolve several
// videos in one call. The poller uses it when present and falls back to
// per-video Stats when not.
type BatchProvider interface {
    Provider
    BatchSize() int
    StatsBatch(ctx context.Context, ids []string) (map[string]*VideoStats, error)
}
```

YouTube implements it. TikTok and Meta do not and never will — their pages are
one video each — so they keep the existing path. Making it an optional interface
rather than widening `Provider` means the two scraping providers do not grow a
method that would only ever return "not supported".

### 6.3 Pacing, per platform

Quota is not the binding constraint; **ban risk is**. The pacing differs by
platform for reasons the provider packages already document:

| Platform  | Concurrency | Min gap between requests | Default interval | Why |
|-----------|-------------|--------------------------|------------------|-----|
| YouTube   | 4           | none                     | 6 h              | Official API, quota-metered, batched 50× |
| TikTok    | 2           | 2 s                      | 12 h             | ~⅓ of requests get a challenge; retrying fast trips a harder block |
| Meta      | 2           | 2 s                      | 12 h             | Login walls and page-shell responses; nothing is gained by pushing |

These are config, not constants (`POLL_<PLATFORM>_CONCURRENCY`,
`_MIN_INTERVAL`, `_DEFAULT_REFRESH`).

### 6.4 Backoff and giving up

On failure, `consecutive_failures` increments and the next attempt is scheduled
at `min(interval × 2^failures, 24h)` with ±20% jitter. Jitter matters: without
it, everything that failed during an outage retries in the same second when the
outage ends.

Terminal handling by sentinel — these map straight onto the errors the providers
already return:

| Error | Poller behaviour |
|-------|------------------|
| `ErrNotFound` ×3 in a row | Set `unavailable_since`, stop polling, keep all history. The dashboard shows the video greyed out with its last known figures. |
| `ErrBlocked` | Back off; do **not** mark unavailable. A login wall is our problem, not evidence the video is gone. |
| `ErrRateLimited` | Back off hard — 1 hour minimum — for the whole platform, not just this video. |
| `ErrMisconfigured` | Stop polling that platform entirely and log loudly. Retrying a missing API key 400 times an hour helps nobody. |

The distinction in row two is the one worth guarding: conflating "blocked" with
"deleted" would quietly delete users' Facebook videos from their dashboards
during a bad afternoon.

### 6.5 Housekeeping

One `time.Ticker` loop, alongside the existing cache reaper:

| Job | Frequency | What |
|-----|-----------|------|
| Rollup | hourly | Upsert yesterday's and today's `metric_daily` rows |
| Retention: snapshots | daily | `DROP TABLE` partitions older than 30 days |
| Retention: attempts | daily | Delete `fetch_attempts` older than 14 days |
| Partition creation | daily | Ensure partitions exist 2 months ahead |
| Session reap | hourly | Delete expired sessions |

### 6.6 Fetch interval when several users share a video

Interval lives on `videos` because the fetch is shared. When users can eventually
choose their own refresh rate, the video's interval is the **minimum** of its
active trackers' preferences — the most demanding subscriber wins, and everyone
else gets data at least as fresh as they asked for. `videos.fetch_interval` is
recomputed on track, untrack and preference change.

For v1 every user gets the platform default, but the column and the rule exist
so adding the preference later is a UI change rather than a schema change.

---

## 7. Go package layout

```
internal/
  storage/postgres/
    pool.go              pgxpool construction, health check
    migrate.go           embedded migration runner (§4)
    migrations/*.sql
    users.go             UserRepo
    sessions.go          SessionRepo
    videos.go            VideoRepo   — upsert, claim, record (§7.2)
    tracking.go          TrackingRepo
    metrics.go           MetricRepo  — history, rollups, retention
  auth/
    password.go          argon2id hash + verify + rehash policy
    session.go           issue, verify, rotate, revoke
    middleware.go        RequireUser, CSRF, login rate limit
  tracking/
    service.go           track/untrack, dashboard queries, deltas
  poller/
    scheduler.go         ticker, claim loop, per-platform pacing
    worker.go            fetch one (or a batch), record, reschedule
    housekeeping.go      rollups, retention, partitions
  web/
    handlers.go          dashboard pages
    templates/*.html     embedded html/template
    static/*             embedded css, one small js file
internal/api/            existing JSON handlers, plus §8's new routes
```

This follows the dependency rule already in the README: `api`/`web` → `tracking`
→ `domain` ← `storage`, `provider`. **Repositories return domain types and
domain sentinel errors** — nothing above `storage/postgres` should ever see a
`pgx` type or a `23505` constraint-violation code. That is what keeps the option
of swapping the store open, and it is why `api/errors.go` can stay the only
place that knows about HTTP status codes.

### 7.1 Interfaces, defined by the consumer

`tracking.Service` declares the narrow interfaces it needs and the Postgres
repositories satisfy them implicitly — the same pattern `stats.Service` already
uses with `Resolver`. That keeps the service testable with fakes and keeps the
import arrow pointing inward.

### 7.2 The one transactional invariant

```go
// Record persists one fetch result: the snapshot, the denormalised current
// values, the audit row and the next schedule, in a single transaction.
//
// These four writes must not be separable. A snapshot without the latest_*
// update makes the dashboard show stale numbers with no way to detect it; a
// schedule update without the snapshot silently loses a data point. Keeping
// them in one method and one transaction is what makes §3.5's denormalisation
// safe to rely on.
func (r *VideoRepo) Record(ctx context.Context, videoID int64, res FetchResult) error
```

---

## 8. HTTP surface

Existing routes are unchanged. New ones:

### Auth
| Method | Path | Notes |
|--------|------|-------|
| POST | `/v1/auth/register` | `{email, password, display_name}` → sets session cookie |
| POST | `/v1/auth/login` | `{email, password}` → sets session cookie |
| POST | `/v1/auth/logout` | deletes the session |
| GET  | `/v1/auth/me` | current user |

### Tracking
| Method | Path | Notes |
|--------|------|-------|
| GET | `/v1/videos` | the user's list with current counts and deltas; `?platform=`, `?sort=`, `?limit=&cursor=` |
| POST | `/v1/videos` | `{url, label?}` — parses, upserts, **fetches immediately**, starts tracking |
| GET | `/v1/videos/{public_id}` | one video with its tracking metadata |
| PATCH | `/v1/videos/{public_id}` | `{label?, notes?}` |
| DELETE | `/v1/videos/{public_id}` | untrack (archive); history is kept |
| GET | `/v1/videos/{public_id}/history` | `?from=&to=&bucket=hour\|day` |
| GET | `/v1/dashboard/summary` | totals, top movers, coverage/health |

`POST /v1/videos` fetching synchronously matters for how the product feels: a
user pastes a URL and sees the number, rather than an empty row that fills in
some time in the next six hours. If the fetch fails the video is still tracked —
with `last_fetch_status` set — because a TikTok challenge at 14:03 is not a
reason to refuse to track a video.

### Dashboard pages (server-rendered)
| Path | Page |
|------|------|
| `GET /` | dashboard: tracked videos, sortable, with sparklines |
| `GET /login`, `/register` | forms |
| `GET /videos/{public_id}` | one video: full history chart, fetch log |
| `GET /settings` | display name, timezone, password change |

### 8.1 The dashboard list query

The hot path, and the reason for the schema choices above:

```sql
SELECT v.public_id, v.platform, v.canonical_url, v.title, v.channel_title,
       t.label,
       v.latest_view_count, v.latest_like_count, v.latest_comment_count,
       v.latest_captured_at, v.last_fetch_status, v.unavailable_since,
       v.latest_view_count - baseline.view_count AS views_gained
FROM tracked_videos t
JOIN videos v ON v.id = t.video_id
LEFT JOIN LATERAL (
    SELECT view_count
    FROM metric_snapshots s
    WHERE s.video_id = v.id
      AND s.captured_at <= now() - $2::interval
    ORDER BY s.captured_at DESC
    LIMIT 1
) baseline ON true
WHERE t.user_id = $1 AND t.archived_at IS NULL
ORDER BY v.latest_view_count DESC NULLS LAST
LIMIT $3;
```

The `LEFT JOIN LATERAL` is the right tool: it runs one indexed
`(video_id, captured_at)` lookup per row, and `LEFT` means a video added
yesterday shows its counts with a null delta rather than dropping out of the
list entirely — which an inner join would do, and which would be a confusing bug
to chase later.

---

## 9. Rendering the dashboard

`html/template` with `go:embed`, parsed once at boot. No build step, no
node_modules, consistent with a project that currently has zero dependencies.

**Charts as inline SVG, generated in Go.** A sparkline is a `<polyline>` with
scaled points; the detail chart is axes plus a path. Perhaps 150 lines of
rendering code, and it means the dashboard works with JavaScript disabled, has
nothing to load from a CDN, and cannot be broken by an upstream library's
release. If we later want zoom and hover-scrub, that is the moment to reconsider
— not before.

Progressive enhancement only: one small embedded JS file for the range picker
and sort controls, with the page fully functional without it (sort and range are
also query parameters).

---

## 10. Configuration

```bash
# ---- database ----
DATABASE_URL=postgres://socialstats:secret@localhost:5432/socialstats?sslmode=disable
DATABASE_MAX_CONNS=10
DATABASE_MIN_CONNS=2
DATABASE_CONN_MAX_LIFETIME=30m
MIGRATE_ON_BOOT=true

# ---- auth ----
SESSION_TTL=720h              # 30 days absolute
SESSION_IDLE_TTL=168h         # 7 days idle
SESSION_COOKIE_NAME=ss_session
SESSION_COOKIE_SECURE=true    # forced false when APP_ENV=development
REGISTRATION_OPEN=true        # false makes it invite/admin-only

# ---- poller ----
POLL_ENABLED=true
POLL_TICK=1m                  # how often we look for due videos
POLL_BATCH=50                 # videos claimed per tick per platform
POLL_YOUTUBE_CONCURRENCY=4
POLL_YOUTUBE_REFRESH=6h
POLL_TIKTOK_CONCURRENCY=2
POLL_TIKTOK_MIN_INTERVAL=2s
POLL_TIKTOK_REFRESH=12h
POLL_META_CONCURRENCY=2
POLL_META_MIN_INTERVAL=2s
POLL_META_REFRESH=12h

# ---- retention ----
SNAPSHOT_RETENTION=30d
ATTEMPT_RETENTION=14d
```

`POLL_ENABLED=false` lets you run web replicas that serve traffic and a single
worker replica that polls — the standard split once this is more than one box.

---

## 11. Dependencies

Two, where there are currently none:

| Module | Why |
|--------|-----|
| `github.com/jackc/pgx/v5` | Postgres driver and pool. There is no Postgres driver in the standard library, so this is unavoidable. pgx over `lib/pq` because the latter is in maintenance mode, and its native interface handles `interval`, `inet` and `citext` without ceremony. |
| `golang.org/x/crypto` | argon2id. Password hashing is not in the standard library, and hand-rolling it is not an option. |

Both are `golang.org/x`-adjacent in stability terms. The README's "no web
framework, no ORM" claim stays true — `pgx` is a driver, and queries stay as
hand-written SQL in the repository layer.

---

## 12. Local development and testing

`docker-compose.yml` with Postgres 16 and a named volume, plus:

```
make db-up      # start postgres
make migrate    # apply migrations
make db-reset   # drop, recreate, migrate, seed
make test       # unit tests (no database needed)
make test-db    # integration tests against TEST_DATABASE_URL
```

**Repository tests run against a real Postgres**, not a mock. Half of what this
design depends on — `SKIP LOCKED`, `LATERAL`, partition routing, `citext`
uniqueness, `ON CONFLICT` upserts — is behaviour a mock would simply assert
back at us. Tests skip themselves with a clear message when `TEST_DATABASE_URL`
is unset, so `make test` stays fast and dependency-free for everyone else, and
CI sets the variable.

Each test runs in a transaction that is rolled back, so they neither interfere
nor need cleanup.

---

## 13. Implementation order

Each step ends with something that runs and is tested.

| # | Step | Delivers |
|---|------|----------|
| 1 | pgx pool, config, migration runner, `docker-compose`, `/readyz` checks the DB | The app boots against Postgres |
| 2 | Migrations 0001–0006 + repository layer + integration tests | Schema exists, is exercised |
| 3 | `auth` package: argon2id, sessions, middleware, rate limit | Login works via curl |
| 4 | Tracking service + JSON endpoints §8 | Full API, no UI |
| 5 | Poller: claim loop, per-platform pacing, backoff, `fetch_attempts` | History accumulates |
| 6 | Housekeeping: rollups, retention, partition creation | Bounded storage |
| 7 | YouTube `BatchProvider` | 50× less quota |
| 8 | `web` package: templates, SVG charts, pages | The dashboard |
| 9 | OpenAPI spec for the new routes, README, `.env.example` | Docs match reality |

Steps 1–4 are the useful minimum: accounts, tracking, and an API. Step 5 is what
makes it a *dashboard* rather than a bookmark list.

---

## 14. Decisions worth arguing with

Flagging these explicitly because they are the ones I would most expect you to
want changed:

1. **Forward-only migrations** (§4). Convenient for us, occasionally painful in
   an incident.
2. **Partitioning from day one** (§3.7). Real complexity before there is real
   data. The counter-argument is that retention by `DELETE` is fine until it
   isn't, and converting later is a bad day.
3. **Denormalised `latest_*`** (§3.5). Duplicated state, justified by the hot
   query. If you would rather keep the schema pure and add a materialised view
   later, that is a defensible different call.
4. **SVG charts rendered in Go** (§9). Cheap and robust, but a charting library
   would give richer interaction sooner.
5. **Synchronous fetch on `POST /v1/videos`** (§8). Better feel, but it makes a
   user-facing request depend on TikTok, which can take ~30 s across retries.
   The alternative is a "pending" row and a spinner.
6. **Shared fetch interval** (§6.6) rather than per-user refresh rates. Simpler
   and cheaper upstream; means a user cannot pay for faster refresh.
