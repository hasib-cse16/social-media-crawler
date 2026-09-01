package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors that the transport layer maps to HTTP status codes.
var (
	ErrInvalidURL  = errors.New("invalid or unsupported url")
	ErrUnsupported = errors.New("platform not supported")
	ErrNotFound    = errors.New("video not found")

	// ErrNeedsResolution means a URL is a short link whose target id cannot be
	// known without following the redirect. It is a fact about the URL, not a
	// failure: the caller's next move is to fetch rather than to give up.
	ErrNeedsResolution = errors.New("url must be resolved over the network before it can be identified")
	ErrRateLimited     = errors.New("upstream rate limited")
	ErrUpstreamFailure = errors.New("upstream request failed")
	ErrNotImplemented  = errors.New("provider not implemented yet")
	ErrMisconfigured   = errors.New("provider is not configured")

	// Authentication and authorisation.
	//
	// ErrInvalidCredentials is returned for both "no such account" and "wrong
	// password", deliberately and identically, so the endpoint cannot be used
	// to find out which addresses have accounts.
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUnauthenticated    = errors.New("not signed in")
	ErrTooManyAttempts    = errors.New("too many attempts")
	ErrWeakPassword       = errors.New("password does not meet the minimum requirements")
	ErrRegistrationClosed = errors.New("registration is closed on this deployment")
	ErrCSRF               = errors.New("csrf check failed")

	// ErrLimitReached means a per-account quota is exhausted — tracking is what
	// creates polling work, and polling work is spent against upstream budgets
	// everyone on the deployment shares.
	ErrLimitReached = errors.New("account limit reached")

	// ErrGone means the resource existed and no longer does, which is a
	// different fact from never having existed.
	ErrGone = errors.New("no longer available")

	// Storage-layer sentinels. They are deliberately generic: the persistence
	// layer does not know whether the row it could not find was a user, a
	// video or a session, and callers that need a specific message wrap these
	// rather than inventing their own.
	ErrRecordNotFound = errors.New("record not found")
	ErrConflict       = errors.New("record already exists")
	ErrStorage        = errors.New("storage failure")

	// ErrBlocked means the platform served an anti-bot challenge or block page
	// rather than content. It is distinct from ErrUpstreamFailure because the
	// remedy differs: back off, rotate egress, or revisit the fetch strategy.
	ErrBlocked = errors.New("blocked by upstream anti-bot protection")
)

// UpstreamError carries the provider's own status/body for logging and
// wraps a sentinel so callers can still use errors.Is.
type UpstreamError struct {
	Provider string
	Status   int
	Body     string
	sentinel error
}

func NewUpstreamError(provider string, status int, body string, sentinel error) *UpstreamError {
	return &UpstreamError{Provider: provider, Status: status, Body: body, sentinel: sentinel}
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s: upstream status %d: %s", e.Provider, e.Status, e.Body)
}

func (e *UpstreamError) Unwrap() error { return e.sentinel }
