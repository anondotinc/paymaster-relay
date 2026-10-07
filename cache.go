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

// maxCacheAge caps a parsed Age so it cannot overflow a time.Duration.
const maxCacheAge = 365 * 24 * time.Hour

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
// / private (with or without a field list, e.g. private="set-cookie"), or a
// missing, malformed, repeated or non-positive freshness lifetime — so the
// relay's caching mirrors exactly what the paymaster permits and fails closed
// on anything ambiguous. The relay is a SHARED cache, so it honors s-maxage in
// preference to max-age (RFC 9111 §5.2.2.10); a malformed s-maxage never falls
// back to max-age. The paymaster currently sets them equal.
func cacheTTL(cacheControl string) (time.Duration, bool) {
	var maxAge, sMaxAge = -1, -1
	for _, directive := range strings.Split(strings.ToLower(cacheControl), ",") {
		var name, value, hasValue = strings.Cut(strings.TrimSpace(directive), "=")
		name = strings.TrimSpace(name)
		switch name {
		case "no-store", "no-cache", "private":
			return 0, false
		case "s-maxage", "max-age":
			var seconds, ok = deltaSeconds(value)
			var lifetime = &maxAge
			if name == "s-maxage" {
				lifetime = &sMaxAge
			}
			if !hasValue || !ok || *lifetime >= 0 {
				return 0, false
			}
			*lifetime = seconds
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

// deltaSeconds parses a delta-seconds value (RFC 9111 §1.2.2): plain digits
// only (no sign, quotes or spaces), at most maxCacheAge.
func deltaSeconds(value string) (int, bool) {
	if value == "" || strings.TrimLeft(value, "0123456789") != "" {
		return 0, false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds > int64(maxCacheAge/time.Second) {
		return 0, false
	}
	return int(seconds), true
}

// upstreamAge parses an upstream Age header (RFC 9111 §5.1). An absent header
// is age zero; a present but invalid one returns ok=false, and the response
// must then be treated as stale.
func upstreamAge(value string) (time.Duration, bool) {
	if value == "" {
		return 0, true
	}
	seconds, ok := deltaSeconds(value) // rejects "+5", "-1", "5, 6"
	if !ok {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
