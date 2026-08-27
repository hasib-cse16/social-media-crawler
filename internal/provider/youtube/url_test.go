package youtube

import (
	"errors"
	"testing"

	"github.com/foodibd/socialstats/internal/domain"
)

func TestExtractVideoID(t *testing.T) {
	const want = "dQw4w9WgXcQ"

	valid := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtube.com/watch?v=dQw4w9WgXcQ&t=42s",
		"http://m.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?si=abc",
		"youtu.be/dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ",
		"https://www.youtube.com/live/dQw4w9WgXcQ",
		"https://www.youtube.com/v/dQw4w9WgXcQ",
		"https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
		"  https://www.youtube.com/watch?v=dQw4w9WgXcQ  ",
	}
	for _, in := range valid {
		got, err := ExtractVideoID(in)
		if err != nil {
			t.Errorf("ExtractVideoID(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ExtractVideoID(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"not a url",
		"ftp://youtube.com/watch?v=dQw4w9WgXcQ",
		"https://vimeo.com/12345",
		"https://www.youtube.com/watch?v=tooshort",
		"https://www.youtube.com/watch?v=waaaaaaaaaytoolong",
		"https://www.youtube.com/results?search_query=cats",
		"https://www.youtube.com/",
	}
	for _, in := range invalid {
		if _, err := ExtractVideoID(in); err == nil {
			t.Errorf("ExtractVideoID(%q) = nil error, want failure", in)
		} else if !errors.Is(err, domain.ErrInvalidURL) {
			t.Errorf("ExtractVideoID(%q) error = %v, want ErrInvalidURL", in, err)
		}
	}
}

func TestIsYouTubeURL(t *testing.T) {
	cases := map[string]bool{
		"https://www.youtube.com/results?search_query=x": true,
		"https://youtu.be/abc":                           true,
		"https://www.tiktok.com/@a/video/1":              false,
		"garbage":                                        false,
	}
	for in, want := range cases {
		if got := IsYouTubeURL(in); got != want {
			t.Errorf("IsYouTubeURL(%q) = %v, want %v", in, got, want)
		}
	}
}
