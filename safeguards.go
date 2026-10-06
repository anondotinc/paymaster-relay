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
	maxSourceRequestsPerMinute   = 60
)

var gatewayRequestTimeout = 20 * time.Second

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

func (s *relaySafeguards) sourceKey(remoteAddr string) [32]byte {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	material := append(append([]byte(nil), s.salt[:]...), []byte(host)...)
	return sha256.Sum256(material)
}

func (s *relaySafeguards) allow(remoteAddr string, now time.Time) bool {
	key := s.sourceKey(remoteAddr)
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
	if window.count >= maxSourceRequestsPerMinute {
		return false
	}
	window.count++
	s.windows[key] = window
	return true
}

func (s *relaySafeguards) gateway(next http.Handler) http.Handler {
	return s.gatewayWithLimit(maxBody, next)
}

func (s *relaySafeguards) gatewayWithLimit(requestLimit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > requestLimit {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !s.allow(r.RemoteAddr, time.Now()) {
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
