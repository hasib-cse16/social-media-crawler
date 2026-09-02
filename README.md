# socialstats

A dashboard and HTTP service for reading public view counts. Paste a video URL
and get its numbers, plus who posted it — including the channel's public
contact email where the owner published one. YouTube, TikTok and Meta
(Facebook + Instagram) are implemented behind one interface.

Every lookup is recorded against your account, so the dashboard is a history of
what you asked. Nothing runs in the background: a lookup is a question, and the
numbers it returns are what the platform said at that moment.

Two dependencies: `pgx` (there is no PostgreSQL driver in the standard library)
and `golang.org/x/crypto` (nor an argon2id). No web framework, no ORM, no
migration tool — queries are hand-written SQL in `internal/storage/postgres`.

## Quick start

```bash
cp .env.example .env          # then set YOUTUBE_API_KEY
make db-up                    # starts postgres in docker
make migrate                  # applies the schema
make run
```

PostgreSQL is required: the dashboard stores accounts, sessions and each user's
lookup history there, and the service refuses to boot without a reachable
`DATABASE_URL`. `make db-up` starts one on `:5434` for development and a
disposable one on `:5433` for integration tests.

The service loads `.env` from the working directory (walking up a few parents,
so running from an IDE works too). **Real environment variables always win**, so
`.env` is inert in production, where you inject real env vars instead. There is
no dotenv dependency — see `internal/config/dotenv.go`.

Running from GoLand or another IDE needs no extra setup: just make sure the run
configuration's working directory is inside the repository. Alternatively, set
`YOUTUBE_API_KEY` in the run configuration's environment.

```bash
open http://localhost:8080     # the dashboard

curl "localhost:8080/v1/stats?url=https://youtu.be/dQw4w9WgXcQ"
curl "localhost:8080/v1/stats?url=https://www.tiktok.com/@user/video/7249376077976472833"
curl "localhost:8080/v1/stats?url=https://www.instagram.com/reel/Cx1y2z3AbCd/"
curl "localhost:8080/v1/stats?url=https://www.facebook.com/watch/?v=1234567890"
curl -XPOST localhost:8080/v1/stats -d '{"url":"https://www.youtube.com/shorts/dQw4w9WgXcQ"}'
curl localhost:8080/healthz
```

Only YouTube needs a credential. TikTok and Meta work with no setup.

Interactive API reference: **<http://localhost:8080/docs>** (Swagger UI).
Raw spec: **<http://localhost:8080/openapi.yaml>**.

Get an API key: Google Cloud Console → enable **YouTube Data API v3** → create
an API key. `videos.list` and `channels.list` cost 1 unit each against a
10,000/day quota, so a YouTube lookup costs 2 — which is why responses are
cached.

## API

| Method | Path          | Notes                                     |
|--------|---------------|-------------------------------------------|
| GET    | `/v1/stats`   | `?url=<video url>`                        |
| POST   | `/v1/stats`   | body `{"url":"..."}`                      |
| GET    | `/`                 | the dashboard: the form and your history  |
| GET    | `/lookups/{id}`     | one lookup: counters and channel details  |
| GET    | `/login`, `/register`, `/settings` | account pages              |
| GET    | `/v1/lookups`       | the caller's lookup history, newest first |
| POST   | `/v1/lookups`       | `{url}` — fetch it now and record it      |
| GET    | `/v1/lookups/{id}`  | one past lookup                           |
| DELETE | `/v1/lookups/{id}`  | remove it from the history                |
| POST   | `/v1/auth/register` | `{email, password, display_name?}` → session |
| POST   | `/v1/auth/login`    | `{email, password}` → session             |
| POST   | `/v1/auth/logout`   | ends this session                         |
| POST   | `/v1/auth/logout-all` | ends every session for the account      |
| POST   | `/v1/auth/password` | change password; revokes every session    |
| GET    | `/v1/auth/me`       | the signed-in account                     |
| GET    | `/healthz`    | liveness + registered platforms                 |
| GET    | `/readyz`     | readiness — 503 when the database is unreachable |
| GET    | `/docs`       | Swagger UI                                |
| GET    | `/openapi.yaml` | OpenAPI 3.1 specification               |
| GET    | `/swagger/index.html` | alias for `/docs` (swaggo convention) |

Success:

```json
{
  "data": {
    "platform": "youtube",
    "video_id": "dQw4w9WgXcQ",
    "canonical_url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
    "title": "Never Gonna Give You Up",
    "channel_id": "UCuAXFkgsw1L7xaCfnd5JJOw",
    "channel_title": "Rick Astley",
    "published_at": "2009-10-25T06:57:33Z",
    "view_count": 1500000000,
    "like_count": 18000000,
    "comment_count": 2200000,
    "fetched_at": "2026-08-27T10:00:00Z",
    "cached": false
  }
}
```

Counters are omitted (rather than zero) when the uploader hides them.

Error:

```json
{"error":{"code":"not_found","message":"the video does not exist, is private, or was removed","request_id":"…"}}
```

| Code                   | HTTP |
|------------------------|------|
| `missing_url`, `invalid_url`, `invalid_body`, `unsupported_platform` | 400 |
| `not_found`            | 404 |
| `upstream_blocked`     | 502 |
| `rate_limited`         | 429 |
| `upstream_error`       | 502 |
| `provider_unavailable` | 503 |
| `upstream_timeout`     | 504 |
| `not_implemented`      | 501 |

Every response carries `X-Request-Id` (an inbound one is honoured) and
`X-Cache: hit|miss`.

Accepted YouTube URL forms: `watch?v=`, `youtu.be/`, `/shorts/`, `/embed/`,
`/live/`, `/v/`, `youtube-nocookie.com`, with or without scheme.

Accepted Meta URL forms: Instagram `/p/`, `/reel/`, `/reels/`, `/tv/`, with or
without a leading handle; Facebook `/watch/?v=`, `/reel/<id>`, `/<page>/videos/
[slug/]<id>`, `video.php?v=`, and the `fb.watch` / `/share/v/` short links.

## Accounts

Sign in with a cookie or a bearer token — the same endpoints serve both, which
is what makes the API usable from a browser and from curl without either one
getting in the other's way.

```bash
TOKEN=$(curl -s -XPOST localhost:8080/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"seven blue mountains rise"}' \
  | jq -r .data.token)

curl -s localhost:8080/v1/auth/me -H "Authorization: Bearer $TOKEN"
```

Login also sets an `HttpOnly` session cookie and a readable CSRF cookie. Which
one you use decides whether CSRF applies: cookies are attached by the browser
automatically and so can be forged cross-site, while a bearer header cannot be,
so bearer requests skip the check. Cookie-authenticated writes must echo the
CSRF cookie back in `X-CSRF-Token` or a `csrf_token` form field.

A few properties worth knowing about:

- **Passwords are argon2id** at 64 MiB, 3 passes. That cost is the point — it is
  what makes an offline attack on a leaked table expensive — and it applies to
  our own login path too, so concurrent hashing is capped
  (`ARGON2_CONCURRENCY`, default 4) and the ceiling is logged at startup. Budget
  256 MiB for it at the defaults.
- **Raising the cost does not lock anyone out.** Each hash stores the parameters
  it was made with, and the next successful login rehashes at the current ones —
  the only moment the plaintext exists.
- **Failed sign-ins are limited per email *and* per IP**, five per fifteen
  minutes, in a Postgres token bucket so the limit holds across replicas rather
  than being multiplied by the pod count. `429` responses carry a truthful
  `Retry-After` computed from the bucket's refill rate. A successful sign-in
  clears the counter.
- **"No such account" and "wrong password" are indistinguishable**, in wording
  and in timing: an unknown address is still hashed against a decoy, because
  returning early would make the endpoint an account-existence oracle no
  identical error text could hide.
- **Sessions are server-side.** The database stores only `sha256(token)`, so a
  dump does not yield live sessions — and "log out everywhere" and instant
  revocation come for free, which a stateless signed token would have cost.

## Looking videos up

```bash
curl -s -XPOST localhost:8080/v1/lookups -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}'

curl -s localhost:8080/v1/lookups -H "Authorization: Bearer $TOKEN"
```

A lookup **fetches synchronously**, so the caller gets a number back rather than
a row that fills in later. `LOOKUP_TIMEOUT` (30s) bounds it, which matters for
TikTok and Meta: those are page scrapes with retries, and the response waits for
them.

Details worth knowing:

- **A lookup is a reading, not a gauge.** The counters recorded are what the
  platform said at `looked_up_at`, and nothing revises them afterwards. Looking
  the same URL up again appends a row rather than updating the old one, so the
  history stays comparable against the timestamps beside it.
- **A failed lookup is not recorded.** The row is written after a successful
  fetch, so history is a list of answers rather than a list of attempts.
- **Ownership is checked, not assumed.** Another account's lookup id returns
  `404`, not `403`, so the endpoint does not confirm that someone else's lookup
  exists.
- **Counters absent ≠ zero.** A platform that does not report a counter, or an
  uploader who hides one, leaves the field out. An Instagram photo post has no
  view count, and `0` would say something different.

## Channel contact email

A YouTube lookup makes a second API call, `channels.list`, for the uploader's
channel. It brings back the channel title, its `@handle` URL and its
description — and the contact email is read out of that description.

That is a deliberate choice of source, not an oversight:

- **The Data API has no email field.** There is nothing to request. The address
  a creator publishes lives in text they wrote.
- **The About page is not scraped.** YouTube renders the "business enquiries"
  address behind a bot check, so scraping it is unreliable and adversarial.
  Owners who want to be contacted overwhelmingly also write the address into the
  description, which the API returns cleanly.
- **So the hit rate is partial, and honestly so.** Large channels using only the
  About-page field come back with no email. `channel_email` absent means the
  owner published nothing readable — never that the lookup failed. The full
  `channel_description` is returned alongside so you can check for yourself.

Extraction is in `internal/provider/youtube/channel.go`. It reads plain
addresses first and only then the dodge-the-scraper forms (`name (at) example
(dot) com`), because the second pattern is looser and more willing to be wrong.
Both patterns require real separators around `at` and `dot`: without that,
"recommendations.TED" parses as `recommend@ions.TED`, which is exactly the bug
the live tests caught.

## The dashboard

Server-rendered HTML from the same binary. No build step, no `node_modules`,
nothing fetched from a CDN — the binary *is* the frontend, which means the
dashboard cannot be broken by an upstream package release and works with
JavaScript disabled.

```
internal/web/
  web.go         server, template parsing, embedded assets
  routes.go      route table and content negotiation
  handlers.go    one handler per page
  present.go     human-readable error copy
  view.go        view models the templates receive
  format.go      counts and relative times
  templates/     layout, pages, partials — parsed once at boot
  static/        app.css, app.js
```

**Templates are parsed at startup.** A typo fails the process where a deploy
notices, rather than on the one page nobody opened before release; the pages
are rendered into a buffer first, so a template error cannot leave a half-page
already sent with a `200`.

**One middleware protects both surfaces; only the presentation differs.** A
browser navigating to a page it needs an account for is redirected to sign in,
with the destination remembered; a script calling the same URL gets a `401` it
can branch on. The request decides, using `Sec-Fetch-Mode` with `Accept` as the
fallback — a redirect to an HTML login form is useless to an API client.

**Every form posts and redirects.** A refresh does not resubmit, and the back
button behaves. The message that survives the redirect rides in a short-lived
cookie, cleared as it is read.

**JavaScript is progressive enhancement only.** Every view has a shareable URL,
the lookup form is a plain POST, and nothing on the page depends on the script
loading.

Both light and dark themes are derived from one set of custom properties, so a
component never picks a colour that works in only one of them.

## Swagger / OpenAPI

The OpenAPI 3.1 document lives at `internal/docs/openapi.yaml` and is compiled
into the binary with `go:embed`, so the docs ship with the deployment and cannot
drift away from the build that serves them. It is hand-written rather than
generated from comments — no annotation tooling in the build, and the spec stays
readable as the contract it is.

```bash
make docs        # print the doc URLs
make spec-lint   # validate the spec (npx @redocly/cli)
make client      # generate a typescript client from the spec
```

Two routes serve it, both behind `DOCS_ENABLED` (default `true`; set it to
`false` on deployments that must not publish their API surface — the routes then
404):

- `GET /docs` — Swagger UI, with "try it out" enabled.
- `GET /openapi.yaml` — the raw document, for client generation and contract tests.
- `GET /swagger/index.html` — the same UI at the location `gin-swagger`/`swaggo`
  users expect; bare `/swagger` and `/swagger/` redirect to `/docs`.

Swagger UI's CSS/JS come from the unpkg CDN. If your deployment cannot reach the
public internet, vendor `swagger-ui-dist` into a `static/` directory, embed it,
and swap the two URLs in `internal/docs/docs.go`.

The spec is the source of truth for the error contract: `ErrorCode` enumerates
every `code` the service can return alongside its HTTP status, and each response
carries worked examples (including the hidden-counter case, where `like_count`
is absent rather than `0`). `docs_test.go` asserts every route appears in the
spec, so adding an undocumented endpoint fails the build.

## Platform support

| Platform | Source | Credential | Reliability |
|----------|--------|-----------|-------------|
| YouTube  | Data API v3 (`videos.list`) | `YOUTUBE_API_KEY` | Reliable; 10,000 quota units/day |
| TikTok   | Public video page state | none | ~90% per request, see below |
| Instagram | Login-free embed render | none | Good; embeds are a supported surface |
| Facebook | Graph API, else public page | `META_ACCESS_TOKEN` (optional) | Patchy; login walls are common |

TikTok returns `share_count` and `save_count` in addition to the common
counters; YouTube does not expose those, so they are absent for YouTube videos.
Facebook reports `share_count` when its page carries one. Instagram photo posts
and carousels have no view count at all, so `view_count` is absent rather than
zero — as it is anywhere a counter could not be measured.

### Why the TikTok provider scrapes

TikTok has no API that returns metrics for an arbitrary video URL. The official
Display API only returns videos belonging to the user who granted an OAuth
token, and the Research API is limited to approved academic applicants. So the
provider reads the public video page, which embeds its own state as JSON in a
`__UNIVERSAL_DATA_FOR_REHYDRATION__` script tag.

What that means in practice, all of it measured rather than assumed:

- **TikTok challenges a large share of requests.** Roughly a third of fetches
  get a ~44 KB challenge page instead of the video page, regardless of headers,
  HTTP version or TLS settings. A retry usually clears it, which is why
  `TIKTOK_MAX_ATTEMPTS` defaults to 4: single-attempt success measured 60–70%,
  and 4 attempts took it to ~90%.
- **Retries must use a fresh connection.** TikTok appears to decide per
  connection, so retrying on a pooled connection gets the same challenge again.
  The provider therefore has its own client with connection reuse disabled
  (`httpclient.NewWithoutConnectionReuse`); with pooling on, retries succeeded
  4/6 versus 6/6 without.
- **Retry gently.** Retrying every few hundred milliseconds trips a rate limiter
  that returns a 1.4 KB block stub to everything for a minute or two. The
  provider fails fast on that stub rather than deepening the block, and
  `TIKTOK_RETRY_BACKOFF` defaults to 1s.
- **The User-Agent string matters more than it should.** TikTok hard-blocks the
  canonical four-part Chrome version (`Chrome/124.0.0.0`), which is what most
  scraping libraries send, while the three-part form (`Chrome/124.0`) is served
  normally. Override with `TIKTOK_USER_AGENT` if that flips.
- **Numbers arrive as either JSON numbers or strings**, sometimes both within one
  object, so counters are decoded flexibly (`flexnum.go`). `statsV2` (strings) is
  preferred over `stats` because counts above 2^53 lose precision as JSON
  numbers.
- **It fails loudly.** A missing or changed page structure returns an error; the
  provider never reports zero views for a video it could not read.
- **Automated access is contrary to TikTok's Terms of Service.** Whether to run
  it is the deploying party's decision, which is why `TIKTOK_ENABLED=false`
  turns it off and returns `503` for TikTok URLs.

### Why the Meta provider works the way it does

Meta has no public API that returns metrics for an arbitrary video URL. The
Graph API only answers for objects the calling app has been granted — in
practice media on Pages or Instagram Business accounts the token administers —
and the public-data endpoints that once served view counts for any id were
removed in Graph v3.0. oEmbed, the one endpoint that does take a public URL,
returns embed markup and an author name, never a counter.

So the provider has two paths, and picks per request:

- **Graph API**, for Facebook, when `META_ACCESS_TOKEN` is set *and* the token
  can see the object. Exact figures, no anti-bot exposure. A token that cannot
  see the object answers `code 100, "Unsupported get request"`, which is the
  normal case for third-party videos and falls through to the page path.
- **Public page**, otherwise. Instagram counters come from the login-free embed
  render (`/reel/<code>/embed/captioned/`), which still carries the post's
  GraphQL `shortcode_media` payload; Facebook counters come from the Relay
  payload on the public video page.

Details worth knowing before you rely on it:

- **Instagram is the more reliable half**, because the embed render exists to be
  loaded by third-party sites — it is a supported product surface rather than
  something the provider is sneaking past.
- **Facebook shows a login wall for much of its catalogue.** That is reported as
  `502 upstream_blocked`, not `404`, because the remedy differs: authenticate,
  or accept the gap. A `404` would claim the video does not exist.
- **The payloads are doubly-escaped.** Meta serialises JSON into a JSON string
  inside a bootstrap script, so extraction unescapes first and then pulls out
  balanced objects by key (`scan.go`) rather than modelling the whole document,
  which changes shape constantly.
- **Rendered figures are a last resort.** When no payload is present, Instagram's
  `og:description` (`"1.2M views, 40,102 likes"`) is parsed, and the provider
  logs a warning: abbreviated figures are rounded, so 1.2M comes back as
  1,200,000 rather than the true value. Exact figures always win when available.
- **Zero is never reported as a measurement.** Meta omits counters by sending
  zero, so a zero counter is left absent, matching how `VideoStats` treats
  hidden counters everywhere else.
- **Egress matters, and it was not verifiable from the development network.**
  Measured from a datacenter IP, `instagram.com` answered every post and embed
  URL with a logged-out JavaScript shell carrying no payload and no OpenGraph
  tags — under browser, `facebookexternalhit`, `Twitterbot` and `Googlebot`
  user agents alike — so the extraction paths above are covered by unit tests
  against realistic fixtures but have not been confirmed end to end. On such a
  network the provider degrades exactly as designed, returning
  `502 upstream_blocked` rather than a fabricated zero. Verify from your own
  egress before depending on it, and expect a residential or proxied IP to be
  the difference.
- **It fails loudly.** A changed page structure returns `502 upstream_error`.
- **Automated access to the public pages is contrary to Meta's Terms of
  Service.** Whether to run it is the deploying party's decision:
  `META_PAGE_FETCH=false` leaves only the Graph path (which disables Instagram
  entirely), and `META_ENABLED=false` turns the provider off and returns `503`
  for Meta URLs.

Clients should treat `502 upstream_blocked` on a TikTok URL as retryable. On a
Facebook URL it usually is not: it means a login wall, which will still be there
on the retry.

## Layout

```
cmd/api/                  process entry point: config → wiring → serve
internal/
  config/                 env-driven config with defaults + validation
  domain/                 VideoStats, Lookup, Provider interface, sentinels
  stats/                  application layer: provider lookup + TTL cache
  api/                    HTTP transport: handlers, router, error mapping
  docs/                   embedded OpenAPI 3.1 spec + Swagger UI handlers
  httpx/                  server lifecycle, middleware, JSON envelopes
  auth/                   argon2id, sessions, CSRF, login rate limiting
  lookup/                 fetch a URL's stats and record it against an account
  web/                    server-rendered dashboard
    templates/            html/template pages and partials, embedded
    static/               one stylesheet, one script, embedded
  storage/postgres/       pool, migrations, repositories (users, sessions,
                          lookups, rate limits)
  provider/               registry that resolves a URL to its provider
    youtube/              YouTube Data API v3 + URL parsing
    meta/                 instagram embed + facebook page/graph extraction
    tiktok/               public page state extraction, with retry
  platform/httpclient/    shared outbound HTTP client
```

Dependencies point inward: `api` → `stats` → `domain` ← `provider/*`. Providers
never import `net/http` status codes, and `api/errors.go` is the single place
that maps a domain error onto an HTTP status.

## Adding a platform

1. Create `internal/provider/<name>/`.
2. Implement `domain.Provider` — `Platform()`, `Match(url)` (no I/O), `Stats(ctx, url)`.
3. Map failures onto the `domain.Err*` sentinels so the HTTP layer classifies them.
4. Add config to `internal/config/config.go` and `.env.example`.
5. Register it in `cmd/api/main.go`'s `NewRegistry(...)` call.
6. Add the platform to the `Platform` enum in `internal/docs/openapi.yaml`.

Nothing else changes — no handler, router, or service edits.

A provider that is registered but switched off returns `503
provider_unavailable` rather than `unsupported_platform`, so a caller can tell
"this deployment does not run that platform" from "that URL is not a video".

## Operations

- **Liveness and readiness are different checks.** `/healthz` reports whether
  the process is alive and deliberately does not touch the database; `/readyz`
  pings it and returns `503` when it is unreachable. Restarting a process does
  not repair a database, so putting the dependency in liveness would drain a
  fleet that would otherwise recover on its own — readiness takes the instance
  out of the load balancer and puts it back when the database returns.
- **Migrations run at boot** behind an advisory lock, so a rolling deploy of
  several replicas cannot race. `MIGRATE_ON_BOOT=false` plus
  `socialstats -migrate-only` splits them into a separate step. They are
  forward-only, and editing an applied migration is refused at startup.
- Structured `slog` output: text in development, JSON elsewhere.
- Graceful shutdown on SIGINT/SIGTERM, draining in-flight requests.
- Timeouts at every layer: read/write/idle, per-handler, per-upstream-call.
- Panics are recovered per request and logged with a stack.
- The cache is per-process; swap `stats.Cache` for Redis behind `Get`/`Set`
  when running more than one replica.
- Boot fails fast if `YOUTUBE_API_KEY` or `DATABASE_URL` is missing.
- **Static assets carry a content hash** and are served `immutable` with a
  one-year max-age; a changed file is a changed URL, so a deploy invalidates
  caches without anyone remembering to bump a version. Pages themselves are
  `no-store`: they are per-account and must not be held by a shared proxy.
- **Set `SESSION_COOKIE_SECURE=true` behind TLS** — it is forced off only when
  `APP_ENV=development`, because a `Secure` cookie is never sent over plain
  HTTP and the symptom (sign-in appears to work, every page is then anonymous)
  is a confusing afternoon.

## Development

```bash
make test     # unit tests, no database needed
make test-db  # integration tests against the :5433 database
make lint     # fmt + vet + test
make build    # ./bin/socialstats
make docker

make db-up    # start postgres (dev :5434, disposable test db :5433)
make migrate  # apply pending migrations
make db-reset # destroy the dev database and rebuild from migrations
make db-shell # psql against the dev database
```

Both targets run with the race detector, which needs cgo and a C compiler. On a
machine without one, `make test RACE=` and `make test-db RACE=` run the same
tests without it.

Repository tests run against a real PostgreSQL rather than a mock, because
most of what the storage layer relies on — advisory locks, transactional DDL,
SQLSTATE codes, `SKIP LOCKED` — is behaviour a mock would only assert back at
us. They skip themselves when `TEST_DATABASE_URL` is unset, so `make test`
stays fast and dependency-free.

## Not built yet

Named honestly, because a list of what is missing is more useful than a list of
what is done.

**Deliberate omissions, with the reasoning:**

- **Per-caller rate limiting on the API.** Only the sign-in path is limited
  today. A lookup costs an upstream call, so an account looping on `POST
  /v1/lookups` can spend the shared YouTube quota or earn a TikTok block. The
  TTL cache absorbs a repeated URL, but not a list of different ones — this is
  the first thing to add if the service is opened up beyond a trusted group.
- **Metrics and tracing.** The logs are structured, which covers the questions
  that have come up so far. A Prometheus endpoint is a small change when
  somebody has a dashboard to put it on.
- **Email.** No verification, no password reset, no notifications. This is why
  registration discloses that an address already has an account — the
  alternative needs somewhere to send a message to.
- **Pagination.** The history returns the most recent `LOOKUP_HISTORY_LIMIT`
  (100) and stops. Older lookups are in the table and readable by id, but
  nothing pages back through them.

- **Scheduled re-checks.** Deliberately removed. If you want to watch a number
  move over time rather than read it once, that is a different product, and the
  polling machinery it needs — a claim queue, per-platform pacing, backoff,
  retirement rules — is what this service used to be.
