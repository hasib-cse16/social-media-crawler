package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/tracking"
)

// Input validation and error presentation.
//
// Query parameters here are validated by whitelist rather than sanitised: a
// dashboard has a fixed set of sorts and ranges, and accepting anything else
// only creates a way for a bad value to reach a query.

var validSorts = map[string]bool{
	"views": true, "gained": true, "recent": true, "title": true, "fetched": true,
}

var validWindows = map[string]bool{
	"24h": true, "168h": true, "720h": true, "2160h": true,
}

func validPlatform(p string) bool {
	switch domain.Platform(p) {
	case domain.PlatformYouTube, domain.PlatformTikTok, domain.PlatformMeta:
		return true
	default:
		return false
	}
}

// firstOf returns value when it is allowed, and the fallback otherwise.
func firstOf(value, fallback string, allowed map[string]bool) string {
	if allowed[value] {
		return value
	}
	return fallback
}

// bucketFor picks a chart resolution for a range.
//
// A 90-day range at six-hourly readings is 360 points across a 720-pixel chart:
// two pixels each, which is noise rather than detail. Bucketing keeps the shape
// and drops the crowding.
func bucketFor(window time.Duration) postgres.Bucket {
	switch {
	case window <= 48*time.Hour:
		return postgres.BucketRaw
	case window <= 30*24*time.Hour:
		return postgres.BucketHour
	default:
		return postgres.BucketDay
	}
}

func platformOptions(platforms []domain.Platform, selected string) []Option {
	out := make([]Option, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, Option{
			Value:    string(p),
			Label:    platformName(p),
			Selected: string(p) == selected,
		})
	}
	return out
}

func rangeOptions(selected string) []Option {
	out := make([]Option, 0, len(windowOptions))
	for _, o := range windowOptions {
		out = append(out, Option{Value: o.value, Label: o.label, Selected: o.value == selected})
	}
	return out
}

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

func toSummary(s *tracking.Summary) SummaryView {
	view := SummaryView{
		Tracked:     s.TrackedVideos,
		TotalViews:  compact(uint64(max(s.TotalViews, 0))),
		TotalExact:  comma(s.TotalViews) + " views",
		Gained:      signed(&s.ViewsGained),
		GainedClass: deltaClass(&s.ViewsGained),
		Stale:       s.Stale,
		Unavailable: s.Unavailable,
		Window:      s.Window,
	}
	for _, p := range s.Platforms {
		view.ByPlatform = append(view.ByPlatform, PlatformCount{
			Platform: p, Name: platformName(p), Count: s.ByPlatform[string(p)],
		})
	}
	return view
}

func toSchedule(v *domain.Video) ScheduleView {
	view := ScheduleView{
		Interval:    duration(v.Schedule.Interval),
		Failures:    v.Schedule.ConsecutiveFailures,
		LastError:   v.Schedule.LastFetchError,
		Retired:     v.Schedule.Retired(),
		RetiredAt:   v.Schedule.UnavailableSince,
		Trackers:    v.Schedule.TrackerCount,
		NextFetch:   "not scheduled",
		NextFetchAt: v.Schedule.NextFetchAt,
	}
	if at := v.Schedule.NextFetchAt; at != nil {
		if at.Before(time.Now()) {
			view.NextFetch = "due now"
		} else {
			view.NextFetch = "in " + duration(time.Until(*at).Round(time.Minute))
		}
	}
	return view
}

func toAttempts(attempts []domain.FetchAttempt) []AttemptView {
	out := make([]AttemptView, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, AttemptView{
			When:     ago(&a.StartedAt),
			WhenAt:   a.StartedAt,
			Status:   attemptLabel(a.Status),
			Tone:     attemptTone(a.Status),
			Duration: strconv.Itoa(a.DurationMS) + " ms",
			Error:    a.ErrorDetail,
		})
	}
	return out
}

func attemptLabel(s domain.AttemptStatus) string {
	switch s {
	case domain.AttemptOK:
		return "OK"
	case domain.AttemptNotFound:
		return "Not found"
	case domain.AttemptBlocked:
		return "Blocked"
	case domain.AttemptRateLimited:
		return "Rate limited"
	case domain.AttemptTimeout:
		return "Timed out"
	default:
		return "Failed"
	}
}

func attemptTone(s domain.AttemptStatus) string {
	switch s {
	case domain.AttemptOK:
		return "ok"
	case domain.AttemptNotFound:
		return "critical"
	default:
		return "warning"
	}
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
			"That video is not on your list. It may have been removed, or the link may be wrong.",
			backTo, backLabel

	case errors.Is(err, domain.ErrGone):
		return "No longer available",
			"The platform reported this video as removed, so it is no longer refreshed. " +
				"Everything already collected is kept.",
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

// addFailureMessage explains why a video could not be tracked.
func addFailureMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrUnsupported):
		return "That link is not from a platform this service tracks. YouTube, TikTok, Instagram and Facebook links work."
	case errors.Is(err, domain.ErrInvalidURL):
		return "That does not look like a video link. Copy the address of the video itself rather than a profile or a search."
	case errors.Is(err, domain.ErrLimitReached):
		return "You have reached the limit on tracked videos. Remove one to make room."
	case errors.Is(err, domain.ErrNotFound):
		return "The platform says that video does not exist. Check the link is right and the video is public."
	case errors.Is(err, domain.ErrBlocked):
		return "The platform would not serve that video to us, so it could not be identified. " +
			"Short links sometimes fail this way — try the full address."
	case errors.Is(err, domain.ErrMisconfigured):
		return "That platform is not enabled on this deployment."
	default:
		return "That video could not be added. Try again in a moment."
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
	case errors.Is(err, domain.ErrGone):
		return http.StatusGone
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
