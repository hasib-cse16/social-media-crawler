package tiktok

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/foodibd/socialstats/internal/domain"
)

// A TikTok item id is a numeric snowflake, currently 19 digits.
var videoIDRe = regexp.MustCompile(`^[0-9]{15,25}$`)

var hostSuffixes = []string{"tiktok.com"}

// shortHosts serve redirect-only links whose id is not in the URL, so they must
// be resolved over the network before the id is known.
var shortHosts = map[string]bool{
	"vm.tiktok.com": true,
	"vt.tiktok.com": true,
}

// Ref is a parsed TikTok URL.
type Ref struct {
	// VideoID is empty when the URL is a short link that still needs resolving.
	VideoID string
	// Username is the @handle when the URL carried one, without the "@".
	Username string
	// Short reports whether this URL must be resolved by following redirects.
	Short bool
}

// ParseURL classifies a TikTok URL. It performs no network I/O: short links
// come back with Short set and VideoID empty.
//
// Recognised forms:
//
//	https://www.tiktok.com/@user/video/7249376077976472833
//	https://www.tiktok.com/@user/photo/7249376077976472833   (photo posts)
//	https://m.tiktok.com/v/7249376077976472833.html
//	https://www.tiktok.com/t/ZTRfabcdef/                     (short, needs resolving)
//	https://vm.tiktok.com/ZTRfabcdef/                        (short, needs resolving)
func ParseURL(rawURL string) (Ref, error) {
	u, err := parse(rawURL)
	if err != nil {
		return Ref{}, err
	}

	host := normalizeHost(u.Host)
	if !isTikTokHost(host) {
		return Ref{}, fmt.Errorf("%w: host %q is not tiktok", domain.ErrInvalidURL, u.Host)
	}

	segments := splitPath(u.Path)

	if shortHosts[host] {
		if len(segments) == 0 {
			return Ref{}, fmt.Errorf("%w: short link has no token", domain.ErrInvalidURL)
		}
		return Ref{Short: true}, nil
	}

	// /t/<token> short links live on the main host.
	if len(segments) >= 2 && segments[0] == "t" {
		return Ref{Short: true}, nil
	}

	// /@user/video/<id> and /@user/photo/<id>
	if len(segments) >= 3 && strings.HasPrefix(segments[0], "@") {
		switch segments[1] {
		case "video", "photo":
			id, err := validate(segments[2])
			if err != nil {
				return Ref{}, err
			}
			return Ref{VideoID: id, Username: strings.TrimPrefix(segments[0], "@")}, nil
		}
	}

	// /v/<id>.html, as served to some mobile clients.
	if len(segments) >= 2 && segments[0] == "v" {
		id, err := validate(strings.TrimSuffix(segments[1], ".html"))
		if err != nil {
			return Ref{}, err
		}
		return Ref{VideoID: id}, nil
	}

	// ?item_id= / ?video_id= query forms used by some share links.
	for _, key := range []string{"item_id", "video_id", "aweme_id"} {
		if v := u.Query().Get(key); v != "" {
			id, err := validate(v)
			if err != nil {
				return Ref{}, err
			}
			return Ref{VideoID: id}, nil
		}
	}

	return Ref{}, fmt.Errorf("%w: no video id in %q", domain.ErrInvalidURL, rawURL)
}

// ExtractVideoID returns just the id, and fails on short links since resolving
// them requires I/O.
func ExtractVideoID(rawURL string) (string, error) {
	ref, err := ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	if ref.VideoID == "" {
		return "", fmt.Errorf("%w: %q is a short link and must be resolved first", domain.ErrInvalidURL, rawURL)
	}
	return ref.VideoID, nil
}

// IsTikTokURL reports whether rawURL points at a TikTok host.
func IsTikTokURL(rawURL string) bool {
	u, err := parse(rawURL)
	if err != nil {
		return false
	}
	return isTikTokHost(normalizeHost(u.Host))
}

// CanonicalURL renders the canonical watch URL. TikTok tolerates any handle in
// the path, but the real one is used when known.
func CanonicalURL(username, id string) string {
	if username == "" {
		username = "_"
	}
	return "https://www.tiktok.com/@" + username + "/video/" + id
}

func parse(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: empty url", domain.ErrInvalidURL)
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrInvalidURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", domain.ErrInvalidURL, u.Scheme)
	}
	return u, nil
}

func normalizeHost(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimPrefix(host, "www.")
}

func isTikTokHost(host string) bool {
	for _, suffix := range hostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func validate(id string) (string, error) {
	if !videoIDRe.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not a tiktok video id", domain.ErrInvalidURL, id)
	}
	return id, nil
}
