package web

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// Flash is a one-shot message shown after a redirect.
//
// Every form here posts and then redirects rather than rendering the result of
// the POST directly, so that a refresh does not resubmit and the browser's back
// button behaves. That pattern needs somewhere to carry "it worked" across the
// redirect, and a short-lived cookie is the smallest thing that does it without
// server-side session storage or a query parameter that lingers in the URL and
// gets shared.
type Flash struct {
	Level   string // "success" | "error" | "info"
	Message string
}

const flashCookie = "ss_flash"

// setFlash queues a message for the next page render.
func (s *Server) setFlash(w http.ResponseWriter, level, message string) {
	// base64 because a cookie value cannot contain a comma, semicolon or space,
	// and these messages are prose.
	value := level + ":" + base64.RawURLEncoding.EncodeToString([]byte(message))

	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   60,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash reads and immediately clears any queued message.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	cookie, err := r.Cookie(flashCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}

	// Cleared as it is read: a flash that survived one render would reappear on
	// every subsequent page, which reads as the app repeating itself.
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	level, encoded, ok := strings.Cut(cookie.Value, ":")
	if !ok {
		return nil
	}
	message, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}

	switch level {
	case "success", "error", "info":
	default:
		level = "info"
	}
	return &Flash{Level: level, Message: string(message)}
}
