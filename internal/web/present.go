package web

import (
	"errors"
	"net/http"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
)

// Error presentation.
//
// The messages here are written for a reader rather than passed through from
// the API: "record not found" is accurate and unhelpful, and somebody looking
// at a dashboard needs to know what to do next, not what the storage layer
// called it.

// timezoneOptions is a short list rather than the full IANA database.
//
// Two thousand entries in a select is not a control anybody uses; these cover
// the common cases, and the field is display-only in any event since every
// reading is stored in UTC.
var commonTimezones = []string{
	"UTC",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin",
	"Europe/Madrid", "Europe/Rome", "Europe/Amsterdam", "Europe/Stockholm", "Europe/Warsaw",
	"Europe/Athens", "Europe/Istanbul", "Europe/Moscow",
	"Africa/Lagos", "Africa/Cairo", "Africa/Nairobi", "Africa/Johannesburg",
	"Asia/Dubai", "Asia/Karachi", "Asia/Dhaka", "Asia/Kolkata", "Asia/Bangkok",
	"Asia/Jakarta", "Asia/Singapore", "Asia/Hong_Kong", "Asia/Shanghai",
	"Asia/Tokyo", "Asia/Seoul", "Asia/Manila",
	"Australia/Perth", "Australia/Sydney", "Pacific/Auckland",
	"America/Sao_Paulo", "America/Argentina/Buenos_Aires", "America/Mexico_City",
	"America/New_York", "America/Toronto", "America/Chicago", "America/Denver",
	"America/Los_Angeles", "America/Vancouver", "America/Anchorage", "Pacific/Honolulu",
}

func timezoneOptions(selected string) []Option {
	out := make([]Option, 0, len(commonTimezones)+1)

	// A configured zone that is not on the list still has to appear, or saving
	// the form would silently move the account to UTC.
	known := false
	for _, tz := range commonTimezones {
		if tz == selected {
			known = true
		}
	}
	if !known && selected != "" {
		out = append(out, Option{Value: selected, Label: selected, Selected: true})
	}

	for _, tz := range commonTimezones {
		out = append(out, Option{Value: tz, Label: tz, Selected: tz == selected})
	}
	return out
}

// ---------- error presentation ----------

// renderError shows a domain error as a page.
//
// The messages are rewritten for a reader rather than passed through from the
// API: "record not found" is accurate and unhelpful, and somebody looking at a
// dashboard needs to know what to do next, not what the storage layer called it.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, err error) {
	status := statusFor(err)
	heading, message, backTo, backLabel := errorCopy(err, status)

	s.log.WarnContext(r.Context(), "rendering an error page",
		"status", status, "path", r.URL.Path, "error", err)

	s.render(w, r, status, "error.html", Page{
		Title: heading,
		Data: map[string]any{
			"Heading":   heading,
			"Message":   message,
			"BackTo":    backTo,
			"BackLabel": backLabel,
			"RequestID": httpx.RequestIDFrom(r.Context()),
		},
	})
}

// NotFound renders unknown pages in the site's own layout.
func (s *Server) NotFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "error.html", Page{
		Title: "Page not found",
		Data: map[string]any{
			"Heading":   "Page not found",
			"Message":   "That address does not exist.",
			"BackTo":    "/",
			"BackLabel": "Back to the dashboard",
			"RequestID": httpx.RequestIDFrom(r.Context()),
		},
	})
}

func errorCopy(err error, status int) (heading, message, backTo, backLabel string) {
	backTo, backLabel = "/", "Back to the dashboard"

	switch {
	case errors.Is(err, domain.ErrRecordNotFound), errors.Is(err, domain.ErrNotFound):
		return "Not found",
			"That lookup is not in your history. It may have been deleted, or the link may be wrong.",
			backTo, backLabel

	case errors.Is(err, domain.ErrRegistrationClosed):
		return "Registration is closed",
			"This deployment does not allow self-service sign-up. Ask an administrator for an account.",
			"/login", "Back to sign in"

	case errors.Is(err, domain.ErrCSRF):
		return "That form has expired",
			"For your safety the request was not carried out. Go back and try again.",
			backTo, backLabel

	case errors.Is(err, domain.ErrUnauthenticated):
		return "Please sign in", "You need to be signed in to see that.", "/login", "Sign in"

	case status >= 500:
		return "Something went wrong",
			"The page could not be loaded. This has been logged; try again in a moment.",
			backTo, backLabel

	default:
		return "That did not work", firstLine(err.Error()), backTo, backLabel
	}
}

// lookupFailureMessage explains why a URL could not be looked up.
func lookupFailureMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrUnsupported):
		return "That link is not from a platform this service reads. YouTube, TikTok, Instagram and Facebook links work."
	case errors.Is(err, domain.ErrInvalidURL):
		return "That does not look like a video link. Copy the address of the video itself rather than a profile or a search."
	case errors.Is(err, domain.ErrRateLimited):
		return "The platform is rate-limiting us right now. Try again in a few minutes."
	case errors.Is(err, domain.ErrNotFound):
		return "The platform says that video does not exist. Check the link is right and the video is public."
	case errors.Is(err, domain.ErrBlocked):
		return "The platform would not serve that video to us, so it could not be identified. " +
			"Short links sometimes fail this way — try the full address."
	case errors.Is(err, domain.ErrMisconfigured):
		return "That platform is not enabled on this deployment."
	default:
		return "That video could not be looked up. Try again in a moment."
	}
}

func signInFailureMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrTooManyAttempts):
		return "Too many attempts. Wait a few minutes and try again."
	case errors.Is(err, domain.ErrInvalidCredentials):
		// Identical for a wrong password and an unknown address, deliberately:
		// any difference turns this form into a way to find out which addresses
		// have accounts.
		return "That email and password do not match an account."
	default:
		return "Sign-in is not working at the moment. Try again shortly."
	}
}

func registerFailureMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrConflict):
		return "That email address already has an account. Try signing in instead."
	case errors.Is(err, domain.ErrWeakPassword):
		return firstLine(err.Error())
	case errors.Is(err, domain.ErrInvalidURL):
		return "That does not look like an email address."
	case errors.Is(err, domain.ErrRegistrationClosed):
		return "This deployment does not allow self-service sign-up."
	default:
		return "The account could not be created. Try again shortly."
	}
}

func passwordFailureMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials):
		return "Your current password is not right."
	case errors.Is(err, domain.ErrWeakPassword):
		return firstLine(err.Error())
	default:
		return "The password could not be changed. Try again shortly."
	}
}

// firstLine keeps a message to its readable part, since sentinels wrap detail
// after a colon and some of it is for logs rather than people.
func firstLine(s string) string {
	if idx := indexOf(s, ": "); idx > 0 && len(s) > idx+2 {
		return upperFirst(s[idx+2:])
	}
	return upperFirst(s)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}

// statusFor maps a domain error to the status its page is rendered with.
//
// It duplicates a little of the API's mapping on purpose: the two layers answer
// different questions. The API's table is a published contract that clients
// branch on, and coupling the HTML pages to it would mean a page could not
// change how it presents a failure without altering that contract.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, domain.ErrUnauthenticated), errors.Is(err, domain.ErrInvalidCredentials):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrCSRF), errors.Is(err, domain.ErrRegistrationClosed):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrRecordNotFound), errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, domain.ErrLimitReached):
		return http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrTooManyAttempts):
		return http.StatusTooManyRequests
	case errors.Is(err, domain.ErrWeakPassword), errors.Is(err, domain.ErrInvalidURL),
		errors.Is(err, domain.ErrUnsupported):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
