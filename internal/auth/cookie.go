package auth

import (
	"net/http"
	"strings"
	"time"
)

// Cookie handling.
//
// The session cookie is the credential for browser traffic, so every attribute
// on it is load-bearing:
//
//	HttpOnly  keeps it out of reach of JavaScript, so an XSS bug cannot walk
//	          away with a signed-in session.
//	Secure    keeps it off plaintext connections. Off only in development,
//	          where there is no TLS to put it on.
//	SameSite  Lax stops the cookie riding along on cross-site form posts,
//	          which is most of CSRF handled before the token is even checked.
//	Path=/    so one cookie covers the whole app rather than surprising
//	          someone with a route that is silently signed out.

// CookieConfig describes how session and CSRF cookies are written.
type CookieConfig struct {
	// Name of the session cookie.
	Name string

	// Secure marks cookies as HTTPS-only. It is forced off in development,
	// because a Secure cookie over http is simply never sent and the symptom —
	// login appears to succeed, then every request is signed out — is a
	// genuinely confusing afternoon.
	Secure bool

	// Domain is left empty for a host-only cookie unless a deployment needs to
	// share sessions across subdomains.
	Domain string

	// TTL is the absolute lifetime.
	TTL time.Duration
}

// csrfCookieName is the CSRF cookie's name for this configuration.
//
// The __Host- prefix is a browser-enforced promise: the cookie must be Secure,
// have Path=/ and carry no Domain, which together mean no sibling subdomain can
// have set it. It is only legal on secure cookies, so development falls back to
// the plain name.
func (c CookieConfig) csrfCookieName() string {
	if c.Secure && c.Domain == "" {
		return "__Host-csrf"
	}
	return "csrf"
}

// WriteSession sets the session cookie.
func (c CookieConfig) WriteSession(w http.ResponseWriter, token Token, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    string(token),
		Path:     "/",
		Domain:   c.Domain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSession expires the session cookie.
//
// The attributes must match the ones it was set with — a browser treats a
// cookie with a different Path or Domain as a different cookie, and clearing
// the wrong one leaves the real session in place while telling the user they
// have signed out.
func (c CookieConfig) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     "/",
		Domain:   c.Domain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// WriteCSRF sets the CSRF cookie.
//
// Not HttpOnly, and that is the whole design: the page's own JavaScript has to
// read it to echo it back in a header. It is safe to expose because it is not a
// credential — knowing it proves nothing without also being able to send the
// session cookie, and a cross-origin attacker can do neither.
func (c CookieConfig) WriteCSRF(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.csrfCookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: false,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCSRF expires the CSRF cookie.
func (c CookieConfig) ClearCSRF(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.csrfCookieName(),
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// TokenFromRequest extracts a session token, preferring the Authorization
// header over the cookie.
//
// Supporting both is what lets one API serve a browser and a script. It also
// decides whether CSRF applies: a request authenticated by a header cannot be
// forged by another site, because a browser will not attach that header on
// their behalf, while a cookie is sent automatically and therefore can be. The
// second return value carries that distinction rather than leaving each handler
// to work it out.
func TokenFromRequest(r *http.Request, cookieName string) (token Token, fromCookie bool) {
	if header := r.Header.Get("Authorization"); header != "" {
		if value, ok := strings.CutPrefix(header, "Bearer "); ok {
			return Token(strings.TrimSpace(value)), false
		}
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		return Token(cookie.Value), true
	}
	return "", false
}
