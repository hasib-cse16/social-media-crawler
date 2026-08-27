package youtube

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/foodibd/socialstats/internal/domain"
)

var (
	// A YouTube video id is exactly 11 chars of the URL-safe base64 alphabet.
	videoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

	hostSuffixes = []string{
		"youtube.com",
		"youtube-nocookie.com",
		"youtu.be",
	}
)

// pathPrefixes maps a first path segment to the position of the id after it.
var pathPrefixes = []string{"embed", "v", "shorts", "live"}

// ExtractVideoID pulls the 11-character video id out of any common YouTube URL
// form: watch?v=, youtu.be/, /shorts/, /embed/, /live/, /v/.
func ExtractVideoID(rawURL string) (string, error) {
	u, err := parse(rawURL)
	if err != nil {
		return "", err
	}

	host := normalizeHost(u.Host)
	if !isYouTubeHost(host) {
		return "", fmt.Errorf("%w: host %q is not youtube", domain.ErrInvalidURL, u.Host)
	}

	segments := splitPath(u.Path)

	// youtu.be/<id>
	if host == "youtu.be" && len(segments) > 0 {
		return validate(segments[0])
	}

	// watch?v=<id> (also attribution_link style ?v=)
	if id := u.Query().Get("v"); id != "" {
		return validate(id)
	}

	if len(segments) >= 2 {
		for _, prefix := range pathPrefixes {
			if segments[0] == prefix {
				return validate(segments[1])
			}
		}
	}

	// /watch/<id> and bare /<id> fallbacks.
	if len(segments) == 1 {
		return validate(segments[0])
	}
	if len(segments) >= 2 && segments[0] == "watch" {
		return validate(segments[1])
	}

	return "", fmt.Errorf("%w: no video id in %q", domain.ErrInvalidURL, rawURL)
}

// IsYouTubeURL reports whether rawURL points at a YouTube host. It does not
// require that a video id be present.
func IsYouTubeURL(rawURL string) bool {
	u, err := parse(rawURL)
	if err != nil {
		return false
	}
	return isYouTubeHost(normalizeHost(u.Host))
}

func parse(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: empty url", domain.ErrInvalidURL)
	}
	// Accept scheme-less input like "youtu.be/abc".
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

func isYouTubeHost(host string) bool {
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
		return "", fmt.Errorf("%w: %q is not a video id", domain.ErrInvalidURL, id)
	}
	return id, nil
}

// CanonicalURL renders the canonical watch URL for a video id.
func CanonicalURL(id string) string {
	return "https://www.youtube.com/watch?v=" + id
}
