package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/tracking"
)

// TrackingHandler serves the dashboard's data endpoints.
type TrackingHandler struct {
	svc *tracking.Service
}

func NewTrackingHandler(svc *tracking.Service) *TrackingHandler {
	return &TrackingHandler{svc: svc}
}

// defaultWindow is the period growth is measured over when none is asked for.
const defaultWindow = 7 * 24 * time.Hour

// List handles GET /v1/videos.
func (h *TrackingHandler) List(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	q := r.URL.Query()
	platform, err := parsePlatform(q.Get("platform"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	window, err := parseWindow(q.Get("window"))
	if err != nil {
		RespondError(w, r, err)
		return
	}

	limit := clampInt(q.Get("limit"), 50, 1, 200)
	offset := clampInt(q.Get("offset"), 0, 0, 100_000)

	entries, err := h.svc.List(r.Context(), tracking.ListQuery{
		UserID:    user.ID,
		Window:    window,
		Platform:  platform,
		Sort:      postgres.DashboardSort(q.Get("sort")),
		Limit:     limit,
		Offset:    offset,
		Sparkline: clampInt(q.Get("sparkline"), 0, 0, 200),
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, map[string]any{
		"videos": entries,
		"window": window.String(),
		"limit":  limit,
		"offset": offset,
		// The count is what a client needs to know whether to ask for the next
		// page; returning it beats making them infer it from a short page.
		"count": len(entries),
	})
}

// addRequest is the body of POST /v1/videos.
type addRequest struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// Add handles POST /v1/videos.
func (h *TrackingHandler) Add(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	var req addRequest
	if !decodeAddRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		httpx.Error(w, r, http.StatusBadRequest, "missing_url", "field 'url' is required")
		return
	}

	entry, err := h.svc.Add(r.Context(), user.ID, req.URL, req.Label)
	if err != nil {
		RespondError(w, r, err)
		return
	}

	// 201 whether or not the first fetch succeeded: the tracking was created,
	// which is what the request asked for. Whether the numbers arrived is
	// reported by the entry's own fetch status rather than by the status code.
	httpx.Data(w, r, http.StatusCreated, entry)
}

// Get handles GET /v1/videos/{id}.
func (h *TrackingHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	entry, err := h.svc.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, entry)
}

// updateRequest is the body of PATCH /v1/videos/{id}.
//
// Pointers, so that "not supplied" and "set to empty" are different requests: a
// PATCH that omits notes must leave them alone, and one that sends "" must
// clear them.
type updateRequest struct {
	Label *string `json:"label,omitempty"`
	Notes *string `json:"notes,omitempty"`
}

// Update handles PATCH /v1/videos/{id}.
func (h *TrackingHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	var req updateRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body",
			"body must be a json object with optional 'label' and 'notes'")
		return
	}
	if req.Label == nil && req.Notes == nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body", "supply at least one of 'label' or 'notes'")
		return
	}

	// PATCH means "change these fields", so the ones that were not sent are
	// read back and rewritten unchanged rather than being blanked.
	current, err := h.svc.Tracked(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		RespondError(w, r, err)
		return
	}

	label, notes := current.Label, current.Notes
	if req.Label != nil {
		label = *req.Label
	}
	if req.Notes != nil {
		notes = *req.Notes
	}

	entry, err := h.svc.Update(r.Context(), user.ID, r.PathValue("id"), label, notes)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, entry)
}

// Remove handles DELETE /v1/videos/{id}.
func (h *TrackingHandler) Remove(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	if err := h.svc.Remove(r.Context(), user.ID, r.PathValue("id")); err != nil {
		RespondError(w, r, err)
		return
	}

	// The history is deliberately kept: other users may track the same video,
	// and re-adding it later should restore what was collected.
	httpx.Data(w, r, http.StatusOK, map[string]any{
		"untracked":    true,
		"history_kept": true,
	})
}

// History handles GET /v1/videos/{id}/history.
func (h *TrackingHandler) History(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	q := r.URL.Query()
	from, err := parseTime(q.Get("from"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	to, err := parseTime(q.Get("to"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	bucket, err := parseBucket(q.Get("bucket"))
	if err != nil {
		RespondError(w, r, err)
		return
	}

	history, err := h.svc.HistoryFor(r.Context(), tracking.HistoryQuery{
		UserID: user.ID, PublicID: r.PathValue("id"),
		From: from, To: to, Bucket: bucket,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, history)
}

// Attempts handles GET /v1/videos/{id}/attempts.
func (h *TrackingHandler) Attempts(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	attempts, err := h.svc.Attempts(r.Context(), user.ID, r.PathValue("id"),
		clampInt(r.URL.Query().Get("limit"), 20, 1, 200))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, map[string]any{"attempts": attempts})
}

// Refresh handles POST /v1/videos/{id}/refresh.
func (h *TrackingHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	entry, err := h.svc.Refresh(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		RespondError(w, r, err)
		return
	}

	// 202: the refresh was scheduled, not performed. Fetching inline would let
	// one impatient user spend everyone's TikTok budget, and the poller is
	// already what paces platform access.
	httpx.Data(w, r, http.StatusAccepted, map[string]any{
		"queued": true,
		"video":  entry,
	})
}

// Summary handles GET /v1/dashboard/summary.
func (h *TrackingHandler) Summary(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	window, err := parseWindow(r.URL.Query().Get("window"))
	if err != nil {
		RespondError(w, r, err)
		return
	}

	summary, err := h.svc.Summarise(r.Context(), user.ID, window,
		clampInt(r.URL.Query().Get("movers"), 5, 0, 50))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, summary)
}

// ---------- query parsing ----------

// parsePlatform validates the platform filter. An unknown value is rejected
// rather than silently returning everything, because a typo that quietly
// changes what a query means is worse than one that fails.
func parsePlatform(raw string) (domain.Platform, error) {
	switch raw {
	case "":
		return "", nil
	case string(domain.PlatformYouTube), string(domain.PlatformTikTok), string(domain.PlatformMeta):
		return domain.Platform(raw), nil
	default:
		return "", httpx.Detail(domain.ErrInvalidURL,
			"unknown platform %q; expected youtube, tiktok or meta", raw)
	}
}

// parseWindow reads the growth window, accepting a duration string.
func parseWindow(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultWindow, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, httpx.Detail(domain.ErrInvalidURL, "window %q is not a duration, try 24h or 168h", raw)
	}
	if d <= 0 {
		return 0, httpx.Detail(domain.ErrInvalidURL, "window must be positive")
	}
	// A year of six-hourly readings is a large scan and nobody asks for it on
	// purpose; the cap turns a typo into an error rather than a slow query.
	if d > 365*24*time.Hour {
		return 0, httpx.Detail(domain.ErrInvalidURL, "window must be a year or less")
	}
	return d, nil
}

func parseBucket(raw string) (postgres.Bucket, error) {
	switch raw {
	case "", "raw":
		return postgres.BucketRaw, nil
	case "hour":
		return postgres.BucketHour, nil
	case "day":
		return postgres.BucketDay, nil
	default:
		return "", httpx.Detail(domain.ErrInvalidURL, "unknown bucket %q; expected raw, hour or day", raw)
	}
}

// parseTime accepts RFC 3339, which is what the responses emit — a client
// should be able to paste a timestamp it was given straight back.
func parseTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, httpx.Detail(domain.ErrInvalidURL,
			"%q is not an RFC 3339 timestamp, try 2026-09-01T00:00:00Z", raw)
	}
	return t, nil
}

// clampInt reads a bounded integer, falling back to a default rather than
// failing: an out-of-range limit is a client being optimistic, not an error
// worth refusing the whole request over.
func clampInt(raw string, def, lo, hi int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// decodeAddRequest accepts JSON or a form post, so the same endpoint serves the
// API and the browser form that will sit in front of it.
func decodeAddRequest(w http.ResponseWriter, r *http.Request, out *addRequest) bool {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/form-data") {

		if err := r.ParseForm(); err != nil {
			httpx.Error(w, r, http.StatusBadRequest, "invalid_body", "could not read the submitted form")
			return false
		}
		out.URL = r.PostFormValue("url")
		out.Label = r.PostFormValue("label")
		return true
	}

	if err := decodeJSON(r, out); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body",
			"body must be a json object with a 'url' field")
		return false
	}
	return true
}
