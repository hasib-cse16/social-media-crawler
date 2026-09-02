package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
)

// LookupHandler serves the JSON side of the dashboard: look a URL up, and read
// back what this account has looked up before.
type LookupHandler struct {
	svc LookupService
}

// LookupService is satisfied by *lookup.Service.
type LookupService interface {
	Lookup(ctx context.Context, userID int64, rawURL string) (*domain.Lookup, error)
	Get(ctx context.Context, userID int64, publicID string) (*domain.Lookup, error)
	History(ctx context.Context, userID int64) ([]domain.Lookup, error)
	Remove(ctx context.Context, userID int64, publicID string) error
}

func NewLookupHandler(svc LookupService) *LookupHandler {
	return &LookupHandler{svc: svc}
}

type lookupRequest struct {
	URL string `json:"url"`
}

// List handles GET /v1/lookups.
func (h *LookupHandler) List(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	history, err := h.svc.History(r.Context(), user.ID)
	if err != nil {
		RespondError(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, map[string]any{
		"lookups": history,
		"count":   len(history),
	})
}

// Create handles POST /v1/lookups.
func (h *LookupHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	var req lookupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		RespondError(w, r, httpx.Detail(domain.ErrInvalidURL, "the request body could not be read"))
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		RespondError(w, r, httpx.Detail(domain.ErrInvalidURL, "url is required"))
		return
	}

	record, err := h.svc.Lookup(r.Context(), user.ID, req.URL)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, record)
}

// Get handles GET /v1/lookups/{id}.
func (h *LookupHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	record, err := h.svc.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, record)
}

// Remove handles DELETE /v1/lookups/{id}.
func (h *LookupHandler) Remove(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		RespondError(w, r, domain.ErrUnauthenticated)
		return
	}

	if err := h.svc.Remove(r.Context(), user.ID, r.PathValue("id")); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
