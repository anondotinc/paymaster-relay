package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Public gas reads: the paymaster's gas tiers and gas history and the API's
// network base fee. All are identity-free and fresh-only (the API answers
// "public, s-maxage=5, max-age=5"), so the SDK reads them through the relay
// instead of calling the API directly. Unlike the verbatim publicReadPaths,
// the relay forwards ONLY a validated, canonical chainId on a fixed path: no
// caller query, path segment or header reaches the API or the cache key.
// Keep in sync with GAS_READ_CHAINS / gasReadTarget in worker/src/index.ts.
const (
	gasTiersPath = "/api/v1/paymaster/gas-tiers"
	// gasHistoryPath is the paymaster's recent gas history (the apps' live gas
	// chart): the same one-chainId read as gas-tiers.
	gasHistoryPath = "/api/v1/paymaster/gas-history"
	gasFeePrefix   = "/api/v1/tx/gas-fee/"
	// maxGasReadQuery bounds the raw query before parsing ("chainId=42161" is 13).
	maxGasReadQuery = 128
)

// gasReadChains are the chains the paymaster serves. Exact decimal strings, so
// "01", "+1" or "1.0" never become an upstream URL or a cache key.
var gasReadChains = map[string]bool{"1": true, "56": true, "137": true, "42161": true}

// canonicalGasTiersQuery accepts exactly one chain field — chainId, or its
// protobuf spelling chain_id — with an allowed value, and nothing else. It
// returns the canonical upstream query ("chainId=<id>").
func canonicalGasTiersQuery(raw string) (string, bool) {
	if raw == "" || len(raw) > maxGasReadQuery {
		return "", false
	}
	var values, err = url.ParseQuery(raw) // rejects bad escapes and ';'
	if err != nil {
		return "", false
	}
	if len(values) != 1 {
		return "", false
	}
	var chain string
	for key, items := range values {
		if (key != "chainId" && key != "chain_id") || len(items) != 1 {
			return "", false
		}
		chain = items[0]
	}
	if !gasReadChains[chain] {
		return "", false
	}
	return "chainId=" + chain, true
}

// gasFeeChain returns the allowed chain in GET /api/v1/tx/gas-fee/{chainId}.
// The route takes no query, and the segment must be a literal allowed chain
// (no percent-encoding).
func gasFeeChain(r *http.Request) (string, bool) {
	var chain = r.PathValue("chainId")
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.EscapedPath() != gasFeePrefix+chain || !gasReadChains[chain] {
		return "", false
	}
	return chain, true
}

// isGasReadPath reports whether a request path belongs to the gas reads, so
// the relay can hand it (404/405 included) to the gas-read mux.
func isGasReadPath(path string) bool {
	return path == gasTiersPath || path == gasHistoryPath ||
		path == strings.TrimSuffix(gasFeePrefix, "/") || strings.HasPrefix(path, gasFeePrefix) ||
		strings.HasPrefix(path, gasTiersPath+"/") || strings.HasPrefix(path, gasHistoryPath+"/")
}

// rejectGasRead answers an invalid gas read without contacting the API. The
// refusal is never cacheable.
func rejectGasRead(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "invalid gas read", http.StatusBadRequest)
}

// gasReadMethod answers preflights and refuses every method but GET (HEAD
// included) before validation, so only a GET can reach the cache or the API.
func gasReadMethod(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return true
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, OPTIONS")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	return false
}

// handleGasReads registers the gas reads on mux. Each forwards only the
// canonical target through reads.serve, which honours (never extends) the
// API's Cache-Control and never caches an error.
func handleGasReads(mux *http.ServeMux, reads *gasReader, gatewayURL string) {
	for _, path := range []string{gasTiersPath, gasHistoryPath} {
		var path = path
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !gasReadMethod(w, r) {
				return
			}
			var query, ok = canonicalGasTiersQuery(r.URL.RawQuery)
			if !ok {
				rejectGasRead(w)
				return
			}
			reads.serve(gatewayURL+path+"?"+query, w, r)
		})
	}
	mux.HandleFunc(gasFeePrefix+"{chainId}", func(w http.ResponseWriter, r *http.Request) {
		if !gasReadMethod(w, r) {
			return
		}
		var chain, ok = gasFeeChain(r)
		if !ok {
			rejectGasRead(w)
			return
		}
		reads.serve(gatewayURL+gasFeePrefix+chain, w, r)
	})
}

// gasReader serves the gas reads: a per-source bucket, the shared TTL cache,
// and singleflight-style coalescing, so concurrent misses for one canonical
// target cost the API a single fetch (the key space is 12 targets).
type gasReader struct {
	client     *http.Client
	cache      *ttlCache
	safeguards *relaySafeguards
	mu         sync.Mutex
	flights    map[string]*gasFlight
}

// gasFlight is one in-progress upstream fetch; done closes once result is set.
type gasFlight struct {
	done   chan struct{}
	result gasResult
}

// gasResult is one gas read as the relay answers it: failed is a 502, and a
// positive maxAge is the freshness left (whole seconds) on a cacheable 200.
type gasResult struct {
	failed      bool
	status      int
	contentType string
	body        []byte
	maxAge      int64
}

func newGasReader(client *http.Client, cache *ttlCache, safeguards *relaySafeguards) *gasReader {
	return &gasReader{client: client, cache: cache, safeguards: safeguards, flights: make(map[string]*gasFlight)}
}

// serve answers a validated gas read for the canonical target, from the cache
// within the API's freshness, otherwise from a (possibly shared) fetch.
func (g *gasReader) serve(target string, w http.ResponseWriter, r *http.Request) {
	if !g.safeguards.allowBucket(bucketGas, r.RemoteAddr, time.Now()) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if e, ok := g.cache.get(target); ok {
		writeGasRead(w, gasResult{status: e.status, contentType: e.contentType, body: e.body, maxAge: int64(e.expiresAt.Sub(timeNow()) / time.Second)})
		return
	}
	var flight = g.join(target)
	select {
	case <-flight.done:
		writeGasRead(w, flight.result)
	case <-r.Context().Done():
		// The caller left; the shared fetch still completes for the others.
		writeGasRead(w, gasResult{failed: true})
	}
}

// join returns the in-progress fetch for target, starting one if there is
// none. The fetch is detached from any one caller (it is bounded by
// upstreamTimeout), so a caller that leaves does not fail the others.
func (g *gasReader) join(target string) *gasFlight {
	g.mu.Lock()
	defer g.mu.Unlock()
	if flight, ok := g.flights[target]; ok {
		return flight
	}
	var flight = &gasFlight{done: make(chan struct{})}
	g.flights[target] = flight
	go func() {
		// fetch caches a fresh 200 before the flight ends, so a request that
		// misses the flight finds the cache.
		flight.result = g.fetch(target)
		g.mu.Lock()
		delete(g.flights, target)
		g.mu.Unlock()
		close(flight.done)
	}()
	return flight
}

// fetch performs one identity-free upstream GET. Gas reads relay API errors
// through (uncached), unlike the stricter proxyGet in relay.go, but fail
// closed on anything ambiguous: a redirect (never followed, its Location
// never relayed), an informational status, or a body that is over maxBody,
// truncated or cut by a read error is a 502, never a (cacheable) 200.
func (g *gasReader) fetch(target string) gasResult {
	var ctx, cancel = context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return gasResult{failed: true}
	}
	// Explicitly suppress the default Go User-Agent so the API sees no
	// user-agent field even though the caller's headers were already omitted.
	req.Header.Set("User-Agent", "")
	resp, err := g.client.Do(req)
	if err != nil {
		return gasResult{failed: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || (resp.StatusCode >= 300 && resp.StatusCode < 400) || resp.ContentLength > maxBody {
		return gasResult{failed: true}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody || (resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength) {
		return gasResult{failed: true}
	}
	var result = gasResult{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}
	if resp.StatusCode != http.StatusOK {
		// Never cache an error, here or downstream (CDN, browser): the next
		// request must reach the API again.
		return result
	}
	if ttl := gasFreshness(resp.Header); ttl > 0 {
		var now = timeNow()
		result.maxAge = int64(ttl / time.Second)
		g.cache.put(target, cacheEntry{status: resp.StatusCode, body: body, contentType: result.contentType, storedAt: now, expiresAt: now.Add(ttl)})
	}
	return result
}

// gasFreshness is the shared-cache freshness left on a 200 gas read: s-maxage
// (max-age only when s-maxage is absent) minus Age. Zero means no-store: Vary
// "*", no-store/no-cache/private (with or without a field list), a missing,
// malformed, repeated or zero lifetime, or an invalid or exhausted Age. Other
// Vary values are dropped: the relay sends the API the same headers for every
// caller. Keep in sync with remainingFreshness in worker/src/index.ts.
func gasFreshness(header http.Header) time.Duration {
	for _, value := range strings.Split(strings.Join(header.Values("Vary"), ","), ",") {
		if strings.TrimSpace(value) == "*" {
			return 0
		}
	}
	var ttl, ok = cacheTTL(strings.Join(header.Values("Cache-Control"), ","))
	var age, ageOK = upstreamAge(strings.Join(header.Values("Age"), ", "))
	if !ok || !ageOK || age >= ttl {
		return 0
	}
	return ttl - age
}

// writeGasRead answers with rebuilt headers only: the upstream Content-Type
// and the freshness left as max-age (no Age, no Vary, cookies or tracing), so
// a cache behind the relay never holds a read past the API's lifetime.
func writeGasRead(w http.ResponseWriter, result gasResult) {
	if result.failed {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if result.contentType != "" {
		w.Header().Set("Content-Type", result.contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	if result.maxAge > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.FormatInt(result.maxAge, 10))
	}
	w.WriteHeader(result.status)
	_, _ = w.Write(result.body)
}
