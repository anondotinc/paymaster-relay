package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func ppoiHandler(t *testing.T, targets *[]string) http.Handler {
	t.Helper()
	var client = &http.Client{Transport: privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		*targets = append(*targets, request.Method+" "+request.URL.String())
		if request.URL.Path == "/ohttp/ohttp-configs" {
			var response = privacyResponse(ctConfigs, "ppoi-config")
			response.Header.Set("Cache-Control", "public, max-age=60")
			return response, nil
		}
		return privacyResponse(ctRes, "sealed"), nil
	})}
	var handler, err = NewWithTargets("https://paymaster.test", "https://scheduler.test/scheduler", "https://proxy.test/ohttp", client)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestPPOIRoutesForwardToPPOITarget(t *testing.T) {
	var targets []string
	var handler = ppoiHandler(t, &targets)
	var response = privacyRequest(handler, http.MethodGet, "/ppoi/ohttp-configs", nil)
	if response.Code != http.StatusOK || response.Body.String() != "ppoi-config" || response.Header().Get("Content-Type") != ctConfigs {
		t.Fatalf("configs: %d %q %v", response.Code, response.Body.String(), response.Header())
	}
	// Key config is cacheable like the paymaster's: the second read is a cache hit.
	if response.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("config not cacheable: %v", response.Header())
	}
	privacyRequest(handler, http.MethodGet, "/ppoi/ohttp-configs", nil)
	response = privacyRequest(handler, http.MethodPost, "/ppoi/gateway", []byte("sealed"))
	if response.Code != http.StatusOK || response.Body.String() != "sealed" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != ctRes {
		t.Fatalf("gateway: %d %v", response.Code, response.Header())
	}
	var want = []string{"GET https://proxy.test/ohttp/ohttp-configs", "POST https://proxy.test/ohttp/gateway"}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Fatalf("targets %v, want %v", targets, want)
	}
	// Wrong methods and unknown ppoi paths are refused without dispatch.
	for _, testCase := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/ppoi/ohttp-configs", http.StatusMethodNotAllowed},
		{http.MethodGet, "/ppoi/gateway", http.StatusMethodNotAllowed},
		{http.MethodGet, "/ppoi/other", http.StatusNotFound},
		{http.MethodGet, "/ppoi/ohttp-configs?x=1", http.StatusBadRequest},
	} {
		if got := privacyRequest(handler, testCase.method, testCase.path, []byte("x")).Code; got != testCase.status {
			t.Errorf("%s %s: %d, want %d", testCase.method, testCase.path, got, testCase.status)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("refused requests were dispatched: %v", targets)
	}
}

func TestPPOIDisabledWhenUnset(t *testing.T) {
	var dispatched int
	var client = &http.Client{Transport: privacyRoundTripper(func(*http.Request) (*http.Response, error) {
		dispatched++
		return privacyResponse(ctRes, "sealed"), nil
	})}
	for name, handler := range map[string]http.Handler{
		"New": New("https://paymaster.test", client),
		"NewWithTargets": func() http.Handler {
			var handler, err = NewWithTargets("https://paymaster.test", "https://scheduler.test", "", client)
			if err != nil {
				t.Fatal(err)
			}
			return handler
		}(),
	} {
		for _, path := range []string{"/ppoi/ohttp-configs", "/ppoi/gateway"} {
			if code := privacyRequest(handler, http.MethodPost, path, []byte("x")).Code; code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", name, path, code)
			}
			if code := privacyRequest(handler, http.MethodGet, path, nil).Code; code != http.StatusNotFound {
				t.Errorf("%s GET %s: %d, want 404", name, path, code)
			}
		}
	}
	if dispatched != 0 {
		t.Fatalf("disabled route dispatched %d requests", dispatched)
	}
}

func TestPPOIRefusesSharedBase(t *testing.T) {
	for _, testCase := range []struct{ paymaster, scheduler, ppoi string }{
		{"https://paymaster.test", "", "https://paymaster.test"},
		{"https://paymaster.test/", "", "https://paymaster.test"},
		{"https://paymaster.test", "https://scheduler.test", "https://scheduler.test/"},
		{"https://paymaster.test", "", "ftp://proxy.test"},
		{"https://paymaster.test", "", "https://proxy.test?x=1"},
	} {
		if _, err := NewWithTargets(testCase.paymaster, testCase.scheduler, testCase.ppoi, nil); err == nil {
			t.Errorf("accepted %+v", testCase)
		}
	}
	// New fails closed: an invalid configuration answers 503, never routes.
	if _, err := NewWithTargets("https://paymaster.test", "", "https://proxy.test", nil); err != nil {
		t.Fatal(err)
	}
}

// TestPerRouteBuckets asserts each route family has its own per-source budget:
// paymaster 60/min, scheduler 60/min, ppoi 180/min.
func TestPerRouteBuckets(t *testing.T) {
	if bucketLimits[bucketPaymaster] != 60 || bucketLimits[bucketScheduler] != 60 || bucketLimits[bucketPPOI] != 180 {
		t.Fatalf("bucket limits %v", bucketLimits)
	}
	var targets []string
	var handler = ppoiHandler(t, &targets)
	var post = func(path string) int { return privacyRequest(handler, http.MethodPost, path, []byte("sealed")).Code }

	// Exhaust the paymaster bucket: ppoi and scheduler stay open.
	for i := 0; i < 60; i++ {
		if code := post("/gateway"); code != http.StatusOK {
			t.Fatalf("paymaster request %d: %d", i+1, code)
		}
	}
	if code := post("/gateway"); code != http.StatusTooManyRequests {
		t.Fatalf("paymaster 61st: %d, want 429", code)
	}
	if code := post("/ppoi/gateway"); code != http.StatusOK {
		t.Fatalf("ppoi consumed by paymaster budget: %d", code)
	}
	if code := post("/scheduler/gateway"); code != http.StatusOK {
		t.Fatalf("scheduler consumed by paymaster budget: %d", code)
	}

	// Exhaust ppoi (1 already used): 179 more fit, the next is refused, and the
	// paymaster bucket is unaffected by ppoi traffic (checked on a fresh handler).
	for i := 1; i < 180; i++ {
		if code := post("/ppoi/gateway"); code != http.StatusOK {
			t.Fatalf("ppoi request %d: %d", i+1, code)
		}
	}
	if code := post("/ppoi/gateway"); code != http.StatusTooManyRequests {
		t.Fatalf("ppoi 181st: %d, want 429", code)
	}
	var fresh = ppoiHandler(t, &targets)
	for i := 0; i < 180; i++ {
		privacyRequest(fresh, http.MethodPost, "/ppoi/gateway", []byte("sealed"))
	}
	if code := privacyRequest(fresh, http.MethodPost, "/gateway", []byte("sealed")).Code; code != http.StatusOK {
		t.Fatalf("ppoi traffic consumed the paymaster budget: %d", code)
	}
	// A different source has its own window in every bucket.
	var other = httptest.NewRequest(http.MethodPost, "/ppoi/gateway", nil)
	other.RemoteAddr = "192.0.2.77:1"
	other.Header.Set("Content-Type", ctReq)
	other.Body = http.NoBody
	var recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, other)
	if recorder.Code == http.StatusTooManyRequests {
		t.Fatal("rate limit is not source-scoped")
	}
}

func TestSealedDeadlineIs40Seconds(t *testing.T) {
	if sealedTimeout != 40*time.Second || gatewayRequestTimeout != 40*time.Second {
		t.Fatalf("sealed %v, gateway %v", sealedTimeout, gatewayRequestTimeout)
	}
	if upstreamTimeout != 20*time.Second {
		t.Fatalf("public reads deadline %v", upstreamTimeout)
	}
	var deadlines = map[string]time.Duration{}
	var client = &http.Client{Transport: privacyRoundTripper(func(request *http.Request) (*http.Response, error) {
		var deadline, ok = request.Context().Deadline()
		if !ok {
			t.Errorf("%s: no deadline", request.URL.Path)
		}
		deadlines[request.Method+" "+request.URL.Path] = time.Until(deadline)
		if request.Method == http.MethodGet {
			return privacyResponse(ctConfigs, "config"), nil
		}
		return privacyResponse(ctRes, "sealed"), nil
	})}
	var handler, err = NewWithTargets("https://paymaster.test", "", "https://proxy.test/ohttp", client)
	if err != nil {
		t.Fatal(err)
	}
	for relayPath, upstreamPath := range map[string]string{"/gateway": "/gateway", "/ppoi/gateway": "/ohttp/gateway"} {
		privacyRequest(handler, http.MethodPost, relayPath, []byte("sealed"))
		var got = deadlines["POST "+upstreamPath]
		if got <= 30*time.Second || got > 40*time.Second {
			t.Errorf("%s deadline %v, want within (30s, 40s]", relayPath, got)
		}
	}
	privacyRequest(handler, http.MethodGet, "/ppoi/ohttp-configs", nil)
	if got := deadlines["GET /ohttp/ohttp-configs"]; got <= 10*time.Second || got > 20*time.Second {
		t.Errorf("config read deadline %v, want within (10s, 20s]", got)
	}
}
