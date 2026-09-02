package web

import (
	"net/http"
	"strings"

	"github.com/foodibd/socialstats/internal/auth"
)

// Routes registers the dashboard's pages on a mux.
//
// Registration lives here rather than in the api package so that adding a page
// touches one file, and so the api package does not have to know this one
// exists. The router calls it; nothing else does.
func (s *Server) Routes(mux *http.ServeMux, mw *auth.Middleware) {
	// Static assets are public and unauthenticated: they are the stylesheet and
	// one script, and putting them behind a session would break the sign-in
	// page they style.
	mux.Handle("GET "+AssetPrefix, s.StaticHandler())

	// Anonymous pages. Optional attaches an identity when there is one, so an
	// already-signed-in visitor hitting /login is redirected onward rather than
	// shown a form they do not need.
	//
	// IssueCSRF runs on the GETs so the token is in the cookie before the form
	// that needs it renders — a form rendered without one fails its first
	// submission and looks like a bug.
	anon := func(h http.HandlerFunc) http.Handler {
		return mw.Optional(mw.IssueCSRF(h))
	}

	mux.Handle("GET /login", anon(s.LoginForm))
	mux.Handle("POST /login", mw.Optional(http.HandlerFunc(s.Login)))
	mux.Handle("GET /register", anon(s.RegisterForm))
	mux.Handle("POST /register", mw.Optional(http.HandlerFunc(s.Register)))
	mux.Handle("POST /logout", mw.Optional(http.HandlerFunc(s.Logout)))

	// Pages that need an account. Require is applied per route rather than to a
	// prefix: a route added outside the prefix later would be silently public,
	// and the compiler cannot warn about that.
	page := func(h http.HandlerFunc) http.Handler {
		return mw.Require(mw.IssueCSRF(h))
	}
	action := func(h http.HandlerFunc) http.Handler {
		return mw.Require(h)
	}

	// "/{$}" anchors to exactly the root. A bare "GET /" would swallow every
	// unmatched GET and turn a typo into the dashboard.
	mux.Handle("GET /{$}", page(s.Dashboard))
	mux.Handle("GET /lookups/{id}", page(s.LookupDetail))
	mux.Handle("GET /settings", page(s.Settings))

	mux.Handle("POST /lookups", action(s.Lookup))
	// A form post rather than DELETE, because HTML forms speak only GET and
	// POST. The route says what it does instead of smuggling a method override.
	mux.Handle("POST /lookups/{id}/delete", action(s.RemoveLookup))
	mux.Handle("POST /settings", action(s.SaveProfile))
	mux.Handle("POST /settings/password", action(s.ChangePassword))
	mux.Handle("POST /logout-all", action(s.LogoutEverywhere))
}

// ErrorResponder returns a responder that renders HTML for browsers and defers
// to the API's JSON for everything else.
//
// One middleware serves both surfaces, so a route cannot be protected in one
// and not the other. What differs is only the presentation, which is decided
// per request: an anonymous browser navigating to a page should land on the
// sign-in form, while a script calling the same URL wants a 401 it can branch
// on — a redirect to an HTML login page is useless to it.
func (s *Server) ErrorResponder(jsonResponder auth.ErrorResponder) auth.ErrorResponder {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		if !prefersHTML(r) {
			jsonResponder(w, r, err)
			return
		}
		if isUnauthenticated(err) {
			s.redirectToLogin(w, r)
			return
		}
		s.renderError(w, r, err)
	}
}

// prefersHTML reports whether this request came from a browser navigating,
// rather than from a script.
//
// Sec-Fetch-Mode is the reliable signal and every current browser sends it: it
// distinguishes a top-level navigation from a fetch() the same page made, which
// Accept alone cannot. Accept is the fallback for older clients, and an explicit
// preference for JSON always wins so an API client is never redirected.
func prefersHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return false
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return false
	}

	switch r.Header.Get("Sec-Fetch-Mode") {
	case "navigate":
		return true
	case "cors", "no-cors", "same-origin", "websocket":
		return false
	}
	return strings.Contains(accept, "text/html")
}

func isUnauthenticated(err error) bool {
	return statusFor(err) == http.StatusUnauthorized
}

// NotFoundHandler renders unknown paths as the site's 404 page for browsers,
// and defers to the JSON fallback for everything else.
func (s *Server) NotFoundHandler(jsonFallback http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if prefersHTML(r) {
			s.NotFound(w, r)
			return
		}
		jsonFallback(w, r)
	}
}
