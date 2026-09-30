package main

import (
	"net/http"
	"net/url"
)

// Public gas reads: the paymaster's gas tiers and the API's network base fee.
// Both are identity-free and fresh-only (the API answers
// "public, s-maxage=5, max-age=5"), so the SDK reads them through the relay
// instead of calling the API directly. Unlike the verbatim publicReadPaths,
// the relay forwards ONLY a validated, canonical chainId on a fixed path: no
// caller query, path segment or header reaches the API or the cache key.
// Keep in sync with GAS_READ_CHAINS / gasReadTarget in worker/src/index.ts.
const (
	gasTiersPath = "/api/v1/paymaster/gas-tiers"
	gasFeePrefix = "/api/v1/tx/gas-fee/"
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

// rejectGasRead answers an invalid gas read without contacting the API. The
// refusal is never cacheable.
func rejectGasRead(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "invalid gas read", http.StatusBadRequest)
}

// handleGasReads registers the two gas reads on mux. Each forwards only the
// canonical target through proxyGet, which honours (never extends) the API's
// Cache-Control and never caches an error.
func handleGasReads(mux *http.ServeMux, client *http.Client, cache *ttlCache, gatewayURL string) {
	mux.HandleFunc("GET "+gasTiersPath, func(w http.ResponseWriter, r *http.Request) {
		var query, ok = canonicalGasTiersQuery(r.URL.RawQuery)
		if !ok {
			rejectGasRead(w)
			return
		}
		proxyGet(client, cache, gatewayURL+gasTiersPath+"?"+query, w, r)
	})
	mux.HandleFunc("GET "+gasFeePrefix+"{chainId}", func(w http.ResponseWriter, r *http.Request) {
		var chain, ok = gasFeeChain(r)
		if !ok {
			rejectGasRead(w)
			return
		}
		proxyGet(client, cache, gatewayURL+gasFeePrefix+chain, w, r)
	})
}
