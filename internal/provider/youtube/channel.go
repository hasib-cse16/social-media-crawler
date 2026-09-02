package youtube

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/foodibd/socialstats/internal/domain"
)

// channelsListResponse mirrors only the fields we consume from channels.list.
type channelsListResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			CustomURL   string `json:"customUrl"`
		} `json:"snippet"`
	} `json:"items"`
}

// channel enriches stats with the uploader's channel details.
//
// The Data API has no field for a channel's business email — that address is
// something owners write into their own About text, which channels.list does
// return as snippet.description. So the email is read out of there rather than
// scraped from the About page, which sits behind a bot check.
//
// Failure here is deliberately not fatal: the caller already has the view count
// the user asked for, and losing the whole lookup because a second API call was
// rate-limited would be a poor trade.
func (p *Provider) channel(ctx context.Context, stats *domain.VideoStats) {
	if stats.ChannelID == "" {
		return
	}

	body, err := p.get(ctx, "/channels", url.Values{
		"part": {"snippet"},
		"id":   {stats.ChannelID},
		"key":  {p.apiKey},
	})
	if err != nil {
		p.log.WarnContext(ctx, "channel lookup failed", "channel_id", stats.ChannelID, "error", err)
		return
	}

	var payload channelsListResponse
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Items) == 0 {
		return
	}

	item := payload.Items[0]
	stats.ChannelDescription = item.Snippet.Description
	if item.Snippet.Title != "" {
		stats.ChannelTitle = item.Snippet.Title
	}
	stats.ChannelURL = channelURL(item.ID, item.Snippet.CustomURL)
	stats.ChannelEmail = ExtractEmail(item.Snippet.Description)
}

// channelURL prefers the @handle form, which is what a human recognises, and
// falls back to the /channel/UC... form, which always exists.
func channelURL(id, customURL string) string {
	if customURL != "" {
		return "https://www.youtube.com/" + strings.TrimPrefix(customURL, "/")
	}
	if id == "" {
		return ""
	}
	return "https://www.youtube.com/channel/" + id
}

// emailPattern is intentionally narrower than RFC 5322. Channel descriptions
// are prose full of URLs, hashtags and emoji, and a permissive pattern picks
// domains out of links far more often than it finds a real address.
var emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// obfuscated matches the forms owners use to dodge scrapers, e.g.
// "business (at) example (dot) com" or "hello [at] example.com".
//
// The separators around "at" and "dot" are required rather than optional, and
// that is the whole difficulty of this pattern: without them the bare words
// match inside ordinary text, and "recommendations.TED" reads as
// "recommend at ions dot TED". Punctuation or whitespace on both sides is what
// makes the word a separator rather than three letters in the middle of
// another one.
var obfuscated = regexp.MustCompile(
	`(?i)\b([a-z0-9._%+\-]+)[\s(\[]+at[\s)\]]+` + // local part, then a delimited "at"
		`(?:` +
		`([a-z0-9\-]+)[\s(\[]+dot[\s)\]]+([a-z]{2,})` + // example (dot) com
		`|` +
		`([a-z0-9\-]+\.[a-z]{2,})` + // example.com
		`)\b`)

// ExtractEmail pulls the first plausible contact address out of channel text.
//
// "First" rather than "all" because owners who list several put the one they
// want used first, and returning a list would push the choice onto the caller,
// who knows less than the owner did.
func ExtractEmail(text string) string {
	if text == "" {
		return ""
	}
	for _, m := range emailPattern.FindAllString(text, -1) {
		if candidate := strings.Trim(m, ".,;:"); plausible(candidate) {
			return candidate
		}
	}
	// Only if nothing was written plainly, since the obfuscated pattern is the
	// looser of the two and more willing to be wrong.
	if m := obfuscated.FindStringSubmatch(text); m != nil {
		domain := m[4] // the "example.com" branch
		if domain == "" {
			domain = m[2] + "." + m[3] // the "example (dot) com" branch
		}
		candidate := strings.ToLower(m[1] + "@" + domain)
		if plausible(candidate) {
			return candidate
		}
	}
	return ""
}

// imageSuffixes are the tails that give away a filename that merely looks like
// an address, which is what "logo@2x.png" is.
var imageSuffixes = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"}

func plausible(candidate string) bool {
	lower := strings.ToLower(candidate)
	for _, suffix := range imageSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	// A local part this long is a hash or a mangled URL, not something a person
	// typed for others to write to.
	at := strings.Index(lower, "@")
	return at > 0 && at <= 64 && len(lower)-at <= 255
}
