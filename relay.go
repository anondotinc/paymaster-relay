// Package main implements a keyless relay with fixed, purpose-specific gateways.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	ctReq            = "message/ohttp-req"
	ctRes            = "message/ohttp-res"
	ctConfigs        = "application/ohttp-keys-signed"
	maxBody          = 1 << 20
	maxSchedulerBody = 4 << 20
	// upstreamTimeout bounds public GET reads and key configs. sealedTimeout
	// bounds sealed POSTs and nests above the backend gateway ladder (handler
	// 25 s < forwarder 28 s < gateway 30-35 s < relay 40 s).
	upstreamTimeout = 20 * time.Second
	sealedTimeout   = 40 * time.Second
)

var publicReadPaths = []string{
	"/api/v1/paymaster/gas-quote",
	"/api/v1/paymaster/supported-tokens",
}

type relayRoute struct {
	method       string
	target       string
	responseType string
	requestLimit int64
	bucket       string
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		next.ServeHTTP(w, r)
	})
}

// New preserves the paymaster-only constructor and fails closed for invalid
// configuration. Main uses NewWithScheduler for explicit startup errors.
func New(gatewayURL string, client *http.Client) http.Handler {
	var handler, err = NewWithScheduler(gatewayURL, "", client)
	if err != nil {
		return withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
		}))
	}
	return handler
}

// NewWithScheduler adds opt-in scheduler routes. Each base is fixed by the
// operator; clients cannot select targets. Gateways retain their separate keys.
func NewWithScheduler(gatewayURL, schedulerURL string, client *http.Client) (http.Handler, error) {
	return NewWithTargets(gatewayURL, schedulerURL, "", client)
}

// NewWithTargets adds the opt-in scheduler routes (/scheduler/*) and PPOI
// routes (/ppoi/*). Each target is disabled (404) when empty, must be a valid
// base that is distinct from the paymaster's and from the other opt-in target,
// and is never inferred from another. Each route family has its own per-source
// rate-limit bucket (paymaster, scheduler, ppoi).
func NewWithTargets(gatewayURL, schedulerURL, ppoiURL string, client *http.Client) (http.Handler, error) {
	var err error
	gatewayURL, err = validateGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}
	if schedulerURL != "" {
		schedulerURL, err = validateGatewayURL(schedulerURL)
		if err != nil {
			return nil, err
		}
		if schedulerURL == gatewayURL {
			return nil, errors.New("scheduler requires a distinct gateway base")
		}
	}
	if ppoiURL != "" {
		ppoiURL, err = validateGatewayURL(ppoiURL)
		if err != nil {
			return nil, err
		}
		if ppoiURL == gatewayURL || ppoiURL == schedulerURL {
			return nil, errors.New("ppoi requires a distinct gateway base")
		}
	}
	var routes = map[string]relayRoute{
		"/health":        {method: http.MethodGet},
		"/ohttp-configs": {method: http.MethodGet, target: gatewayURL + "/ohttp-configs", responseType: ctConfigs, bucket: bucketPaymaster},
		"/gateway":       {method: http.MethodPost, target: gatewayURL + "/gateway", responseType: ctRes, requestLimit: maxBody, bucket: bucketPaymaster},
	}
	for _, routePath := range publicReadPaths {
		routes[routePath] = relayRoute{method: http.MethodGet, target: gatewayURL + routePath, responseType: "application/json", bucket: bucketPaymaster}
	}
	if schedulerURL != "" {
		routes["/scheduler/ohttp-configs"] = relayRoute{method: http.MethodGet, target: schedulerURL + "/ohttp-configs", responseType: ctConfigs, bucket: bucketScheduler}
		routes["/scheduler/gateway"] = relayRoute{method: http.MethodPost, target: schedulerURL + "/gateway", responseType: ctRes, requestLimit: maxSchedulerBody, bucket: bucketScheduler}
	}
	if ppoiURL != "" {
		routes["/ppoi/ohttp-configs"] = relayRoute{method: http.MethodGet, target: ppoiURL + "/ohttp-configs", responseType: ctConfigs, bucket: bucketPPOI}
		routes["/ppoi/gateway"] = relayRoute{method: http.MethodPost, target: ppoiURL + "/gateway", responseType: ctRes, requestLimit: maxBody, bucket: bucketPPOI}
	}
	var cache = newTTLCache()
	var safeguards = newRelaySafeguards()
	client = relayHTTPClient(client)
	// Public gas reads (gas tiers, gas history, network base fee): strictly
	// validated and canonicalized before forwarding; see gas_reads.go. They keep
	// their own mux and fresh-only proxy, and are not behind the sealed-route
	// safeguards (identity-free, cached for seconds, canonical key space).
	var gasReads = http.NewServeMux()
	var gasClient = *client
	gasClient.Timeout = upstreamTimeout
	handleGasReads(gasReads, &gasClient, cache, gatewayURL)
	return withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if isGasReadPath(r.URL.Path) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			gasReads.ServeHTTP(w, r)
			return
		}
		var route, exists = routes[r.URL.Path]
		if !exists || r.URL.EscapedPath() != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		var query, queryErr = canonicalPublicQuery(r.URL.Path, r.URL.RawQuery)
		if queryErr != nil {
			http.Error(w, "invalid query", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodOptions {
			var requested = r.Header.Get("Access-Control-Request-Method")
			if requested != "" && requested != route.method {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != route.method {
			w.Header().Set("Allow", route.method+", OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		var requestLimit = route.requestLimit
		if requestLimit == 0 {
			requestLimit = maxBody
		}
		safeguards.gatewayWithLimit(route.bucket, requestLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if route.method == http.MethodGet {
				if query != "" {
					route.target += "?" + query
				}
				proxyGet(client, cache, route.target, route.responseType, w, r)
				return
			}
			proxySealed(client, route, w, r)
		})).ServeHTTP(w, r)
	})), nil
}

func validateGatewayURL(raw string) (string, error) {
	var target, err = url.Parse(raw)
	if err != nil || target == nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(raw, "#") || (target.Scheme != "http" && target.Scheme != "https") {
		return "", errors.New("invalid gateway base URL")
	}
	if target.EscapedPath() != target.Path || strings.Contains(target.Path, "\\") {
		return "", errors.New("invalid gateway base path")
	}
	target.Path = strings.TrimRight(target.Path, "/")
	if target.Path != "" && path.Clean(target.Path) != target.Path {
		return "", errors.New("invalid gateway base path")
	}
	return target.String(), nil
}

// Restrict protobuf GET fields and values so identifiers never become
// upstream URLs or shared-cache keys. Accept both protobuf field spellings.
func canonicalPublicQuery(routePath, raw string) (string, error) {
	var invalid = errors.New("invalid query")
	if routePath != publicReadPaths[0] && routePath != publicReadPaths[1] {
		if raw != "" {
			return "", invalid
		}
		return "", nil
	}
	if len(raw) > 128 {
		return "", invalid
	}
	var values, err = url.ParseQuery(raw)
	if err != nil {
		return "", invalid
	}
	var chain string
	var priority string
	for key, items := range values {
		if len(items) != 1 {
			return "", invalid
		}
		switch key {
		case "chainId", "chain_id":
			if chain != "" {
				return "", invalid
			}
			chain = items[0]
			if chain != "1" && chain != "56" && chain != "137" && chain != "42161" {
				return "", invalid
			}
		case "priority":
			if routePath != publicReadPaths[0] {
				return "", invalid
			}
			priority = strings.ToLower(items[0])
			if priority != "" && priority != "slow" && priority != "normal" && priority != "fast" {
				return "", invalid
			}
		default:
			return "", invalid
		}
	}
	if chain == "" {
		return "", invalid
	}
	var normalized = url.Values{"chainId": {chain}}
	if priority != "" {
		normalized.Set("priority", priority)
	}
	return normalized.Encode(), nil
}

func relayHTTPClient(original *http.Client) *http.Client {
	var client http.Client
	if original != nil {
		client = *original
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 || client.Timeout > sealedTimeout {
		client.Timeout = sealedTimeout
	}
	return &client
}

func upstreamRequest(r *http.Request, method, target, accept string, body io.Reader) (*http.Request, context.CancelFunc, error) {
	var timeout = upstreamTimeout
	if method == http.MethodPost {
		timeout = sealedTimeout
	}
	var ctx, cancel = context.WithTimeout(r.Context(), timeout)
	var request, err = http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	request.Header.Set("User-Agent", "")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Accept", accept)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", ctReq)
	}
	return request, cancel, nil
}

func readUpstream(response *http.Response, expectedType string) ([]byte, error) {
	var contentType, _, err = mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != expectedType || response.StatusCode != http.StatusOK || response.ContentLength > maxBody {
		return nil, errors.New("invalid gateway response")
	}
	var encoding = response.Header.Get("Content-Encoding")
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, errors.New("unexpected gateway encoding")
	}
	var body []byte
	body, err = io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(body) == 0 || len(body) > maxBody || (response.ContentLength >= 0 && int64(len(body)) != response.ContentLength) {
		return nil, errors.New("invalid gateway response body")
	}
	return body, nil
}

func proxySealed(client *http.Client, route relayRoute, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != ctReq {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	var encoding = r.Header.Get("Content-Encoding")
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}
	var body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, route.requestLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) == 0 || (r.ContentLength >= 0 && int64(len(body)) != r.ContentLength) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var request, cancel, requestErr = upstreamRequest(r, http.MethodPost, route.target, ctRes, bytes.NewReader(body))
	if requestErr != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer cancel()
	var response *http.Response
	response, err = client.Do(request)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err = readUpstream(response, ctRes)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctRes)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func proxyGet(client *http.Client, cache *ttlCache, target, responseType string, w http.ResponseWriter, r *http.Request) {
	var cached, exists = cache.get(target)
	if exists {
		writeCached(w, cached)
		return
	}
	var request, cancel, err = upstreamRequest(r, http.MethodGet, target, responseType, nil)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer cancel()
	var response *http.Response
	response, err = client.Do(request)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	var body []byte
	body, err = readUpstream(response, responseType)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	var cacheControl = response.Header.Get("Cache-Control")
	var ttl, cacheable = cacheTTL(cacheControl)
	// upstreamAge (cache.go) parses the upstream Age; an invalid or exhausted
	// Age means the response is stale and must not be cached.
	var age, ageOK = upstreamAge(response.Header.Get("Age"))
	if !ageOK || age >= ttl {
		cacheable = false
	} else {
		ttl -= age
	}
	cacheControl = "no-store"
	if cacheable && ttl > 0 && response.Header.Get("Vary") == "" {
		cacheControl = "public, max-age=" + strconv.FormatInt(int64(ttl/time.Second), 10)
		var now = timeNow()
		cache.put(target, cacheEntry{status: response.StatusCode, body: body, contentType: responseType, cacheCtl: cacheControl, storedAt: now, expiresAt: now.Add(ttl)})
	}
	w.Header().Set("Content-Type", responseType)
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func writeCached(w http.ResponseWriter, entry cacheEntry) {
	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("Cache-Control", entry.cacheCtl)
	var age = int(timeNow().Sub(entry.storedAt).Seconds())
	if age < 0 {
		age = 0
	}
	w.Header().Set("Age", strconv.Itoa(age))
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}
