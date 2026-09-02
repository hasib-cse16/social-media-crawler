// Package api is the HTTP transport for the stats service: it parses requests,
// calls the service, and renders responses. It contains no business logic.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/stats"
)

const maxRequestBody = 64 << 10 // 64 KiB

// StatsService is the behaviour the handlers need from the application layer.
type StatsService interface {
	ByURL(ctx context.Context, rawURL string) (*stats.Result, error)
	Platforms() []domain.Platform
}

// Prober reports whether a dependency this service cannot work without is
// currently reachable. *postgres.DB satisfies it.
type Prober interface {
	Ping(ctx context.Context) error
}

// Handler holds the handler dependencies.
type Handler struct {
	svc     StatsService
	log     *slog.Logger
	version string

	// db is probed by /readyz. It is an interface rather than a concrete type
	// so this package never imports the storage layer.
	db Prober
}

func NewHandler(svc StatsService, log *slog.Logger, version string, db Prober) *Handler {
	return &Handler{svc: svc, log: log, version: version, db: db}
}

// statsResponse is the wire shape of a stats result.
type statsResponse struct {
	*domain.VideoStats
	Cached bool `json:"cached"`
}

// GetStats handles GET /v1/stats?url=...
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if rawURL == "" {
		httpx.Error(w, r, http.StatusBadRequest, "missing_url", "query parameter 'url' is required")
		return
	}
	h.respondStats(w, r, rawURL)
}

// statsRequest is the POST /v1/stats body.
type statsRequest struct {
	URL string `json:"url"`
}

// PostStats handles POST /v1/stats with a JSON body, for URLs that are awkward
// to pass in a query string.
func (h *Handler) PostStats(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body", "could not read request body")
		return
	}

	var req statsRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body", "body must be a json object with a 'url' field")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		httpx.Error(w, r, http.StatusBadRequest, "missing_url", "field 'url' is required")
		return
	}
	h.respondStats(w, r, strings.TrimSpace(req.URL))
}

func (h *Handler) respondStats(w http.ResponseWriter, r *http.Request, rawURL string) {
	result, err := h.svc.ByURL(r.Context(), rawURL)
	if err != nil {
		status, code, message := httpErrorFor(err)
		// Client-caused errors are noise at error level; upstream ones are not.
		if status >= 500 {
			h.log.ErrorContext(r.Context(), "stats request failed", "error", err, "url", rawURL, "code", code)
		} else {
			h.log.WarnContext(r.Context(), "stats request rejected", "error", err, "url", rawURL, "code", code)
		}
		httpx.Error(w, r, status, code, message)
		return
	}

	if result.Cached {
		w.Header().Set("X-Cache", "hit")
	} else {
		w.Header().Set("X-Cache", "miss")
	}
	httpx.Data(w, r, http.StatusOK, statsResponse{VideoStats: result.Stats, Cached: result.Cached})
}

// Health handles GET /healthz: is this process alive?
//
// Liveness deliberately does not touch the database. A failing liveness probe
// gets the container killed and restarted, and restarting the application does
// not fix a database that is down — it just removes capacity from a fleet that
// will recover on its own the moment the database comes back. Dependencies
// belong in readiness, not liveness.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status":    "ok",
		"version":   h.version,
		"platforms": h.svc.Platforms(),
	}
	httpx.Data(w, r, http.StatusOK, body)
}

// Ready handles GET /readyz: can this process serve traffic?
//
// It returns 503 when the database is unreachable, which takes the instance out
// of the load balancer without restarting it.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	if h.db != nil {
		if err := h.db.Ping(r.Context()); err != nil {
			h.log.WarnContext(r.Context(), "readiness check failed", "dependency", "database", "error", err)
			checks["database"] = "unreachable"
			ready = false
		} else {
			checks["database"] = "ok"
		}
	}

	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}

	httpx.Data(w, r, status, map[string]any{
		"status":  state,
		"version": h.version,
		"checks":  checks,
	})
}

// NotFound renders unknown routes in the standard error shape.
func (h *Handler) NotFound(w http.ResponseWriter, r *http.Request) {
	httpx.Error(w, r, http.StatusNotFound, "route_not_found", "no such endpoint")
}
