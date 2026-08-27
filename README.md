# socialstats

An HTTP service that returns public view counts for a video URL. YouTube and
TikTok are implemented; Meta (Facebook/Instagram) is wired into the router and
lands behind the same interface.

Standard library only — no web framework, no ORM.

## Quick start

```bash
cp .env.example .env          # then set YOUTUBE_API_KEY
make run
```

The service loads `.env` from the working directory (walking up a few parents,
so running from an IDE works too). **Real environment variables always win**, so
`.env` is inert in production, where you inject real env vars instead. There is
no dotenv dependency — see `internal/config/dotenv.go`.

Running from GoLand or another IDE needs no extra setup: just make sure the run
configuration's working directory is inside the repository. Alternatively, set
`YOUTUBE_API_KEY` in the run configuration's environment.

```bash
curl "localhost:8080/v1/stats?url=https://youtu.be/dQw4w9WgXcQ"
curl "localhost:8080/v1/stats?url=https://www.tiktok.com/@user/video/7249376077976472833"
curl -XPOST localhost:8080/v1/stats -d '{"url":"https://www.youtube.com/shorts/dQw4w9WgXcQ"}'
curl localhost:8080/healthz
```

Only YouTube needs a credential. TikTok works with no setup.

Interactive API reference: **<http://localhost:8080/docs>** (Swagger UI).
Raw spec: **<http://localhost:8080/openapi.yaml>**.

Get an API key: Google Cloud Console → enable **YouTube Data API v3** → create
an API key. `videos.list` costs 1 unit per call against a 10,000/day quota,
which is why responses are cached.

## API

| Method | Path          | Notes                                     |
|--------|---------------|-------------------------------------------|
| GET    | `/v1/stats`   | `?url=<video url>`                        |
| POST   | `/v1/stats`   | body `{"url":"..."}`                      |
| GET    | `/healthz`    | liveness + registered platforms           |
| GET    | `/readyz`     | readiness                                 |
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
| Meta     | not implemented | — | returns `501` |

TikTok returns `share_count` and `save_count` in addition to the common
counters; YouTube does not expose those, so they are absent for YouTube videos.

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

Clients should treat `502 upstream_blocked` on a TikTok URL as retryable.

## Layout

```
cmd/api/                  process entry point: config → wiring → serve
internal/
  config/                 env-driven config with defaults + validation
  domain/                 VideoStats, Provider interface, sentinel errors
  stats/                  application layer: provider lookup + TTL cache
  api/                    HTTP transport: handlers, router, error mapping
  docs/                   embedded OpenAPI 3.1 spec + Swagger UI handlers
  httpx/                  server lifecycle, middleware, JSON envelopes
  provider/               registry that resolves a URL to its provider
    youtube/              YouTube Data API v3 + URL parsing
    meta/                 matcher done, fetch pending credentials
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

For Meta and TikTok the matchers already exist, so those URLs return a clear
`501 not_implemented` rather than `unsupported_platform`; filling in `Stats` is
the only remaining work.

## Operations

- Structured `slog` output: text in development, JSON elsewhere.
- Graceful shutdown on SIGINT/SIGTERM, draining in-flight requests.
- Timeouts at every layer: read/write/idle, per-handler, per-upstream-call.
- Panics are recovered per request and logged with a stack.
- The cache is per-process; swap `stats.Cache` for Redis behind `Get`/`Set`
  when running more than one replica.
- Boot fails fast if `YOUTUBE_API_KEY` is missing.

## Development

```bash
make test     # go test -race ./...
make lint     # fmt + vet + test
make build    # ./bin/socialstats
make docker
```

## Not included (deliberate next steps)

Auth on the API itself, per-caller rate limiting, retry with backoff on 5xx,
Prometheus metrics, and persistence for historical view-count tracking.
