package stats

import (
	"sync"
	"time"
)

// Cache is a small TTL cache in front of the providers. Upstream quotas are the
// scarce resource here, so repeated lookups of the same URL are served locally.
// Swap this for Redis behind the same two methods when you run more than one pod.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
	ttl     time.Duration
	now     func() time.Time
}

type entry struct {
	value     any
	expiresAt time.Time
}

func NewCache(ttl time.Duration) *Cache {
	return &Cache{
		entries: make(map[string]entry),
		ttl:     ttl,
		now:     time.Now,
	}
}

func (c *Cache) Get(key string) (any, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || c.now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

func (c *Cache) Set(key string, value any) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[key] = entry{value: value, expiresAt: c.now().Add(c.ttl)}
	c.mu.Unlock()
}

// Reap drops expired entries; call it periodically so the map cannot grow
// without bound on a long-running process.
func (c *Cache) Reap() int {
	if c == nil {
		return 0
	}
	now := c.now()
	removed := 0
	c.mu.Lock()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
			removed++
		}
	}
	c.mu.Unlock()
	return removed
}
