# socialstats

An HTTP service that returns public view counts for a video URL. YouTube,
TikTok and Meta (Facebook + Instagram) are implemented behind one interface.

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
curl "localhost:8080/v1/stats?url=https://www.instagram.com/reel/Cx1y2z3AbCd/"
curl "localhost:8080/v1/stats?url=https://www.facebook.com/watch/?v=1234567890"
curl -XPOST localhost:8080/v1/stats -d '{"url":"https://www.youtube.com/shorts/dQw4w9WgXcQ"}'
curl localhost:8080/healthz
```

Only YouTube needs a credential. TikTok and Meta work with no setup.

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

Accepted Meta URL forms: Instagram `/p/`, `/reel/`, `/reels/`, `/tv/`, with or
without a leading handle; Facebook `/watch/?v=`, `/reel/<id>`, `/<page>/videos/
[slug/]<id>`, `video.php?v=`, and the `fb.watch` / `/share/v/` short links.

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
  domain/                 VideoStats, Provider interface, sentinel errors
  stats/                  application layer: provider lookup + TTL cache
  api/                    HTTP transport: handlers, router, error mapping
  docs/                   embedded OpenAPI 3.1 spec + Swagger UI handlers
  httpx/                  server lifecycle, middleware, JSON envelopes
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
