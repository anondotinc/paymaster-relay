package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type gasRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip gasRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// gasUpstream answers like the API: JSON with the given status, Cache-Control
// and optional Age.
func gasUpstream(status int, cacheControl, age, body string) *http.Response {
	var header = http.Header{"Content-Type": {"application/json"}, "Vary": {"Origin, accept-encoding"}}
	if cacheControl != "" {
		header.Set("Cache-Control", cacheControl)
	}
	if age != "" {
		header.Set("Age", age)
	}
	header.Set("Set-Cookie", "upstream=secret")
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func gasRead(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	var request = httptest.NewRequest(method, target, nil)
	addClientIdentityHeaders(request)
	request.Header.Set("Cookie", "client=secret")
	var response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestGasReadsProxied asserts both gas reads reach the fixed API with only the
// canonical chain — no client header, cookie or extra query — and relay the
// API's JSON, Cache-Control and CORS.
func TestGasReadsProxied(t *testing.T) {
	const fresh = "public, s-maxage=5, max-age=5"
	var cases = []struct{ path, target, body string }{
		{"/api/v1/paymaster/gas-tiers?chainId=42161", "https://api.test/api/v1/paymaster/gas-tiers?chainId=42161", `{"chainId":"42161","tiers":[]}`},
		{"/api/v1/paymaster/gas-tiers?chain_id=1", "https://api.test/api/v1/paymaster/gas-tiers?chainId=1", `{"chainId":"1","tiers":[]}`},
		{"/api/v1/paymaster/gas-history?chainId=137", "https://api.test/api/v1/paymaster/gas-history?chainId=137", `{"chainId":"137","points":[],"windowSeconds":1800}`},
		{"/api/v1/paymaster/gas-history?chain_id=1", "https://api.test/api/v1/paymaster/gas-history?chainId=1", `{"chainId":"1","windowSeconds":1800}`},
		{"/api/v1/tx/gas-fee/42161", "https://api.test/api/v1/tx/gas-fee/42161", `{"chainId":"42161","pendingBaseFee":"0.02"}`},
		{"/api/v1/tx/gas-fee/56", "https://api.test/api/v1/tx/gas-fee/56", `{"chainId":"56","pendingBaseFee":"0.1"}`},
	}
	for _, testCase := range cases {
		var targets []string
		var client = &http.Client{Transport: gasRoundTripper(func(request *http.Request) (*http.Response, error) {
			targets = append(targets, request.URL.String())
			assertNoClientIdentityHeaders(t, request.Header)
			if request.Header.Get("Cookie") != "" {
				t.Errorf("%s: cookie forwarded", testCase.path)
			}
			return gasUpstream(http.StatusOK, fresh, "", testCase.body), nil
		})}
		var response = gasRead(New("https://api.test", client), http.MethodGet, testCase.path)
		if response.Code != http.StatusOK || len(targets) != 1 || targets[0] != testCase.target {
			t.Fatalf("%s: status %d, targets %v", testCase.path, response.Code, targets)
		}
		if response.Body.String() != testCase.body {
			t.Errorf("%s: body %q", testCase.path, response.Body.String())
		}
		var header = response.Header()
		if header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != fresh {
			t.Errorf("%s: headers %v", testCase.path, header)
		}
		if header.Get("Access-Control-Allow-Origin") != "*" || header.Get("Access-Control-Allow-Methods") != "GET, POST, OPTIONS" {
			t.Errorf("%s: CORS headers %v", testCase.path, header)
		}
		if header.Get("Set-Cookie") != "" || header.Get("Vary") != "" {
			t.Errorf("%s: upstream response headers leaked: %v", testCase.path, header)
		}
	}
}

// TestGasReadsRejectInvalid asserts the strict validation: exactly one allowed
// chain, no other field, no query on the path route, no encoded or extra path
// segments. Nothing invalid reaches the API, and no refusal is cacheable.
func TestGasReadsRejectInvalid(t *testing.T) {
	var client = &http.Client{Transport: gasRoundTripper(func(request *http.Request) (*http.Response, error) {
		t.Errorf("invalid gas read reached the API: %s", request.URL)
		return nil, errors.New("must not dispatch")
	})}
	var handler = New("https://api.test", client)
	var cases = []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/paymaster/gas-tiers", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=999", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=01", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=-1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1.0", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=+1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1&chainId=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1&chain_id=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=&chain_id=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1&priority=fast", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1&account=secret", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1;account=secret", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1&url=https://attacker.test", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1%zz", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1" + strings.Repeat("&", 128), 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history?chainId=999", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history?chainId=01", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history?chainId=1&chainId=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history?chainId=1&window=86400", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-history?chainId=1&account=secret", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/999", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/01", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/-1", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/secret", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/%34%32161", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161?blockCount=5", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161?chainId=1", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161?", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161%2Fextra", 400},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161/", 404},
		{http.MethodGet, "/api/v1/tx/gas-fee/42161/extra", 404},
		{http.MethodGet, "/api/v1/tx/gas-fee", 404},
		{http.MethodGet, "/api/v1/tx/gas-fees/42161", 404},
		{http.MethodGet, "/api/v1/paymaster/gas-tiers/1", 404},
		{http.MethodPost, "/api/v1/paymaster/gas-tiers?chainId=1", 405},
		{http.MethodPost, "/api/v1/tx/gas-fee/42161", 405},
	}
	for _, testCase := range cases {
		var response = gasRead(handler, testCase.method, testCase.path)
		if response.Code != testCase.status {
			t.Errorf("%s %s: got %d want %d", testCase.method, testCase.path, response.Code, testCase.status)
		}
		if testCase.status == http.StatusBadRequest && response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: refusal is cacheable", testCase.method, testCase.path)
		}
	}
}

func TestCanonicalGasTiersQuery(t *testing.T) {
	var cases = []struct{ raw, want string }{
		{"chainId=1", "chainId=1"},
		{"chainId=56", "chainId=56"},
		{"chainId=137", "chainId=137"},
		{"chain_id=42161", "chainId=42161"},
		{"chainId=%31", "chainId=1"},
		{"chainId=1&", "chainId=1"},
	}
	for _, testCase := range cases {
		var got, ok = canonicalGasTiersQuery(testCase.raw)
		if !ok || got != testCase.want {
			t.Errorf("%q: %q %v", testCase.raw, got, ok)
		}
	}
}

// TestGasReadsFreshOnly asserts the relay caches a gas read only for what is
// left of the API's freshness: never past s-maxage, never resetting an
// upstream Age, and passing the Age on so a downstream cache cannot extend it.
func TestGasReadsFreshOnly(t *testing.T) {
	var realNow = timeNow
	defer func() { timeNow = realNow }()
	var base = time.Unix(1_700_000_000, 0)
	var now = base
	timeNow = func() time.Time { return now }

	var age string
	var calls int
	var client = &http.Client{Transport: gasRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return gasUpstream(http.StatusOK, "public, s-maxage=5, max-age=5", age, `{"chainId":"42161"}`), nil
	})}
	var handler = New("https://api.test", client)
	const path = "/api/v1/tx/gas-fee/42161"

	// No upstream Age: cached for the full 5s, then refetched.
	var response = gasRead(handler, http.MethodGet, path)
	if calls != 1 || response.Header().Get("Age") != "" {
		t.Fatalf("fresh fetch: calls %d, age %q", calls, response.Header().Get("Age"))
	}
	now = base.Add(4 * time.Second)
	response = gasRead(handler, http.MethodGet, path)
	if calls != 1 || response.Header().Get("Age") != "4" || response.Header().Get("Cache-Control") != "public, s-maxage=5, max-age=5" {
		t.Fatalf("cache hit: calls %d, headers %v", calls, response.Header())
	}
	now = base.Add(5 * time.Second)
	gasRead(handler, http.MethodGet, path)
	if calls != 2 {
		t.Fatalf("served past s-maxage: calls %d", calls)
	}

	// Upstream Age 3: only 2s left, and the Age travels with the response.
	handler = New("https://api.test", client)
	now, calls, age = base, 0, "3"
	response = gasRead(handler, http.MethodGet, path)
	if response.Header().Get("Age") != "3" {
		t.Fatalf("fresh fetch dropped upstream Age: %v", response.Header())
	}
	now = base.Add(1 * time.Second)
	response = gasRead(handler, http.MethodGet, path)
	if calls != 1 || response.Header().Get("Age") != "4" {
		t.Fatalf("cache hit reset upstream Age: calls %d, age %q", calls, response.Header().Get("Age"))
	}
	now = base.Add(2 * time.Second)
	gasRead(handler, http.MethodGet, path)
	if calls != 2 {
		t.Fatalf("upstream Age extended freshness: calls %d", calls)
	}

	// Exhausted or invalid Age: stale, so neither cached nor cacheable.
	for _, stale := range []string{"5", "60", "x", "-1", "+1", "1, 2"} {
		handler = New("https://api.test", client)
		now, calls, age = base, 0, stale
		for i := 0; i < 2; i++ {
			response = gasRead(handler, http.MethodGet, path)
			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Age") != "" {
				t.Fatalf("Age %q: status %d, headers %v", stale, response.Code, response.Header())
			}
		}
		if calls != 2 {
			t.Fatalf("Age %q: stale response cached (calls %d)", stale, calls)
		}
	}
}

// TestGasReadsNeverCacheErrors asserts API errors (400 for a chain the
// paymaster does not serve, 503 with no base fee) pass through uncached and
// uncacheable, even when the API mislabels them as cacheable.
func TestGasReadsNeverCacheErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		var calls int
		var client = &http.Client{Transport: gasRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			return gasUpstream(status, "public, s-maxage=60, max-age=60", "", `{"code":"error"}`), nil
		})}
		var handler = New("https://api.test", client)
		for _, path := range []string{"/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1", "/api/v1/tx/gas-fee/1"} {
			var response = gasRead(handler, http.MethodGet, path)
			if response.Code != status || response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != `{"code":"error"}` {
				t.Fatalf("%d %s: status %d, headers %v, body %q", status, path, response.Code, response.Header(), response.Body.String())
			}
		}
		if calls != 4 {
			t.Fatalf("%d: error cached (calls %d)", status, calls)
		}
	}

	var client = &http.Client{Transport: gasRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	var response = gasRead(New("https://api.test", client), http.MethodGet, "/api/v1/tx/gas-fee/1")
	if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("transport error: %d %v", response.Code, response.Header())
	}
}

// TestGasReadsPreflight asserts browsers can preflight the gas reads like the
// other public reads.
func TestGasReadsPreflight(t *testing.T) {
	var handler = New("https://api.test", &http.Client{Transport: gasRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("preflight reached the API")
		return nil, errors.New("must not dispatch")
	})})
	for _, path := range []string{"/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1"} {
		var request = httptest.NewRequest(http.MethodOptions, path, nil)
		request.Header.Set("Origin", "https://app.test")
		request.Header.Set("Access-Control-Request-Method", http.MethodGet)
		var response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: preflight %d %v", path, response.Code, response.Header())
		}
	}
}

func TestUpstreamAge(t *testing.T) {
	var cases = []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", 0, true},
		{"0", 0, true},
		{"3", 3 * time.Second, true},
		{"x", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"1, 2", 0, false},
		{"99999999999999999999", 0, false},
	}
	for _, testCase := range cases {
		var got, ok = upstreamAge(testCase.value)
		if got != testCase.want || ok != testCase.ok {
			t.Errorf("upstreamAge(%q) = (%v,%v), want (%v,%v)", testCase.value, got, ok, testCase.want, testCase.ok)
		}
	}
}
