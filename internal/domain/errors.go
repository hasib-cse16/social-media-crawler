package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors that the transport layer maps to HTTP status codes.
var (
	ErrInvalidURL      = errors.New("invalid or unsupported url")
	ErrUnsupported     = errors.New("platform not supported")
	ErrNotFound        = errors.New("video not found")
	ErrRateLimited     = errors.New("upstream rate limited")
	ErrUpstreamFailure = errors.New("upstream request failed")
	ErrNotImplemented  = errors.New("provider not implemented yet")
	ErrMisconfigured   = errors.New("provider is not configured")

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
