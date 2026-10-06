package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type privacyRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip privacyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func privacyResponse(contentType, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func privacyRequest(handler http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	var request = httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Header.Set("Content-Type", ctReq)
	var response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestPrivacyPurposeRoutingAndNoActivation(t *testing.T) {
	var targets []string
	var client = &http.Client{Transport: privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		targets = append(targets, request.URL.String())
		if strings.HasSuffix(request.URL.Path, "/ohttp-configs") {
			var response = privacyResponse(ctConfigs, request.URL.Host)
			response.Header.Set("Cache-Control", "public, max-age=60")
			return response, nil
		}
		return privacyResponse(ctRes, "sealed"), nil
	})}
	var handler, err = NewWithScheduler("https://paymaster.test", "https://scheduler.test/scheduler", client)
	if err != nil {
		t.Fatal(err)
	}
	var cases = []struct{ path, method, target string }{
		{"/ohttp-configs", http.MethodGet, "https://paymaster.test/ohttp-configs"},
		{"/scheduler/ohttp-configs", http.MethodGet, "https://scheduler.test/scheduler/ohttp-configs"},
		{"/gateway", http.MethodPost, "https://paymaster.test/gateway"},
		{"/scheduler/gateway", http.MethodPost, "https://scheduler.test/scheduler/gateway"},
	}
	for _, testCase := range cases {
		var response = privacyRequest(handler, testCase.method, testCase.path, []byte("sealed"))
		if response.Code != http.StatusOK || targets[len(targets)-1] != testCase.target {
			t.Fatalf("%s: status %d, targets %v", testCase.path, response.Code, targets)
		}
	}
	if privacyRequest(handler, http.MethodGet, "/ohttp-configs", nil).Body.String() != "paymaster.test" {
		t.Fatal("wrong cached paymaster config")
	}
	if privacyRequest(handler, http.MethodGet, "/scheduler/ohttp-configs", nil).Body.String() != "scheduler.test" {
		t.Fatal("wrong cached scheduler config")
	}
	var disabled = New("https://paymaster.test", client)
	if privacyRequest(disabled, http.MethodPost, "/scheduler/gateway", nil).Code != http.StatusNotFound {
		t.Fatal("scheduler activated without target")
	}
	if len(targets) != 4 {
		t.Fatalf("unexpected upstream dispatch count %d", len(targets))
	}
}

func TestPrivacyGatewayConfiguration(t *testing.T) {
	var invalid = []string{"", "//host.test", "ftp://host.test", "https://user:secret@host.test", "https://host.test?token=x", "https://host.test?", "https://host.test#", "https://host.test/a/../b", "https://host.test/%2e%2e/path", "https://host.test/a//b", "https://host.test/a\\b"}
	for _, candidate := range invalid {
		var _, err = NewWithScheduler(candidate, "", nil)
		if err == nil {
			t.Errorf("accepted invalid gateway %q", candidate)
		}
	}
	var _, err = NewWithScheduler("https://host.test/", "https://host.test", nil)
	if err == nil {
		t.Fatal("accepted the same scheduler/paymaster gateway")
	}
	var normalized string
	normalized, err = validateGatewayURL("http://backend:8080/scheduler/")
	if err != nil || normalized != "http://backend:8080/scheduler" {
		t.Fatalf("internal fixed base: %q %v", normalized, err)
	}
}

func TestPrivacyRejectsUnknownOrIdentifyingRequests(t *testing.T) {
	var client = &http.Client{Transport: privacyRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("invalid request reached upstream")
		return nil, errors.New("must not dispatch")
	})}
	var handler = New("https://paymaster.test", client)
	var cases = []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1&account=secret", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1&chain_id=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1&chainId=1", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1&priority=secret", 400},
		{http.MethodGet, "/api/v1/paymaster/supported-tokens?chainId=1&priority=fast", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=999", 400},
		{http.MethodGet, "/api/v1/paymaster/gas-quote", 400},
		{http.MethodGet, "/ohttp-configs?account=secret", 400},
		{http.MethodPost, "/gateway?target=https://attacker.test", 400},
		{http.MethodGet, "/health?account=secret", 400},
		{http.MethodGet, "/%6fhttp-configs", 404},
		{http.MethodGet, "/a/../ohttp-configs", 404},
		{http.MethodPost, "/gateway/", 404},
		{http.MethodOptions, "/not-allowlisted", 404},
		{http.MethodHead, "/ohttp-configs", 405},
		{http.MethodGet, "/gateway", 405},
	}
	for _, testCase := range cases {
		var response = privacyRequest(handler, testCase.method, testCase.path, nil)
		if response.Code != testCase.status {
			t.Errorf("%s %s: got %d want %d", testCase.method, testCase.path, response.Code, testCase.status)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Error("failure must not be cached")
		}
	}
}

func TestPrivacyPublicQueryCanonicalization(t *testing.T) {
	var cases = []struct{ raw, want string }{
		{"chain_id=42161&priority=FAST", "chainId=42161&priority=fast"},
		{"priority=&chainId=1", "chainId=1"},
		{"chainId=137", "chainId=137"},
	}
	for _, testCase := range cases {
		var result, err = canonicalPublicQuery(publicReadPaths[0], testCase.raw)
		if err != nil || result != testCase.want {
			t.Errorf("%q: %q %v", testCase.raw, result, err)
		}
	}
}

func TestPrivacyStripsHeadersCookiesAndResponseTracking(t *testing.T) {
	var jar, _ = cookiejar.New(nil)
	var base, _ = url.Parse("https://paymaster.test")
	jar.SetCookies(base, []*http.Cookie{{Name: "operator-cookie", Value: "secret"}})
	var calls int
	var client = &http.Client{Jar: jar, Transport: privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		assertNoClientIdentityHeaders(t, request.Header)
		for _, name := range []string{"Cookie", "Traceparent", "Baggage", "X-Request-ID"} {
			if request.Header.Get(name) != "" {
				t.Errorf("identity header %s was forwarded", name)
			}
		}
		if request.Header.Get("Accept-Encoding") != "identity" {
			t.Error("response compression not disabled")
		}
		var response = privacyResponse(ctRes, "sealed")
		response.Header.Set("Set-Cookie", "upstream-cookie=secret")
		response.Header.Set("Location", "https://attacker.test")
		response.Header.Set("X-Request-ID", "trace-secret")
		response.Header.Set("Cache-Control", "public, max-age=600")
		return response, nil
	})}
	var handler = New("https://paymaster.test", client)
	var request = httptest.NewRequest(http.MethodPost, "/gateway", strings.NewReader("sealed"))
	addClientIdentityHeaders(request)
	request.Header.Set("Content-Type", ctReq)
	for _, name := range []string{"Cookie", "Traceparent", "Baggage", "X-Request-ID"} {
		request.Header.Set(name, "secret")
	}
	var response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bad sealed response %d %v", response.Code, response.Header())
	}
	for _, name := range []string{"Set-Cookie", "Location", "X-Request-ID"} {
		if response.Header().Get(name) != "" {
			t.Errorf("upstream tracking response header %s returned", name)
		}
	}
	if client.Jar == nil || len(jar.Cookies(base)) != 1 {
		t.Fatal("caller client mutated")
	}
	if privacyRequest(handler, http.MethodPost, "/gateway", []byte("sealed")).Code != 200 || calls != 2 {
		t.Fatal("sealed response was cached")
	}
}

func TestPrivacySchedulerRequestLimits(t *testing.T) {
	var received int
	var client = &http.Client{Transport: privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		var body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		received = len(body)
		return privacyResponse(ctRes, "sealed"), nil
	})}
	var handler, err = NewWithScheduler("https://paymaster.test", "https://scheduler.test", client)
	if err != nil {
		t.Fatal(err)
	}
	var response = privacyRequest(handler, http.MethodPost, "/scheduler/gateway", make([]byte, maxSchedulerBody))
	if response.Code != 200 || received != maxSchedulerBody {
		t.Fatalf("scheduler max request: %d, received %d", response.Code, received)
	}
	response = privacyRequest(handler, http.MethodPost, "/gateway", make([]byte, maxBody+1))
	if response.Code != 413 {
		t.Fatalf("paymaster limit changed: %d", response.Code)
	}
	var request = httptest.NewRequest(http.MethodPost, "/scheduler/gateway", bytes.NewReader(make([]byte, maxSchedulerBody+1)))
	request.ContentLength = -1
	request.Header.Set("Content-Type", ctReq)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 413 {
		t.Fatalf("chunked scheduler overflow accepted: %d", response.Code)
	}
}

func TestPrivacyRejectsBadSealedBodies(t *testing.T) {
	var client = &http.Client{Transport: privacyRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("invalid sealed body reached upstream")
		return nil, errors.New("must not dispatch")
	})}
	var handler = New("https://paymaster.test", client)
	var cases = []struct {
		encoding, body string
		length         int64
		status         int
	}{
		{"gzip", "compressed", 10, 415},
		{"", "", 0, 400},
		{"", "partial", 50, 400},
		{"", "too-long", 1, 400},
	}
	for _, testCase := range cases {
		var request = httptest.NewRequest(http.MethodPost, "/gateway", strings.NewReader(testCase.body))
		request.ContentLength = testCase.length
		request.Header.Set("Content-Type", ctReq)
		request.Header.Set("Content-Encoding", testCase.encoding)
		var response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != testCase.status {
			t.Errorf("bad body %q: %d", testCase.encoding, response.Code)
		}
	}
}

type truncatedPrivacyBody struct{}

func (truncatedPrivacyBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (truncatedPrivacyBody) Close() error             { return nil }

func TestPrivacyRejectsBadResponsesWithoutFollowingRedirects(t *testing.T) {
	var cases = []struct {
		name     string
		response func() *http.Response
	}{
		{"empty", func() *http.Response { return privacyResponse(ctConfigs, "") }},
		{"partial-status", func() *http.Response {
			var response = privacyResponse(ctConfigs, "partial")
			response.StatusCode = http.StatusPartialContent
			return response
		}},
		{"redirect", func() *http.Response {
			var response = privacyResponse(ctConfigs, "redirect")
			response.StatusCode = 307
			response.Header.Set("Location", "https://attacker.test")
			return response
		}},
		{"mime", func() *http.Response { return privacyResponse("text/html", "private upstream debug") }},
		{"compressed", func() *http.Response {
			var response = privacyResponse(ctConfigs, "compressed")
			response.Header.Set("Content-Encoding", "gzip")
			return response
		}},
		{"oversized", func() *http.Response {
			var response = privacyResponse(ctConfigs, strings.Repeat("x", maxBody+1))
			response.ContentLength = -1
			return response
		}},
		{"truncated", func() *http.Response {
			var response = privacyResponse(ctConfigs, "partial")
			response.ContentLength = 100
			return response
		}},
		{"read-error", func() *http.Response {
			var response = privacyResponse(ctConfigs, "")
			response.Body = truncatedPrivacyBody{}
			return response
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls int
			var client = &http.Client{Transport: privacyRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return testCase.response(), nil })}
			var response = privacyRequest(New("https://paymaster.test", client), http.MethodGet, "/ohttp-configs", nil)
			if response.Code != 502 || response.Body.String() != "bad gateway\n" || calls != 1 {
				t.Fatalf("bad response relayed or retried: %d %q calls=%d", response.Code, response.Body.String(), calls)
			}
		})
	}
}

func TestPrivacyCacheDoesNotResetUpstreamAge(t *testing.T) {
	var calls int
	var client = &http.Client{Transport: privacyRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		var response = privacyResponse("application/json", "{}")
		response.Header.Set("Cache-Control", "public, max-age=60")
		response.Header.Set("Age", "50")
		return response, nil
	})}
	var handler = New("https://paymaster.test", client)
	var response = privacyRequest(handler, http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1", nil)
	if response.Header().Get("Cache-Control") != "public, max-age=10" {
		t.Fatalf("freshness extended: %s", response.Header().Get("Cache-Control"))
	}
	response = privacyRequest(handler, http.MethodGet, "/api/v1/paymaster/gas-quote?chainId=1", nil)
	if calls != 1 || response.Header().Get("Cache-Control") != "public, max-age=10" {
		t.Fatal("remaining lifetime not cached correctly")
	}
}

func TestPrivacyUpstreamDeadlineAndCancellation(t *testing.T) {
	var client = relayHTTPClient(&http.Client{Timeout: time.Hour})
	if client.Timeout != upstreamTimeout || client.Jar != nil {
		t.Fatal("client protections missing")
	}
	var observed bool
	client.Transport = privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		var deadline, exists = request.Context().Deadline()
		if !exists || time.Until(deadline) > upstreamTimeout {
			t.Error("unbounded request")
		}
		observed = true
		return nil, request.Context().Err()
	})
	var request = httptest.NewRequest(http.MethodGet, "/ohttp-configs", nil)
	var ctx, cancel = context.WithCancel(request.Context())
	cancel()
	var response = httptest.NewRecorder()
	New("https://paymaster.test", client).ServeHTTP(response, request.WithContext(ctx))
	if response.Code != 502 || !observed {
		t.Fatalf("cancelled request: %d observed=%v", response.Code, observed)
	}
}
