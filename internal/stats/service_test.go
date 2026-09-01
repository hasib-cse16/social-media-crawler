package stats

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

type fakeProvider struct {
	calls int
	err   error
}

func (f *fakeProvider) Platform() domain.Platform { return domain.PlatformYouTube }
func (f *fakeProvider) Match(string) bool         { return true }
func (f *fakeProvider) Identify(string) (domain.VideoRef, error) {
	return domain.VideoRef{Platform: domain.PlatformYouTube, VideoID: "abc"}, nil
}
func (f *fakeProvider) Stats(context.Context, string) (*domain.VideoStats, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &domain.VideoStats{Platform: domain.PlatformYouTube, VideoID: "abc", ViewCount: domain.U64(7)}, nil
}

type fakeResolver struct{ p domain.Provider }

func (f fakeResolver) For(string) (domain.Provider, error) { return f.p, nil }
func (f fakeResolver) Platforms() []domain.Platform        { return []domain.Platform{domain.PlatformYouTube} }

func TestByURLCachesRepeatLookups(t *testing.T) {
	p := &fakeProvider{}
	svc := NewService(fakeResolver{p: p}, NewCache(time.Minute), slog.New(slog.DiscardHandler))

	first, err := svc.ByURL(context.Background(), "https://youtu.be/abc")
	if err != nil {
		t.Fatalf("first ByURL: %v", err)
	}
	if first.Cached {
		t.Error("first call reported a cache hit")
	}

	second, err := svc.ByURL(context.Background(), "https://youtu.be/abc")
	if err != nil {
		t.Fatalf("second ByURL: %v", err)
	}
	if !second.Cached {
		t.Error("second call was not served from cache")
	}
	if p.calls != 1 {
		t.Errorf("provider called %d times, want 1", p.calls)
	}
}

func TestByURLRejectsEmpty(t *testing.T) {
	svc := NewService(fakeResolver{p: &fakeProvider{}}, NewCache(0), slog.New(slog.DiscardHandler))
	if _, err := svc.ByURL(context.Background(), "   "); !errors.Is(err, domain.ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestCacheExpiryAndReap(t *testing.T) {
	c := NewCache(time.Minute)
	c.Set("k", "v")
	if _, ok := c.Get("k"); !ok {
		t.Fatal("value not stored")
	}

	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := c.Get("k"); ok {
		t.Error("expired value was returned")
	}
	if n := c.Reap(); n != 1 {
		t.Errorf("Reap = %d, want 1", n)
	}
}

func TestZeroTTLCacheIsDisabled(t *testing.T) {
	c := NewCache(0)
	c.Set("k", "v")
	if _, ok := c.Get("k"); ok {
		t.Error("cache with zero ttl stored a value")
	}
}
