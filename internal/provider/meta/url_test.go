package meta

import (
	"errors"
	"testing"

	"github.com/foodibd/socialstats/internal/domain"
)

func TestParseURLInstagram(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want Ref
	}{
		{"post", "https://www.instagram.com/p/Cx1y2z3AbCd/", Ref{Network: NetworkInstagram, Kind: KindPost, Shortcode: "Cx1y2z3AbCd"}},
		{"reel", "https://www.instagram.com/reel/Cx1y2z3AbCd/", Ref{Network: NetworkInstagram, Kind: KindReel, Shortcode: "Cx1y2z3AbCd"}},
		{"reels plural", "https://www.instagram.com/reels/Cx1y2z3AbCd/", Ref{Network: NetworkInstagram, Kind: KindReel, Shortcode: "Cx1y2z3AbCd"}},
		{"igtv", "https://www.instagram.com/tv/Cx1y2z3AbCd/", Ref{Network: NetworkInstagram, Kind: KindTV, Shortcode: "Cx1y2z3AbCd"}},
		{"handle prefix", "https://www.instagram.com/natgeo/reel/Cx1y2z3AbCd/", Ref{Network: NetworkInstagram, Kind: KindReel, Shortcode: "Cx1y2z3AbCd", Username: "natgeo"}},
		{"no scheme", "instagram.com/p/Cx1y2z3AbCd", Ref{Network: NetworkInstagram, Kind: KindPost, Shortcode: "Cx1y2z3AbCd"}},
		{"query junk", "https://www.instagram.com/reel/Cx1y2z3AbCd/?igsh=abc123&utm_source=ig_web", Ref{Network: NetworkInstagram, Kind: KindReel, Shortcode: "Cx1y2z3AbCd"}},
		{"share stub", "https://www.instagram.com/share/reel/AbCdEf/", Ref{Network: NetworkInstagram, Short: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseURL(tc.url)
			if err != nil {
				t.Fatalf("ParseURL(%q) returned error: %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("ParseURL(%q) = %+v, want %+v", tc.url, got, tc.want)
			}
		})
	}
}

func TestParseURLFacebook(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want Ref
	}{
		{"watch query", "https://www.facebook.com/watch/?v=1234567890", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890"}},
		{"watch no slash", "https://www.facebook.com/watch?v=1234567890", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890"}},
		{"video.php", "https://www.facebook.com/video.php?v=1234567890", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890"}},
		{"reel", "https://www.facebook.com/reel/1234567890", Ref{Network: NetworkFacebook, Kind: KindReel, ID: "1234567890"}},
		{"page videos", "https://www.facebook.com/natgeo/videos/1234567890/", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890", Username: "natgeo"}},
		{"page videos with slug", "https://www.facebook.com/natgeo/videos/a-really-long-slug-here/1234567890/", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890", Username: "natgeo"}},
		{"mobile host", "https://m.facebook.com/watch/?v=1234567890", Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "1234567890"}},
		{"fb.watch stub", "https://fb.watch/AbCdEfGh/", Ref{Network: NetworkFacebook, Short: true}},
		{"share stub", "https://www.facebook.com/share/v/AbCdEfGh/", Ref{Network: NetworkFacebook, Short: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseURL(tc.url)
			if err != nil {
				t.Fatalf("ParseURL(%q) returned error: %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("ParseURL(%q) = %+v, want %+v", tc.url, got, tc.want)
			}
		})
	}
}

func TestParseURLRejects(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"other platform", "https://www.tiktok.com/@user/video/7249376077976472833"},
		{"youtube", "https://youtu.be/dQw4w9WgXcQ"},
		{"instagram profile", "https://www.instagram.com/natgeo/"},
		{"instagram root", "https://www.instagram.com/"},
		{"facebook profile", "https://www.facebook.com/natgeo"},
		{"facebook non-numeric v", "https://www.facebook.com/watch/?v=not-an-id"},
		{"ftp scheme", "ftp://instagram.com/p/Cx1y2z3AbCd/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseURL(tc.url); !errors.Is(err, domain.ErrInvalidURL) {
				t.Errorf("ParseURL(%q) error = %v, want ErrInvalidURL", tc.url, err)
			}
		})
	}
}

func TestIsMetaURL(t *testing.T) {
	yes := []string{
		"https://www.instagram.com/p/Cx1y2z3AbCd/",
		"https://www.instagram.com/natgeo/", // a profile is still a meta url
		"https://fb.watch/AbCdEfGh/",
		"https://web.facebook.com/watch/?v=1",
		"instagr.am/p/Cx1y2z3AbCd/",
	}
	for _, u := range yes {
		if !IsMetaURL(u) {
			t.Errorf("IsMetaURL(%q) = false, want true", u)
		}
	}

	no := []string{"", "https://youtube.com/watch?v=x", "https://tiktok.com/@u/video/1", "notaurl", "https://fakefacebook.com/watch/?v=1"}
	for _, u := range no {
		if IsMetaURL(u) {
			t.Errorf("IsMetaURL(%q) = true, want false", u)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	tests := []struct {
		ref  Ref
		want string
	}{
		{Ref{Network: NetworkInstagram, Kind: KindReel, Shortcode: "Abc"}, "https://www.instagram.com/reel/Abc/"},
		{Ref{Network: NetworkInstagram, Shortcode: "Abc"}, "https://www.instagram.com/p/Abc/"},
		{Ref{Network: NetworkFacebook, Kind: KindReel, ID: "12"}, "https://www.facebook.com/reel/12"},
		{Ref{Network: NetworkFacebook, Kind: KindVideo, ID: "12"}, "https://www.facebook.com/watch/?v=12"},
	}
	for _, tc := range tests {
		if got := CanonicalURL(tc.ref); got != tc.want {
			t.Errorf("CanonicalURL(%+v) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestExtractIDRejectsShortLinks(t *testing.T) {
	if _, err := ExtractID("https://fb.watch/AbCdEfGh/"); !errors.Is(err, domain.ErrInvalidURL) {
		t.Errorf("ExtractID on a short link should report ErrInvalidURL, got %v", err)
	}
	id, err := ExtractID("https://www.instagram.com/reel/Cx1y2z3AbCd/")
	if err != nil || id != "Cx1y2z3AbCd" {
		t.Errorf("ExtractID = (%q, %v), want (Cx1y2z3AbCd, nil)", id, err)
	}
}
