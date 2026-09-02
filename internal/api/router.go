package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/docs"
	"github.com/foodibd/socialstats/internal/httpx"
)

// RouterConfig tunes transport-level behaviour of the mux.
type RouterConfig struct {
	HandlerTimeout time.Duration

	// DocsEnabled exposes /docs and /openapi.yaml. Turn it off on deployments
	// that must not publish their API surface.
	DocsEnabled bool

	// Auth serves the account endpoints and protects the rest. It is optional
	// so the router stays usable in tests that are not about authentication.
	Auth *AuthHandler

	// Middleware attaches the caller's identity and enforces CSRF.
	Middleware *auth.Middleware

	// Lookups serves the dashboard's data endpoints.
	Lookups *LookupHandler

	// Web registers the server-rendered dashboard. It is optional: a deployment
	// can serve the JSON API alone.
	Web WebRoutes
}

// WebRoutes is the dashboard's route registration, declared as an interface so
// this package does not import the web package. The dependency runs one way —
// the composition root wires them together — which keeps the JSON API buildable
// and testable without the dashboard.
type WebRoutes interface {
	Routes(mux *http.ServeMux, mw *auth.Middleware)

	// NotFoundHandler renders unknown paths, deferring to the JSON fallback for
	// callers that are not browsers.
	NotFoundHandler(jsonFallback http.HandlerFunc) http.HandlerFunc
}

// NewRouter builds the fully wrapped application handler. Routes use the
// stdlib method+pattern syntax (Go 1.22+), so no third-party router is needed.
func NewRouter(h *Handler, log *slog.Logger, cfg RouterConfig) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /readyz", h.Ready)
	mux.HandleFunc("GET /v1/stats", h.GetStats)
	mux.HandleFunc("POST /v1/stats", h.PostStats)

	if cfg.Auth != nil && cfg.Middleware != nil {
		mw := cfg.Middleware

		// Registration and login are the two endpoints that must be reachable
		// without already being signed in, so they carry their own rate limits
		// rather than a session requirement.
		mux.HandleFunc("POST /v1/auth/register", cfg.Auth.Register)
		mux.HandleFunc("POST /v1/auth/login", cfg.Auth.Login)
		mux.HandleFunc("POST /v1/auth/logout", cfg.Auth.Logout)

		// Everything below needs an identity. Require is applied per route
		// rather than to a prefix, because a route that is protected by being
		// inside the right subtree stops being protected the moment someone
		// adds one outside it.
		mux.Handle("GET /v1/auth/me", mw.Require(http.HandlerFunc(cfg.Auth.Me)))
		mux.Handle("POST /v1/auth/logout-all", mw.Require(http.HandlerFunc(cfg.Auth.LogoutEverywhere)))
		mux.Handle("POST /v1/auth/password", mw.Require(http.HandlerFunc(cfg.Auth.ChangePassword)))
	}

	if cfg.Lookups != nil && cfg.Middleware != nil {
		mw := cfg.Middleware

		// Every lookup route is scoped to the caller, so Require is applied to
		// each one individually. Protecting a subtree instead would mean the
		// next route added outside it is silently public.
		for pattern, handler := range map[string]http.HandlerFunc{
			"GET /v1/lookups":         cfg.Lookups.List,
			"POST /v1/lookups":        cfg.Lookups.Create,
			"GET /v1/lookups/{id}":    cfg.Lookups.Get,
			"DELETE /v1/lookups/{id}": cfg.Lookups.Remove,
		} {
			mux.Handle(pattern, mw.Require(handler))
		}
	}

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

	if cfg.Web != nil && cfg.Middleware != nil {
		cfg.Web.Routes(mux, cfg.Middleware)
	}

	// The catch-all. Registered last so every real route wins, and it is a bare
	// "/" rather than "GET /" so an unknown path answers the same way whatever
	// method was used.
	//
	// A browser gets the site's own 404 page; anything else gets the JSON error
	// shape, so a 404 from a script still parses like every other error.
	notFound := h.NotFound
	if cfg.Web != nil {
		notFound = cfg.Web.NotFoundHandler(h.NotFound)
	}
	mux.HandleFunc("/", notFound)

	timeout := cfg.HandlerTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	handler := http.Handler(mux)

	// CSRF wraps the mux rather than individual routes so a new state-changing
	// endpoint is covered the moment it is added, instead of the moment
	// somebody remembers. It exempts safe methods and bearer-token requests
	// itself; see auth.Middleware.CSRF.
	if cfg.Middleware != nil {
		handler = cfg.Middleware.CSRF(handler)
	}

	return httpx.Chain(handler,
		httpx.RequestID,
		httpx.Logger(log),
		httpx.Recover(log),
		httpx.Timeout(timeout),
	)
}

// RespondError renders a domain error in the standard envelope. It is handed to
// the auth middleware so that package can report failures in this transport's
// format without importing it and creating a cycle.
func RespondError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := httpErrorFor(err)
	httpx.Error(w, r, status, code, message)
}
