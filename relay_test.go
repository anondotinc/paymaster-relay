package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestGatewayForwards asserts POST /gateway forwards the encapsulated body and
// its Content-Type to the fixed gateway, and relays the gateway's
// message/ohttp-res response back unchanged.
func TestGatewayForwards(t *testing.T) {
	const reqBody = "encapsulated-request-bytes"
	const resBody = "encapsulated-response-bytes"

	var (
		gotPath string
		gotCT   string
		gotBody []byte
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", ctRes)
		_, _ = w.Write([]byte(resBody))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	resp, err := http.Post(relay.URL+"/gateway", ctReq, bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotPath != "/gateway" {
		t.Fatalf("forwarded path = %q, want /gateway", gotPath)
	}
	if gotCT != ctReq {
		t.Fatalf("forwarded content-type = %q, want %q", gotCT, ctReq)
	}
	if string(gotBody) != reqBody {
		t.Fatalf("forwarded body = %q, want %q", gotBody, reqBody)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != ctRes {
		t.Fatalf("relay response content-type = %q, want %q", ct, ctRes)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != resBody {
		t.Fatalf("relay response body = %q, want %q", body, resBody)
	}
}

// TestGatewayRejectsWrongContentType asserts a non-OHTTP request is rejected at
// the relay without touching the gateway.
func TestGatewayRejectsWrongContentType(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("gateway must not be reached for a wrong content-type")
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	resp, err := http.Post(relay.URL+"/gateway", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

// TestConfigsProxied asserts GET /ohttp-configs proxies the gateway's keyconfig
// (body + Content-Type) so the client never connects to the gateway directly.
func TestConfigsProxied(t *testing.T) {
	const cfgBody = "signed-keyconfig-bytes"
	const cfgCT = "application/ohttp-keys-signed"

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ohttp-configs" {
			t.Errorf("unexpected backend path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", cfgCT)
		_, _ = w.Write([]byte(cfgBody))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	resp, err := http.Get(relay.URL + "/ohttp-configs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != cfgCT {
		t.Fatalf("content-type = %q, want %q", ct, cfgCT)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != cfgBody {
		t.Fatalf("body = %q, want %q", body, cfgBody)
	}
}

// TestPublicReadProxied asserts the relay proxies the paymaster's public read
// endpoints (gas-quote/supported-tokens) forwarding path+query and preserving
// Content-Type + Cache-Control — so the client fetches the quote via the relay,
// never exposing its IP to the paymaster.
func TestPublicReadProxied(t *testing.T) {
	const quoteBody = `{"quoteId":"tok","chainId":"1"}`

	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=50")
		_, _ = w.Write([]byte(quoteBody))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	resp, err := http.Get(relay.URL + "/api/v1/paymaster/gas-quote?chainId=1&priority=fast")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/api/v1/paymaster/gas-quote" {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	if gotQuery != "chainId=1&priority=fast" {
		t.Fatalf("forwarded query = %q, want chainId=1&priority=fast", gotQuery)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=50" {
		t.Fatalf("cache-control = %q", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != quoteBody {
		t.Fatalf("body = %q", body)
	}
}

// TestPublicReadCachedHonorsTTL asserts the relay caches a public read for the
// paymaster's Cache-Control max-age (serving from cache with an Age header) and
// refetches once that TTL expires — so it never serves a quote staler than the
// paymaster permits.
func TestPublicReadCachedHonorsTTL(t *testing.T) {
	realNow := timeNow
	defer func() { timeNow = realNow }()
	base := time.Unix(1_700_000_000, 0)
	timeNow = func() time.Time { return base }

	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write([]byte(`{"quoteId":"q"}`))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	do := func() (string, string) {
		resp, err := http.Get(relay.URL + "/api/v1/paymaster/gas-quote?chainId=1&priority=fast")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Header.Get("Age")
	}

	b1, _ := do()
	b2, age2 := do()
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 (second served from cache)", hits.Load())
	}
	if b1 != b2 {
		t.Fatalf("cached body mismatch: %q vs %q", b1, b2)
	}
	if age2 == "" {
		t.Fatalf("cache hit missing Age header")
	}

	// Past the TTL, the relay must refetch.
	timeNow = func() time.Time { return base.Add(61 * time.Second) }
	_, _ = do()
	if hits.Load() != 2 {
		t.Fatalf("after TTL expiry backend hits = %d, want 2", hits.Load())
	}
}

// TestPublicReadNoStoreNotCached asserts the relay honors no-store — it must
// refetch every time rather than serve a cached copy.
func TestPublicReadNoStoreNotCached(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Get(relay.URL + "/api/v1/paymaster/supported-tokens?chainId=1")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if hits.Load() != 2 {
		t.Fatalf("no-store: backend hits = %d, want 2 (must not cache)", hits.Load())
	}
}

// TestCacheTTL covers Cache-Control parsing: cache only for a positive max-age,
// and never when no-store/no-cache/private is present.
func TestCacheTTL(t *testing.T) {
	cases := []struct {
		cc   string
		want time.Duration
		ok   bool
	}{
		{"public, max-age=60", 60 * time.Second, true},
		{"max-age=5", 5 * time.Second, true},
		{"", 0, false},
		{"no-store", 0, false},
		{"public, no-cache", 0, false},
		{"private, max-age=60", 0, false},
		{"max-age=0", 0, false},
		{"immutable", 0, false},
		// Shared cache prefers s-maxage over max-age (the paymaster's format).
		{"public, s-maxage=96, max-age=96", 96 * time.Second, true},
		{"public, s-maxage=30, max-age=10", 30 * time.Second, true},
		{"s-maxage=0, max-age=60", 0, false},
	}
	for _, c := range cases {
		got, ok := cacheTTL(c.cc)
		if ok != c.ok || got != c.want {
			t.Errorf("cacheTTL(%q) = (%v,%v), want (%v,%v)", c.cc, got, ok, c.want, c.ok)
		}
	}
}

// TestHealth asserts the relay's own health check does not touch the gateway.
func TestHealth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("gateway must not be reached for /health")
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	resp, err := http.Get(relay.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("health body = %q, want ok", body)
	}
}

// TestBrowserCORS verifies that browser apps on any origin can read relay
// responses and preflight the OHTTP POST without credentials.
func TestBrowserCORS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/ohttp-keys-signed")
		_, _ = w.Write([]byte("config"))
	}))
	defer backend.Close()

	relay := httptest.NewServer(New(backend.URL, http.DefaultClient))
	defer relay.Close()

	getReq, err := http.NewRequest(http.MethodGet, relay.URL+"/ohttp-configs", nil)
	if err != nil {
		t.Fatal(err)
	}
	getReq.Header.Set("Origin", "http://localhost:4455")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if origin := getResp.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("GET allow-origin = %q, want *", origin)
	}

	preflight, err := http.NewRequest(http.MethodOptions, relay.URL+"/gateway", nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Origin", "http://localhost:4455")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "content-type")
	preflightResp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	defer preflightResp.Body.Close()
	if preflightResp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", preflightResp.StatusCode)
	}
	if methods := preflightResp.Header.Get("Access-Control-Allow-Methods"); methods != "GET, POST, OPTIONS" {
		t.Fatalf("preflight allow-methods = %q", methods)
	}
	if headers := preflightResp.Header.Get("Access-Control-Allow-Headers"); headers != "Content-Type" {
		t.Fatalf("preflight allow-headers = %q", headers)
	}
}
