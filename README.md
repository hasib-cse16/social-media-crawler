# socialstats

An HTTP service that returns public view counts for a video URL. YouTube is
implemented today; Meta (Facebook/Instagram) and TikTok are wired into the
router and land behind the same interface.

Standard library only — no web framework, no ORM.

## Quick start

```bash
cp .env.example .env          # then set YOUTUBE_API_KEY
export $(grep -v '^#' .env | xargs)
make run
```

```bash
curl "localhost:8080/v1/stats?url=https://youtu.be/dQw4w9WgXcQ"
curl -XPOST localhost:8080/v1/stats -d '{"url":"https://www.youtube.com/shorts/dQw4w9WgXcQ"}'
curl localhost:8080/healthz
```

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

Swagger UI's CSS/JS come from the unpkg CDN. If your deployment cannot reach the
public internet, vendor `swagger-ui-dist` into a `static/` directory, embed it,
and swap the two URLs in `internal/docs/docs.go`.

The spec is the source of truth for the error contract: `ErrorCode` enumerates
every `code` the service can return alongside its HTTP status, and each response
carries worked examples (including the hidden-counter case, where `like_count`
is absent rather than `0`). `docs_test.go` asserts every route appears in the
spec, so adding an undocumented endpoint fails the build.

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
    tiktok/               matcher done, fetch pending credentials
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
