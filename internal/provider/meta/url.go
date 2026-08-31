package meta

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/foodibd/socialstats/internal/domain"
)

// Network distinguishes the two properties behind the "meta" platform. They
// share a company and a Graph API, but nothing about how their public pages are
// read is the same, so every code path branches on this.
type Network string

const (
	NetworkInstagram Network = "instagram"
	NetworkFacebook  Network = "facebook"
)

// Kind is the post type named in the URL path. Instagram serves all three from
// the same endpoint; it is kept because it decides the canonical URL we echo
// back, and because /p/ can be a photo carousel with no view count at all.
type Kind string

const (
	KindPost  Kind = "p"    // instagram feed post (photo, carousel or video)
	KindReel  Kind = "reel" // instagram reel, facebook reel
	KindTV    Kind = "tv"   // instagram igtv (legacy, still resolvable)
	KindVideo Kind = "video"
)

var (
	// Instagram shortcodes are base64url-ish and currently 11 characters, but
	// older posts are shorter and the length has changed before.
	shortcodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{5,24}$`)

	// Facebook numeric object ids.
	fbIDRe = regexp.MustCompile(`^[0-9]{5,25}$`)
)

var instagramHosts = []string{"instagram.com", "instagr.am", "cdninstagram.com"}

var facebookHosts = []string{"facebook.com", "fb.com", "fb.watch", "facebookwkhpilnemxj7asaniu7vnjjbiltxjqhye3mhbshg7kx5tfyd.onion"}

// shortHosts and short path prefixes hide the id behind a redirect, so the id
// is only known after the network fetch.
var shortHosts = map[string]bool{
	"fb.watch": true,
}

// Ref is a parsed Meta URL.
type Ref struct {
	Network Network
	Kind    Kind

	// Shortcode is the Instagram post shortcode ("Cx1y2z3AbCd").
	Shortcode string

	// ID is the Facebook numeric object id.
	ID string

	// Username is the handle when the URL carried one, without the "@".
	Username string

	// Short reports that the URL is a redirect stub: neither Shortcode nor ID
	// is populated and the target must be resolved over the network.
	Short bool
}

// Key returns the stable identifier for the ref, whichever network it is on.
func (r Ref) Key() string {
	if r.Shortcode != "" {
		return r.Shortcode
	}
	return r.ID
}

// ParseURL classifies a Facebook or Instagram URL. It performs no network I/O:
// short links come back with Short set and no id.
//
// Recognised forms:
//
//	https://www.instagram.com/p/Cx1y2z3AbCd/
//	https://www.instagram.com/reel/Cx1y2z3AbCd/
//	https://www.instagram.com/reels/Cx1y2z3AbCd/
//	https://www.instagram.com/tv/Cx1y2z3AbCd/
//	https://www.instagram.com/user/reel/Cx1y2z3AbCd/
//	https://www.instagram.com/share/reel/AbCdEf/          (short, needs resolving)
//	https://www.facebook.com/watch/?v=1234567890
//	https://www.facebook.com/somepage/videos/a-slug/1234567890/
//	https://www.facebook.com/reel/1234567890
//	https://www.facebook.com/video.php?v=1234567890
//	https://fb.watch/AbCdEfGh/                            (short, needs resolving)
//	https://www.facebook.com/share/v/AbCdEfGh/            (short, needs resolving)
func ParseURL(rawURL string) (Ref, error) {
	u, err := parse(rawURL)
	if err != nil {
		return Ref{}, err
	}

	host := normalizeHost(u.Host)
	switch {
	case matchesHost(host, instagramHosts):
		return parseInstagram(u, host)
	case matchesHost(host, facebookHosts):
		return parseFacebook(u, host)
	default:
		return Ref{}, fmt.Errorf("%w: host %q is not a meta property", domain.ErrInvalidURL, u.Host)
	}
}

func parseInstagram(u *url.URL, _ string) (Ref, error) {
	segments := splitPath(u.Path)
	if len(segments) == 0 {
		return Ref{}, fmt.Errorf("%w: instagram url has no post path", domain.ErrInvalidURL)
	}

	// /share/... and /share/reel/... are redirect stubs.
	if segments[0] == "share" {
		return Ref{Network: NetworkInstagram, Short: true}, nil
	}

	// A leading handle is optional: /p/<code> and /<user>/p/<code> both exist.
	username := ""
	if len(segments) >= 3 && kindOf(segments[0]) == "" && kindOf(segments[1]) != "" {
		username = strings.TrimPrefix(segments[0], "@")
		segments = segments[1:]
	}

	kind := kindOf(segments[0])
	if kind == "" {
		return Ref{}, fmt.Errorf("%w: %q is an instagram profile or unsupported path, not a post", domain.ErrInvalidURL, u.Path)
	}
	if len(segments) < 2 {
		return Ref{}, fmt.Errorf("%w: instagram %s url carries no shortcode", domain.ErrInvalidURL, kind)
	}

	code := segments[1]
	if !shortcodeRe.MatchString(code) {
		return Ref{}, fmt.Errorf("%w: %q is not an instagram shortcode", domain.ErrInvalidURL, code)
	}
	return Ref{Network: NetworkInstagram, Kind: kind, Shortcode: code, Username: username}, nil
}

// kindOf maps an Instagram path segment to a post kind, or "" when the segment
// is not a post prefix at all.
func kindOf(segment string) Kind {
	switch segment {
	case "p":
		return KindPost
	case "reel", "reels":
		return KindReel
	case "tv":
		return KindTV
	default:
		return ""
	}
}

func parseFacebook(u *url.URL, host string) (Ref, error) {
	segments := splitPath(u.Path)

	if shortHosts[host] {
		if len(segments) == 0 {
			return Ref{}, fmt.Errorf("%w: fb.watch link has no token", domain.ErrInvalidURL)
		}
		return Ref{Network: NetworkFacebook, Short: true}, nil
	}

	// /share/v/<token>, /share/r/<token> and bare /share/<token> are stubs.
	if len(segments) >= 2 && segments[0] == "share" {
		return Ref{Network: NetworkFacebook, Short: true}, nil
	}

	// ?v=, ?story_fbid= and ?video_id= carry the id directly, whatever the path.
	q := u.Query()
	for _, key := range []string{"v", "video_id", "story_fbid"} {
		if v := q.Get(key); v != "" {
			id, err := validateFBID(v)
			if err != nil {
				return Ref{}, err
			}
			return Ref{Network: NetworkFacebook, Kind: KindVideo, ID: id, Username: q.Get("id")}, nil
		}
	}

	// /reel/<id>
	if len(segments) >= 2 && segments[0] == "reel" {
		id, err := validateFBID(segments[1])
		if err != nil {
			return Ref{}, err
		}
		return Ref{Network: NetworkFacebook, Kind: KindReel, ID: id}, nil
	}

	// /<page>/videos/<id> and /<page>/videos/<slug>/<id>. The id is the last
	// numeric segment, because the slug between them is free text.
	for i, seg := range segments {
		if seg != "videos" && seg != "video" {
			continue
		}
		username := ""
		if i > 0 {
			username = segments[i-1]
		}
		if id := lastNumeric(segments[i+1:]); id != "" {
			return Ref{Network: NetworkFacebook, Kind: KindVideo, ID: id, Username: username}, nil
		}
	}

	// /watch/<id> (rare; /watch/?v= is the common form, handled above).
	if len(segments) >= 2 && segments[0] == "watch" {
		if id := lastNumeric(segments[1:]); id != "" {
			return Ref{Network: NetworkFacebook, Kind: KindVideo, ID: id}, nil
		}
	}

	return Ref{}, fmt.Errorf("%w: no facebook video id in %q", domain.ErrInvalidURL, u.String())
}

// ExtractID returns the post's identifier, and fails on short links since
// resolving those requires I/O.
func ExtractID(rawURL string) (string, error) {
	ref, err := ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	if ref.Key() == "" {
		return "", fmt.Errorf("%w: %q is a short link and must be resolved first", domain.ErrInvalidURL, rawURL)
	}
	return ref.Key(), nil
}

// IsMetaURL reports whether rawURL points at a Facebook or Instagram host.
func IsMetaURL(rawURL string) bool {
	u, err := parse(rawURL)
	if err != nil {
		return false
	}
	host := normalizeHost(u.Host)
	return matchesHost(host, instagramHosts) || matchesHost(host, facebookHosts)
}

// CanonicalURL renders the stable public URL for a ref.
func CanonicalURL(ref Ref) string {
	switch ref.Network {
	case NetworkInstagram:
		kind := ref.Kind
		if kind == "" {
			kind = KindPost
		}
		return "https://www.instagram.com/" + string(kind) + "/" + ref.Shortcode + "/"
	case NetworkFacebook:
		if ref.Kind == KindReel {
			return "https://www.facebook.com/reel/" + ref.ID
		}
		return "https://www.facebook.com/watch/?v=" + ref.ID
	default:
		return ""
	}
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
	if u.Host == "" {
		return nil, fmt.Errorf("%w: url has no host", domain.ErrInvalidURL)
	}
	return u, nil
}

func normalizeHost(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "www.")
	// Facebook serves the same content from regional and mobile subdomains
	// (m., web., d., free., mbasic.); they are all the same site to us.
	for _, prefix := range []string{"m.", "web.", "mbasic.", "touch.", "free.", "d."} {
		host = strings.TrimPrefix(host, prefix)
	}
	return host
}

func matchesHost(host string, suffixes []string) bool {
	for _, suffix := range suffixes {
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

// lastNumeric returns the last all-digit segment, which is where Facebook puts
// the object id in slugged video paths.
func lastNumeric(segments []string) string {
	for i := len(segments) - 1; i >= 0; i-- {
		if fbIDRe.MatchString(segments[i]) {
			return segments[i]
		}
	}
	return ""
}

func validateFBID(id string) (string, error) {
	if !fbIDRe.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not a facebook object id", domain.ErrInvalidURL, id)
	}
	return id, nil
}
