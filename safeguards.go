package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	maxConcurrentGatewayRequests = 128
	// maxSourceRequestsPerMinute is the paymaster and scheduler per-source budget.
	maxSourceRequestsPerMinute = 60
	// maxPPOIRequestsPerMinute is the PPOI budget: POI sync keeps 3 concurrent
	// 50-commitment batches in flight and must not starve (or be starved by)
	// paymaster status polling, so it has its own, larger bucket.
	maxPPOIRequestsPerMinute = 180
	// maxGasReadsPerMinute is the public gas-read budget. The SDK refreshes each
	// chain's reads every 10-30 s (under 100/min for a wallet on all four
	// chains), so several wallets behind one address fit, while a single source
	// is held to 4 reads/s. Cache hits count too.
	maxGasReadsPerMinute = 240
)

// Named per-source rate-limit buckets. Each route belongs to exactly one, so
// traffic on one purpose never consumes another purpose's budget.
const (
	bucketPaymaster = "paymaster"
	bucketScheduler = "scheduler"
	bucketPPOI      = "ppoi"
	bucketGas       = "gas"
)

var bucketLimits = map[string]int{
	bucketPaymaster: maxSourceRequestsPerMinute,
	bucketScheduler: maxSourceRequestsPerMinute,
	bucketPPOI:      maxPPOIRequestsPerMinute,
	bucketGas:       maxGasReadsPerMinute,
}

// gatewayRequestTimeout is the outer deadline of a relayed request. It is the
// sealed-route deadline (sealedTimeout): it nests above the backend gateway
// ladder (handler 25 s < forwarder 28 s < gateway 30-35 s < relay 40 s).
var gatewayRequestTimeout = sealedTimeout

type sourceWindow struct {
	started time.Time
	count   int
}

// relaySafeguards is process-local by design. Source identifiers are salted,
// hashed, never logged, and disappear on restart; there is no durable identity
// or cross-request telemetry trail.
type relaySafeguards struct {
	mu      sync.Mutex
	salt    [32]byte
	windows map[[32]byte]sourceWindow
	active  chan struct{}
}

func newRelaySafeguards() *relaySafeguards {
	var salt [32]byte
	_, _ = rand.Read(salt[:])
	return &relaySafeguards{
		salt:    salt,
		windows: make(map[[32]byte]sourceWindow),
		active:  make(chan struct{}, maxConcurrentGatewayRequests),
	}
}

// sourceKey hashes the salted source with the bucket name: same hashing for
// every bucket, but a source has an independent window in each.
func (s *relaySafeguards) sourceKey(bucket, remoteAddr string) [32]byte {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	material := append(append([]byte(nil), s.salt[:]...), []byte(bucket)...)
	material = append(append(material, 0), []byte(host)...)
	return sha256.Sum256(material)
}

// allow charges the paymaster bucket.
func (s *relaySafeguards) allow(remoteAddr string, now time.Time) bool {
	return s.allowBucket(bucketPaymaster, remoteAddr, now)
}

func (s *relaySafeguards) allowBucket(bucket, remoteAddr string, now time.Time) bool {
	limit, known := bucketLimits[bucket]
	if !known {
		return false
	}
	key := s.sourceKey(bucket, remoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.windows) > 10000 {
		for candidate, window := range s.windows {
			if now.Sub(window.started) >= time.Minute {
				delete(s.windows, candidate)
			}
		}
		if len(s.windows) > 10000 {
			for candidate := range s.windows {
				delete(s.windows, candidate)
				break
			}
		}
	}
	window := s.windows[key]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		s.windows[key] = sourceWindow{started: now, count: 1}
		return true
	}
	if window.count >= limit {
		return false
	}
	window.count++
	s.windows[key] = window
	return true
}

func (s *relaySafeguards) gateway(next http.Handler) http.Handler {
	return s.gatewayWithLimit(bucketPaymaster, maxBody, next)
}

func (s *relaySafeguards) gatewayWithLimit(bucket string, requestLimit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > requestLimit {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !s.allowBucket(bucket, r.RemoteAddr, time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		select {
		case s.active <- struct{}{}:
			defer func() { <-s.active }()
		default:
			http.Error(w, "relay busy", http.StatusServiceUnavailable)
			return
		}
		var ctx, cancel = context.WithTimeout(r.Context(), gatewayRequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
