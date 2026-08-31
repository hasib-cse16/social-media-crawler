package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/foodibd/socialstats/internal/docs"
	"github.com/foodibd/socialstats/internal/httpx"
)

// RouterConfig tunes transport-level behaviour of the mux.
type RouterConfig struct {
	HandlerTimeout time.Duration

	// DocsEnabled exposes /docs and /openapi.yaml. Turn it off on deployments
	// that must not publish their API surface.
	DocsEnabled bool
}

// NewRouter builds the fully wrapped application handler. Routes use the
// stdlib method+pattern syntax (Go 1.22+), so no third-party router is needed.
func NewRouter(h *Handler, log *slog.Logger, cfg RouterConfig) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /readyz", h.Ready)
	mux.HandleFunc("GET /v1/stats", h.GetStats)
	mux.HandleFunc("POST /v1/stats", h.PostStats)

	if cfg.DocsEnabled {
		ui := docs.UIHandler(docs.SpecPath)

		mux.HandleFunc("GET "+docs.SpecPath, docs.SpecHandler())
		mux.HandleFunc("GET /docs", ui)
		// Swagger UI links are commonly shared as /docs/.
		mux.Handle("GET /docs/", http.RedirectHandler("/docs", http.StatusMovedPermanently))

		// /swagger/index.html is the well-known location other Go stacks use
		// (gin-swagger, swaggo). Honour it so habit and copied links both work.
		mux.HandleFunc("GET /swagger/index.html", ui)
		mux.Handle("GET /swagger", http.RedirectHandler("/docs", http.StatusMovedPermanently))
		mux.Handle("GET /swagger/", http.RedirectHandler("/docs", http.StatusMovedPermanently))
	}

	mux.HandleFunc("/", h.NotFound)

	timeout := cfg.HandlerTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	return httpx.Chain(mux,
		httpx.RequestID,
		httpx.Logger(log),
		httpx.Recover(log),
		httpx.Timeout(timeout),
	)
}
