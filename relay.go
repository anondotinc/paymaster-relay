// Package main implements a keyless Oblivious HTTP relay: it forwards
// encapsulated requests to a fixed gateway and returns the encapsulated
// response. It holds NO keys and sees NO plaintext — safe for any operator to
// run.
package main

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	ctReq   = "message/ohttp-req"
	ctRes   = "message/ohttp-res"
	maxBody = 1 << 20 // 1 MiB
)

// The relay is a public, keyless service intended to be called directly by
// third-party browser apps. It uses no cookies or other browser credentials,
// so a wildcard origin is both sufficient and easier for integrators than an
// origin allowlist.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// publicReadPaths are the paymaster's public, cacheable, identity-free GET
// endpoints the relay proxies so the client can fetch them WITHOUT exposing its
// IP to the paymaster. They carry no client identity and are byte-identical per
// (chain, priority, cache-window), so no HPKE sealing is needed — the relay
// forwards path+query verbatim. Allowlisted so the relay is never an open proxy.
var publicReadPaths = []string{
	"/api/v1/paymaster/gas-quote",
	"/api/v1/paymaster/supported-tokens",
}

// New builds the relay's HTTP handler. It proxies the keyconfig fetch
// (GET /ohttp-configs) and the public read endpoints (gas-quote, supported-tokens)
// so the client can run the whole flow — quote, execute, status — through the
// relay without ever contacting the paymaster directly, and forwards encapsulated
// requests (POST /gateway) to the fixed gatewayURL, relaying back the encapsulated
// response. Only encapsulated bodies / allowlisted public GETs cross the relay —
// no client headers, no PII, no keys. Public GETs are cached per the paymaster's
// Cache-Control TTL (shared across clients; see cache.go).
func New(gatewayURL string, client *http.Client) http.Handler {
	mux := http.NewServeMux()
	cache := newTTLCache()
	safeguards := newRelaySafeguards()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	mux.HandleFunc("GET /ohttp-configs", func(w http.ResponseWriter, r *http.Request) {
		proxyGet(client, cache, gatewayURL+"/ohttp-configs", w, r)
	})

	// Public read endpoints: forward path + query (no client headers, no PII),
	// cached per the paymaster's Cache-Control TTL.
	for _, p := range publicReadPaths {
		path := p
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			target := gatewayURL + r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			proxyGet(client, cache, target, w, r)
		})
	}

	// Public gas reads (gas tiers, network base fee): strictly validated and
	// canonicalized before forwarding; see gas_reads.go.
	handleGasReads(mux, client, cache, gatewayURL)

	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			body []byte
			req  *http.Request
			resp *http.Response
			err  error
		)
		if r.Header.Get("Content-Type") != ctReq {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		body, err = io.ReadAll(r.Body)
		if err != nil {
			if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Forward ONLY the encapsulated body + content-type. No client headers, no PII.
		req, err = http.NewRequestWithContext(r.Context(), http.MethodPost, gatewayURL+"/gateway", bytesReader(body))
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		req.Header.Set("Content-Type", ctReq)
		// Suppress net/http's generic User-Agent too. It is not copied from the
		// caller, but omitting it keeps gateway telemetry entirely free of a
		// user-agent dimension and makes this allowlist exact.
		req.Header.Set("User-Agent", "")
		resp, err = client.Do(req)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if readErr != nil || len(responseBody) > maxBody {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", ctRes)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	})
	mux.Handle("POST /gateway", safeguards.gateway(gateway))

	return withCORS(mux)
}

// proxyGet forwards a GET to url and copies the upstream status, Content-Type,
// and body back to w. Used for the keyconfig passthrough and the public read
// endpoints so the client never connects to the gateway directly. Successful
// (200) responses are cached for as long as the upstream Cache-Control: max-age
// permits (cacheTTL); within that window the relay answers from cache without
// touching the paymaster. Fresh (non-cached) responses stream through; cache
// hits carry an Age header.
func proxyGet(client *http.Client, cache *ttlCache, url string, w http.ResponseWriter, r *http.Request) {
	if e, ok := cache.get(url); ok {
		writeCached(w, e)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	// proxyGet reconstructs an identity-free request. Explicitly suppress the
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

// writeCached serves a cached entry, tagging it with an Age header (the
// upstream Age plus seconds since it was stored) as an HTTP cache should.
func writeCached(w http.ResponseWriter, e cacheEntry) {
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

// bytesReader wraps a byte slice as an io.Reader for the forwarded request body.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
