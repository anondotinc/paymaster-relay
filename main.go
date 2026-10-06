// Command anon-ohttp-relay is a keyless Oblivious HTTP relay. It forwards
// encapsulated OHTTP requests to a fixed gateway (the Anon paymaster with OHTTP
// enabled) and returns the encapsulated response. It holds no keys and sees no
// plaintext, so any operator independent of the paymaster can run it — that
// independence is what makes the IP↔content unlinkability hold.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	var (
		listen       = envOr("RELAY_LISTEN", ":8080")
		gatewayURL   = os.Getenv("OHTTP_GATEWAY_URL")
		schedulerURL = os.Getenv("OHTTP_SCHEDULER_GATEWAY_URL")
		ppoiURL      = os.Getenv("PPOI_TARGET")
		handler      http.Handler
		err          error
	)
	if gatewayURL == "" {
		log.Fatal("OHTTP_GATEWAY_URL is required")
	}
	handler, err = NewWithTargets(gatewayURL, schedulerURL, ppoiURL, &http.Client{Timeout: sealedTimeout})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("anon-ohttp-relay listening on %s (scheduler enabled: %t, ppoi enabled: %t)", listen, schedulerURL != "", ppoiURL != "")
	// Explicit timeouts: this relay is public and keyless, so it must not let a
	// slow/idle client tie up a connection indefinitely (Slowloris). WriteTimeout
	// comfortably exceeds the 40s sealed upstream client timeout so a legitimately slow
	// gateway response still makes it back.
	var srv = &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	err = srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}

func envOr(k, def string) string {
	var v string
	v = os.Getenv(k)
	if v != "" {
		return v
	}
	return def
}
