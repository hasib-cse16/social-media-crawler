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
		return http.StatusBadRequest, "invalid_url", "the supplied url could not be parsed as a supported video url"
	case errors.Is(err, domain.ErrUnsupported):
		return http.StatusBadRequest, "unsupported_platform", "no provider handles that url"
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
	case errors.Is(err, domain.ErrUpstreamFailure):
		return http.StatusBadGateway, "upstream_error", "the upstream platform returned an error"
	default:
		return http.StatusInternalServerError, "internal_error", "unexpected server error"
	}
}
