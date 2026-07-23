package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// timeNow is the relay's clock, overridable in tests.
var timeNow = time.Now

// maxCacheEntries bounds the public-read cache. The allowlisted paths have a
// tiny key space (path × chainId × priority), so this is only a runaway guard.
const maxCacheEntries = 1024

// cacheEntry is a stored upstream GET response plus its freshness window.
type cacheEntry struct {
	status      int
	body        []byte
	contentType string
	cacheCtl    string
	storedAt    time.Time
	expiresAt   time.Time
}

// ttlCache is a tiny, concurrency-safe response cache for the relay's public
// GET endpoints. It keeps a response only for as long as the paymaster's
// Cache-Control: max-age permits, so the relay never serves a quote staler than
// the paymaster itself allows. Shared across all clients — the responses are
// public and identity-free, so a shared cache is correct and reduces the
// request volume the paymaster sees.
type ttlCache struct {
	mu sync.RWMutex
	m  map[string]cacheEntry
}

func newTTLCache() *ttlCache { return &ttlCache{m: make(map[string]cacheEntry)} }

// get returns a still-fresh entry for key, or ok=false if absent/expired.
func (c *ttlCache) get(key string) (cacheEntry, bool) {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || !timeNow().Before(e.expiresAt) {
		return cacheEntry{}, false
	}
	return e, true
}

// put stores e under key. It clears the map on hitting the size cap (a simple
// bound; the key space is tiny in practice).
func (c *ttlCache) put(key string, e cacheEntry) {
	c.mu.Lock()
	if len(c.m) >= maxCacheEntries {
		c.m = make(map[string]cacheEntry)
	}
	c.m[key] = e
	c.mu.Unlock()
}

// cacheTTL parses Cache-Control and returns how long to cache the response.
// It returns ok=false when the response must not be cached — no-store / no-cache
// / private, or a missing / non-positive freshness lifetime — so the relay's
// caching mirrors exactly what the paymaster permits. The relay is a SHARED
// cache, so it honors s-maxage in preference to max-age when both are present
// (RFC 9111 §5.2.2.10); the paymaster currently sets them equal.
func cacheTTL(cacheControl string) (time.Duration, bool) {
	if cacheControl == "" {
		return 0, false
	}
	dirs := strings.Split(strings.ToLower(cacheControl), ",")
	for _, d := range dirs {
		switch strings.TrimSpace(d) {
		case "no-store", "no-cache", "private":
			return 0, false
		}
	}
	var maxAge, sMaxAge = -1, -1
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if v, ok := strings.CutPrefix(d, "s-maxage="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				sMaxAge = n
			}
			continue
		}
		if v, ok := strings.CutPrefix(d, "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				maxAge = n
			}
		}
	}
	// s-maxage wins for a shared cache; fall back to max-age.
	lifetime := maxAge
	if sMaxAge >= 0 {
		lifetime = sMaxAge
	}
	if lifetime > 0 {
		return time.Duration(lifetime) * time.Second, true
	}
	return 0, false
}
