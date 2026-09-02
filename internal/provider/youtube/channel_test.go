package youtube

import "testing"

// The email is read out of prose that is mostly links, hashtags and emoji, so
// the cases that matter are the ones where something merely looks like an
// address.
func TestExtractEmail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Business enquiries: hello@example.com", "hello@example.com"},
		{"trailing punctuation", "Contact me at hello@example.com.", "hello@example.com"},
		{"first of several wins", "hello@example.com or backup@example.com", "hello@example.com"},
		{"obfuscated", "business (at) example (dot) com", "business@example.com"},
		{"obfuscated with brackets", "hello [at] example [dot] com", "hello@example.com"},
		{"plain beats obfuscated", "real@example.com — or fake (at) other (dot) com", "real@example.com"},
		{"none", "Subscribe for more! #shorts", ""},
		{"empty", "", ""},
		{"a URL is not an address", "Visit https://example.com/about for more", ""},
		{"an image filename is not an address", "logo@2x.png", ""},
		{"a social handle is not an address", "Follow @creator on everything", ""},

		// Found live against TED's channel: the bare words "at" and "dot" were
		// matching inside an ordinary one, turning "recommendations.TED" into
		// "recommend@ions.TED".
		{"at inside a word is not a separator", "Watch our recommendations.TED talks daily", ""},
		{"dot inside a word is not a separator", "Read the dotfiles at github", ""},
		{"plain domain after an obfuscated at", "hello [at] example.com", "hello@example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractEmail(tc.in); got != tc.want {
				t.Errorf("ExtractEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChannelURLPrefersTheHandle(t *testing.T) {
	if got := channelURL("UC123", "@creator"); got != "https://www.youtube.com/@creator" {
		t.Errorf("with a handle = %q", got)
	}
	if got := channelURL("UC123", ""); got != "https://www.youtube.com/channel/UC123" {
		t.Errorf("without a handle = %q", got)
	}
	if got := channelURL("", ""); got != "" {
		t.Errorf("with nothing = %q, want empty", got)
	}
}
