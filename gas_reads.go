package main

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// handleGasReads registers the two gas reads on mux. Each forwards only the
// canonical target through proxyGasGet, which honours (never extends) the API's
// Cache-Control and never caches an error.
func handleGasReads(mux *http.ServeMux, client *http.Client, cache *ttlCache, gatewayURL string) {
	for _, path := range []string{gasTiersPath, gasHistoryPath} {
		var path = path
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			var query, ok = canonicalGasTiersQuery(r.URL.RawQuery)
			if !ok {
				rejectGasRead(w)
				return
			}
			proxyGasGet(client, cache, gatewayURL+path+"?"+query, w, r)
		})
	}
	mux.HandleFunc("GET "+gasFeePrefix+"{chainId}", func(w http.ResponseWriter, r *http.Request) {
		var chain, ok = gasFeeChain(r)
		if !ok {
			rejectGasRead(w)
			return
		}
		proxyGasGet(client, cache, gatewayURL+gasFeePrefix+chain, w, r)
	})
}

// proxyGasGet forwards a GET to url and copies the upstream status, Content-Type,
// and body back to w. Used for the gas reads, which (unlike the other public reads) relay API
// errors through uncached and honour an upstream Age; see gas_reads_test.go.
// The other public reads use the stricter proxyGet in relay.go. Successful
// (200) responses are cached for as long as the upstream Cache-Control: max-age
// permits (cacheTTL); within that window the relay answers from cache without
// touching the paymaster. Fresh (non-cached) responses stream through; cache
// hits carry an Age header.
func proxyGasGet(client *http.Client, cache *ttlCache, url string, w http.ResponseWriter, r *http.Request) {
	if e, ok := cache.get(url); ok {
		writeGasCached(w, e)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	// proxyGasGet reconstructs an identity-free request. Explicitly suppress the
	// default Go User-Agent so public quote/token telemetry has no user-agent
	// field even though the caller's headers were already omitted.
	req.Header.Set("User-Agent", "")
	resp, err := client.Do(req)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	ct := resp.Header.Get("Content-Type")
	cc := resp.Header.Get("Cache-Control")
	age, ageOK := upstreamAge(resp.Header.Get("Age"))
	switch {
	case resp.StatusCode != http.StatusOK:
		// Never cache an error, here or downstream (CDN, browser): the next
		// request must reach the paymaster again.
		cc = "no-store"
	case !ageOK:
		// RFC 9111 §5.1: an invalid Age means the response must be treated as stale.
		cc = "no-store"
	default:
		// Cache only 200s, and only for what is LEFT of the TTL the paymaster
		// grants (cacheTTL returns ok=false for no-store/no-cache/private or a
		// missing max-age). An upstream Age counts against it, so a response that
		// already sat in an upstream cache is never kept past its freshness.
		if ttl, ok := cacheTTL(cc); ok {
			if age >= ttl {
				cc = "no-store"
				break
			}
			now := timeNow()
			cache.put(url, cacheEntry{
				status:      resp.StatusCode,
				body:        body,
				contentType: ct,
				cacheCtl:    cc,
				upstreamAge: age,
				storedAt:    now,
				expiresAt:   now.Add(ttl - age),
			})
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Preserve cacheability so a CDN in front of the relay can cache the
	// (public) keyconfig / gas-quote / supported-tokens responses too. Age is
	// passed on with it, so a downstream cache cannot extend the freshness.
	if cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	if age > 0 && cc != "no-store" {
		w.Header().Set("Age", strconv.Itoa(int(age/time.Second)))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// writeGasCached serves a cached entry, tagging it with an Age header (the
// upstream Age plus seconds since it was stored) as an HTTP cache should.
func writeGasCached(w http.ResponseWriter, e cacheEntry) {
	if e.contentType != "" {
		w.Header().Set("Content-Type", e.contentType)
	}
	if e.cacheCtl != "" {
		w.Header().Set("Cache-Control", e.cacheCtl)
	}
	age := timeNow().Sub(e.storedAt)
	if age < 0 {
		age = 0
	}
	w.Header().Set("Age", strconv.Itoa(int((e.upstreamAge+age)/time.Second)))
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}
