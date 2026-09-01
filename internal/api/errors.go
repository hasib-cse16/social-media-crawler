package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/foodibd/socialstats/internal/domain"
)

// httpErrorFor maps a domain error onto a status code and stable error code.
// This is the only place that knows the mapping, so providers never import net/http
// for the sake of status codes.
func httpErrorFor(err error) (status int, code string, message string) {
	switch {
	case errors.Is(err, domain.ErrInvalidURL):
		// The wrapped detail is echoed rather than replaced with fixed text.
		// Every message under this sentinel is about the input the caller just
		// sent — which query parameter was wrong, which URL form was not
		// recognised — so withholding it only makes the caller guess. It
		// carries nothing internal.
		return http.StatusBadRequest, "invalid_url", err.Error()
	case errors.Is(err, domain.ErrUnsupported):
		return http.StatusBadRequest, "unsupported_platform", "no provider handles that url"
	case errors.Is(err, domain.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated", "sign in to use this endpoint"
	case errors.Is(err, domain.ErrInvalidCredentials):
		// One message for "no such account" and for "wrong password". Any
		// difference here turns the login endpoint into a way to find out which
		// addresses have accounts.
		return http.StatusUnauthorized, "invalid_credentials", "invalid email or password"
	case errors.Is(err, domain.ErrTooManyAttempts):
		return http.StatusTooManyRequests, "too_many_attempts", "too many attempts, wait before trying again"
	case errors.Is(err, domain.ErrWeakPassword):
		// The message from the sentinel is echoed, because a password rule the
		// user cannot see is one they cannot satisfy.
		return http.StatusBadRequest, "weak_password", err.Error()
	case errors.Is(err, domain.ErrRegistrationClosed):
		return http.StatusForbidden, "registration_closed", "registration is closed on this deployment"
	case errors.Is(err, domain.ErrCSRF):
		return http.StatusForbidden, "csrf_failed", "the csrf token was missing or did not match"
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, "already_exists", "that email address already has an account"
	case errors.Is(err, domain.ErrLimitReached):
		return http.StatusUnprocessableEntity, "limit_reached", err.Error()
	case errors.Is(err, domain.ErrGone):
		return http.StatusGone, "gone", err.Error()
	case errors.Is(err, domain.ErrNeedsResolution):
		// Only reachable if a short link's redirect could not be followed; the
		// caller's remedy is to paste the resolved URL.
		return http.StatusBadGateway, "unresolved_short_link",
			"that short link could not be resolved; try the full url"
	case errors.Is(err, domain.ErrRecordNotFound):
		return http.StatusNotFound, "not_found", "no such record"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not_found", "the video does not exist, is private, or was removed"
	case errors.Is(err, domain.ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited", "upstream quota exhausted, retry later"
	case errors.Is(err, domain.ErrNotImplemented):
		return http.StatusNotImplemented, "not_implemented", "this platform is not supported yet"
	case errors.Is(err, domain.ErrMisconfigured):
		return http.StatusServiceUnavailable, "provider_unavailable", "the provider for that url is not configured on this deployment"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream_timeout", "the upstream platform did not respond in time"
	case errors.Is(err, context.Canceled):
		return 499, "client_closed_request", "the client closed the request"
	case errors.Is(err, domain.ErrBlocked):
		return http.StatusBadGateway, "upstream_blocked", "the platform served an anti-bot challenge instead of content; retry later"
	case errors.Is(err, domain.ErrUpstreamFailure):
		return http.StatusBadGateway, "upstream_error", "the upstream platform returned an error"
	default:
		return http.StatusInternalServerError, "internal_error", "unexpected server error"
	}
}
