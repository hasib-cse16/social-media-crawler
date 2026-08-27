// Package httpclient builds the shared outbound HTTP client used by providers.
package httpclient

import (
	"net"
	"net/http"
	"time"
)

// New returns a client with sane connection pooling and an overall timeout.
// One client is shared by API-based providers so connections are reused.
func New(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// NewWithoutConnectionReuse returns a client that opens a fresh connection for
// every request.
//
// This exists for providers scraping a site that decides per-connection whether
// to serve real content or an anti-bot challenge. When retries reuse a
// connection that has already been given a challenge, they get the same
// challenge again and the retry is wasted: measured against TikTok, retrying on
// a pooled connection succeeded 4 times in 6, while retrying on fresh
// connections succeeded 6 times in 6.
//
// The cost is a TLS handshake per request, which is acceptable for a provider
// that is already fetching a ~400 KB HTML page.
func NewWithoutConnectionReuse(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}
