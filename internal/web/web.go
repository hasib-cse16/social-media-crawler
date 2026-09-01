package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/tracking"
)

//go:embed templates/*.html templates/partials/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server renders the dashboard.
type Server struct {
	tracking *tracking.Service
	auth     *auth.Service
	log      *slog.Logger

	templates map[string]*template.Template
	version   string

	// assetPath carries a hash of the embedded assets, so a deploy invalidates
	// caches without anyone remembering to bump a query string — and so the
	// files can be served with a long max-age, which is what makes that
	// worthwhile.
	assetPath     string
	secureCookies bool

	// registrationOpen mirrors the auth service's setting, so the sign-in page
	// only offers a "create one" link on a deployment that would honour it.
	registrationOpen bool

	// trustProxy decides whether X-Forwarded-For is believed when attributing
	// a sign-in attempt to a source address.
	trustProxy bool
}

// Config is what the dashboard needs to render.
type Config struct {
	Version string

	// SecureCookies must match the session cookie's setting, or the flash
	// cookie is dropped in production and success messages silently vanish.
	SecureCookies bool

	// RegistrationOpen mirrors the auth setting, so the sign-in page does not
	// offer a link to a page that will refuse.
	RegistrationOpen bool

	// TrustProxyHeaders mirrors the auth setting for client-address handling.
	TrustProxyHeaders bool
}

// New builds the dashboard server, parsing every template up front.
//
// Parsing at boot rather than per request is not only faster: a template with a
// typo in it fails the process at startup, where a deploy notices, instead of
// on the one page nobody visited before release.
func New(trackingSvc *tracking.Service, authSvc *auth.Service, cfg Config, log *slog.Logger) (*Server, error) {
	s := &Server{
		tracking:         trackingSvc,
		auth:             authSvc,
		log:              log.With("component", "web"),
		version:          cfg.Version,
		secureCookies:    cfg.SecureCookies,
		registrationOpen: cfg.RegistrationOpen,
		trustProxy:       cfg.TrustProxyHeaders,
	}

	if err := s.parseTemplates(); err != nil {
		return nil, err
	}

	hash, err := hashAssets()
	if err != nil {
		return nil, fmt.Errorf("hash static assets: %w", err)
	}
	s.assetPath = "/static/" + hash

	return s, nil
}

// pages are the templates that get a full layout. Each is parsed together with
// the layout and every partial, so a partial is available to all of them
// without being listed one page at a time.
var pages = []string{
	"dashboard.html",
	"video.html",
	"login.html",
	"register.html",
	"settings.html",
	"error.html",
}

func (s *Server) parseTemplates() error {
	partials, err := fs.Glob(templateFS, "templates/partials/*.html")
	if err != nil {
		return fmt.Errorf("list partials: %w", err)
	}

	s.templates = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		files := append([]string{"templates/layout.html", "templates/" + page}, partials...)

		tmpl, err := template.New("layout.html").Funcs(s.funcMap()).ParseFS(templateFS, files...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", page, err)
		}
		s.templates[page] = tmpl
	}
	return nil
}

// funcMap exposes the formatting helpers to templates.
func (s *Server) funcMap() template.FuncMap {
	return template.FuncMap{
		"compact":      compact,
		"compactPtr":   compactPtr,
		"exact":        exact,
		"comma":        comma,
		"signed":       signed,
		"deltaClass":   deltaClass,
		"ago":          ago,
		"timestamp":    timestamp,
		"duration":     duration,
		"platformName": platformName,
		"truncate":     truncate,
		"plural":       pluralise,
		"percent":      percent,
		"itoa":         itoa,
		"windowLabel":  windowLabel,
		"queryString":  queryString,
		"asset":        s.asset,
		"lower":        strings.ToLower,
	}
}

// asset resolves a static file to its cache-busted path.
func (s *Server) asset(name string) string { return s.assetPath + "/" + name }

// render writes a page.
//
// It renders into a buffer first. Writing straight to the ResponseWriter means a
// template error halfway through leaves a half-page already sent with a 200
// status and no way to correct it; buffering makes the failure recoverable and
// the error page possible.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data Page) {
	tmpl, ok := s.templates[page]
	if !ok {
		s.log.ErrorContext(r.Context(), "no such template", "page", page)
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}

	data.User = auth.UserFrom(r.Context())
	data.CSRFToken = auth.CSRFTokenFrom(r.Context())
	data.Version = s.version
	data.Now = time.Now()
	if data.Flash == nil {
		data.Flash = s.takeFlash(w, r)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		s.log.ErrorContext(r.Context(), "render failed", "page", page, "error", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages are per-user and change as the poller runs; caching them would show
	// somebody else's dashboard from a shared proxy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.String())
}

// StaticHandler serves the embedded assets.
//
// The path carries a content hash, so the files are immutable by construction
// and can be cached for a year: a changed file is a changed URL.
func (s *Server) StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a build-time mistake.
		panic("web: static assets are not embedded: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the hash segment; it exists to change the URL, not to select a
		// file.
		trimmed := strings.TrimPrefix(r.URL.Path, "/static/")
		if slash := strings.Index(trimmed, "/"); slash >= 0 {
			trimmed = trimmed[slash+1:]
		}

		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + path.Clean(trimmed)
		files.ServeHTTP(w, r2)
	})
}

// AssetPrefix is the route prefix the static handler is mounted at.
const AssetPrefix = "/static/"

// platformsFor lists the platforms this deployment tracks, for the filter.
func (s *Server) platformsFor() []domain.Platform { return s.tracking.Platforms() }

// hashAssets fingerprints the embedded static files.
//
// One hash for all of them rather than one per file: the set is three small
// files that ship together, and a single prefix keeps the template helper to a
// name lookup instead of a manifest.
func hashAssets() (string, error) {
	sum := sha256.New()

	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		// The name is hashed too, so renaming a file changes the fingerprint
		// even when its contents did not.
		sum.Write([]byte(p))
		sum.Write(body)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}
