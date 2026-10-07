package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// TestGasReadsProxied asserts the gas reads reach the fixed API with only the
// canonical chain — no client header, cookie or extra query — and relay the
// API's JSON with rebuilt Cache-Control, nosniff and CORS.
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
		if header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != "public, max-age=5" || header.Get("X-Content-Type-Options") != "nosniff" {
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
		{http.MethodHead, "/api/v1/paymaster/gas-history?chainId=1", 405},
		{http.MethodHead, "/api/v1/tx/gas-fee/42161", 405},
		{http.MethodPost, "/api/v1/tx/gas-fee/999", 405},
		{http.MethodOptions, "/api/v1/tx/gas-fee/42161/extra", 404},
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
// upstream Age, and advertising only the remaining seconds (max-age, no Age)
// so a downstream cache cannot extend it either.
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
	var expect = func(step string, wantCalls int, cacheControl string) {
		t.Helper()
		var response = gasRead(handler, http.MethodGet, path)
		if calls != wantCalls || response.Code != http.StatusOK || response.Header().Get("Cache-Control") != cacheControl || response.Header().Get("Age") != "" {
			t.Fatalf("%s: calls %d, status %d, headers %v", step, calls, response.Code, response.Header())
		}
	}

	// No upstream Age: cached for the full 5s, each hit advertising what is left.
	expect("fresh fetch", 1, "public, max-age=5")
	now = base.Add(4 * time.Second)
	expect("hit at 4s", 1, "public, max-age=1")
	now = base.Add(4500 * time.Millisecond)
	expect("hit under 1s left", 1, "no-store")
	now = base.Add(5 * time.Second)
	expect("past s-maxage", 2, "public, max-age=5")

	// Upstream Age 3: only 2s left.
	handler = New("https://api.test", client)
	now, calls, age = base, 0, "3"
	expect("fresh fetch with Age", 1, "public, max-age=2")
	now = base.Add(1 * time.Second)
	expect("hit with Age", 1, "public, max-age=1")
	now = base.Add(2 * time.Second)
	expect("upstream Age exhausted", 2, "public, max-age=2")
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

// gasReadVectors is testdata/gas_read_vectors.json, shared with the Worker
// tests (worker/test/gas-reads.test.mjs) so both implementations keep one rule set.
type gasReadVectors struct {
	Path    string `json:"path"`
	Target  string `json:"target"`
	MaxBody int    `json:"maxBody"`
	Vectors []struct {
		Name     string `json:"name"`
		Method   string `json:"method"`
		Path     string `json:"path"`
		Upstream *struct {
			Status        int               `json:"status"`
			Headers       map[string]string `json:"headers"`
			Body          *string           `json:"body"`
			BodyBytes     int               `json:"bodyBytes"`
			ContentLength *int64            `json:"contentLength"`
			BodyError     bool              `json:"bodyError"`
		} `json:"upstream"`
		Expect struct {
			Status        int     `json:"status"`
			CacheControl  string  `json:"cacheControl"`
			Body          *string `json:"body"`
			BodyBytes     int     `json:"bodyBytes"`
			Cached        bool    `json:"cached"`
			Allow         string  `json:"allow"`
			UpstreamCalls *int    `json:"upstreamCalls"`
		} `json:"expect"`
	} `json:"vectors"`
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestGasReadVectors(t *testing.T) {
	var raw, err = os.ReadFile("testdata/gas_read_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file gasReadVectors
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.MaxBody != maxBody || len(file.Vectors) == 0 {
		t.Fatalf("vector file: maxBody %d, %d vectors", file.MaxBody, len(file.Vectors))
	}
	// Both attempts happen at the same instant, so a hit advertises the full
	// remaining freshness (TestGasReadsFreshOnly covers the clock moving).
	var realNow = timeNow
	defer func() { timeNow = realNow }()
	var frozen = time.Unix(1_700_000_000, 0)
	timeNow = func() time.Time { return frozen }
	for _, vector := range file.Vectors {
		var calls int
		var client = &http.Client{Transport: gasRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.String() != "https://api.test"+file.Target {
				t.Errorf("%s: target %s", vector.Name, request.URL)
			}
			var upstream = vector.Upstream
			if upstream == nil {
				return gasUpstream(http.StatusOK, "public, s-maxage=5, max-age=5", "", "{}"), nil
			}
			var header = http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"upstream=secret"}, "X-Upstream-Trace": {"trace"}}
			for name, value := range upstream.Headers {
				header.Set(name, value)
			}
			var body = strings.Repeat("x", upstream.BodyBytes)
			if upstream.Body != nil {
				body = *upstream.Body
			}
			var reader io.Reader = strings.NewReader(body)
			var length = int64(len(body))
			if upstream.ContentLength != nil {
				length = *upstream.ContentLength
			}
			if upstream.BodyError {
				reader, length = io.MultiReader(reader, failingBody{}), -1
			}
			return &http.Response{StatusCode: upstream.Status, Header: header, Body: io.NopCloser(reader), ContentLength: length}, nil
		})}
		var handler = New("https://api.test", client)
		var method = vector.Method
		if method == "" {
			method = http.MethodGet
		}
		var path = file.Path
		if vector.Path != "" {
			path = vector.Path
		}
		for attempt := 1; attempt <= 2; attempt++ {
			var response = gasRead(handler, method, path)
			var header = response.Header()
			if response.Code != vector.Expect.Status || header.Get("Cache-Control") != vector.Expect.CacheControl {
				t.Errorf("%s (#%d): status %d, Cache-Control %q", vector.Name, attempt, response.Code, header.Get("Cache-Control"))
			}
			if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("%s (#%d): headers %v", vector.Name, attempt, header)
			}
			for _, name := range []string{"Location", "Set-Cookie", "Vary", "Age", "X-Upstream-Trace"} {
				if header.Get(name) != "" {
					t.Errorf("%s (#%d): %s relayed", vector.Name, attempt, name)
				}
			}
			if vector.Expect.Allow != "" && header.Get("Allow") != vector.Expect.Allow {
				t.Errorf("%s (#%d): Allow %q", vector.Name, attempt, header.Get("Allow"))
			}
			if vector.Expect.Body != nil && response.Body.String() != *vector.Expect.Body {
				t.Errorf("%s (#%d): body %q", vector.Name, attempt, response.Body.String())
			}
			if vector.Expect.BodyBytes > 0 && response.Body.Len() != vector.Expect.BodyBytes {
				t.Errorf("%s (#%d): body length %d", vector.Name, attempt, response.Body.Len())
			}
		}
		var wantCalls = 2
		switch {
		case vector.Expect.UpstreamCalls != nil:
			wantCalls = *vector.Expect.UpstreamCalls
		case vector.Expect.Cached:
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Errorf("%s: %d upstream calls, want %d", vector.Name, calls, wantCalls)
		}
	}
}

// TestGasReadsCoalesce asserts concurrent misses for one canonical target
// share a single upstream fetch, even when its answer is not cacheable.
func TestGasReadsCoalesce(t *testing.T) {
	// blockingClient answers every fetch only once release is closed.
	var blockingClient = func(calls *atomic.Int32, entered, release chan struct{}) *http.Client {
		return &http.Client{Transport: gasRoundTripper(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return gasUpstream(http.StatusOK, "no-store", "", `{"chainId":"1"}`), nil
		})}
	}
	var calls atomic.Int32
	var entered, release = make(chan struct{}), make(chan struct{})
	var reads = newGasReader(blockingClient(&calls, entered, release), newTTLCache(), newRelaySafeguards())
	const target = "https://api.test/api/v1/tx/gas-fee/1"
	var flight = reads.join(target)
	<-entered
	for i := 0; i < 20; i++ {
		if reads.join(target) != flight {
			t.Fatal("a concurrent miss started a second fetch")
		}
	}
	var other = reads.join("https://api.test/api/v1/tx/gas-fee/56")
	if other == flight {
		t.Fatal("different targets were coalesced")
	}
	close(release)
	<-flight.done
	<-other.done
	if flight.result.failed || flight.result.status != http.StatusOK || string(flight.result.body) != `{"chainId":"1"}` || flight.result.maxAge != 0 {
		t.Fatalf("shared result %+v", flight.result)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls %d, want 2 (one per target)", calls.Load())
	}

	// Through the handler: every waiter gets the one shared answer.
	var handlerCalls atomic.Int32
	var handlerEntered, handlerRelease = make(chan struct{}), make(chan struct{})
	var handler = New("https://api.test", blockingClient(&handlerCalls, handlerEntered, handlerRelease))
	var group sync.WaitGroup
	var responses = make([]*httptest.ResponseRecorder, 10)
	for i := range responses {
		group.Add(1)
		go func() {
			defer group.Done()
			responses[i] = gasRead(handler, http.MethodGet, "/api/v1/tx/gas-fee/1")
		}()
	}
	<-handlerEntered
	time.Sleep(20 * time.Millisecond)
	close(handlerRelease)
	group.Wait()
	for _, response := range responses {
		if response.Code != http.StatusOK || response.Body.String() != `{"chainId":"1"}` || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("waiter: %d %v %q", response.Code, response.Header(), response.Body.String())
		}
	}
	if handlerCalls.Load() < 1 || handlerCalls.Load() > int32(len(responses)) {
		t.Fatalf("upstream calls %d", handlerCalls.Load())
	}
}

// TestGasReadsSourceBucket asserts the gas reads have their own per-source
// budget: it trips after maxGasReadsPerMinute (cache hits included), does not
// affect another source, and never consumes the paymaster's sealed budget.
func TestGasReadsSourceBucket(t *testing.T) {
	var calls int
	var client = &http.Client{Transport: gasRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path == "/gateway" {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {ctRes}}, Body: io.NopCloser(strings.NewReader("sealed")), ContentLength: 6}, nil
		}
		return gasUpstream(http.StatusOK, "public, s-maxage=5, max-age=5", "", "{}"), nil
	})}
	var handler = New("https://api.test", client)
	for i := 0; i < maxGasReadsPerMinute; i++ {
		if response := gasRead(handler, http.MethodGet, "/api/v1/tx/gas-fee/1"); response.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i+1, response.Code)
		}
	}
	var limited = gasRead(handler, http.MethodGet, "/api/v1/paymaster/gas-tiers?chainId=1")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" || limited.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("over budget: %d %v", limited.Code, limited.Header())
	}
	if calls != 1 {
		t.Fatalf("upstream calls %d, want 1 (the rest are cache hits)", calls)
	}
	var other = httptest.NewRequest(http.MethodGet, "/api/v1/tx/gas-fee/1", nil)
	other.RemoteAddr = "198.51.100.7:1234"
	var response = httptest.NewRecorder()
	handler.ServeHTTP(response, other)
	if response.Code != http.StatusOK {
		t.Fatalf("other source: %d", response.Code)
	}
	var sealed = httptest.NewRequest(http.MethodPost, "/gateway", strings.NewReader("sealed"))
	sealed.Header.Set("Content-Type", ctReq)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, sealed)
	if response.Code != http.StatusOK {
		t.Fatalf("sealed request after gas budget: %d", response.Code)
	}
}
