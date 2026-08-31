package tiktok

import (
	"errors"
	"testing"

	"github.com/foodibd/socialstats/internal/domain"
)

func TestParseURLLongForm(t *testing.T) {
	const wantID = "7249376077976472833"

	cases := []struct {
		in       string
		wantUser string
	}{
		{"https://www.tiktok.com/@thebillionairebros/video/7249376077976472833", "thebillionairebros"},
		{"https://tiktok.com/@user.name/video/7249376077976472833?is_from_webapp=1", "user.name"},
		{"https://www.tiktok.com/@user/photo/7249376077976472833", "user"},
		{"tiktok.com/@user/video/7249376077976472833", "user"},
		{"https://m.tiktok.com/v/7249376077976472833.html", ""},
		{"https://www.tiktok.com/share?item_id=7249376077976472833", ""},
	}
	for _, tc := range cases {
		ref, err := ParseURL(tc.in)
		if err != nil {
			t.Errorf("ParseURL(%q): %v", tc.in, err)
			continue
		}
		if ref.VideoID != wantID {
			t.Errorf("ParseURL(%q).VideoID = %q, want %q", tc.in, ref.VideoID, wantID)
		}
		if ref.Username != tc.wantUser {
			t.Errorf("ParseURL(%q).Username = %q, want %q", tc.in, ref.Username, tc.wantUser)
		}
		if ref.Short {
			t.Errorf("ParseURL(%q).Short = true, want false", tc.in)
		}
	}
}

func TestParseURLShortLinks(t *testing.T) {
	for _, in := range []string{
		"https://vm.tiktok.com/ZTRfabcdef/",
		"https://vt.tiktok.com/ZTRfabcdef/",
		"https://www.tiktok.com/t/ZTRfabcdef/",
	} {
		ref, err := ParseURL(in)
		if err != nil {
			t.Errorf("ParseURL(%q): %v", in, err)
			continue
		}
		if !ref.Short {
			t.Errorf("ParseURL(%q).Short = false, want true", in)
		}
		if ref.VideoID != "" {
			t.Errorf("ParseURL(%q).VideoID = %q, want empty until resolved", in, ref.VideoID)
		}
		// ExtractVideoID must refuse, since resolving needs network I/O.
		if _, err := ExtractVideoID(in); !errors.Is(err, domain.ErrInvalidURL) {
			t.Errorf("ExtractVideoID(%q) = %v, want ErrInvalidURL", in, err)
		}
	}
}

func TestParseURLRejectsJunk(t *testing.T) {
	for _, in := range []string{
		"",
		"not a url",
		"ftp://tiktok.com/@u/video/7249376077976472833",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.tiktok.com/@user/video/notanid",
		"https://www.tiktok.com/@user/video/123",
		"https://www.tiktok.com/",
		"https://www.tiktok.com/@user",
	} {
		if _, err := ParseURL(in); !errors.Is(err, domain.ErrInvalidURL) {
			t.Errorf("ParseURL(%q) = %v, want ErrInvalidURL", in, err)
		}
	}
}

func TestIsTikTokURL(t *testing.T) {
	cases := map[string]bool{
		"https://www.tiktok.com/@u/video/7249376077976472833": true,
		"https://vm.tiktok.com/ZTRfabcdef/":                   true,
		"https://www.tiktok.com/foryou":                       true,
		"https://youtu.be/dQw4w9WgXcQ":                        false,
		"nonsense":                                            false,
	}
	for in, want := range cases {
		if got := IsTikTokURL(in); got != want {
			t.Errorf("IsTikTokURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	if got := CanonicalURL("user", "7249376077976472833"); got != "https://www.tiktok.com/@user/video/7249376077976472833" {
		t.Errorf("CanonicalURL = %q", got)
	}
	// An unknown handle still yields a URL TikTok will resolve.
	if got := CanonicalURL("", "7249376077976472833"); got != "https://www.tiktok.com/@_/video/7249376077976472833" {
		t.Errorf("CanonicalURL with no user = %q", got)
	}
}
