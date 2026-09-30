SHELL := /bin/bash
.DEFAULT_GOAL := help

WORKER_DIR ?= worker
WRANGLER ?= npx --yes wrangler@4
RELAY_URL ?= https://relay.anon.inc

.PHONY: help test worker-test worker-check check cloudflare-login deploy deploy-worker deploy-cloudflare verify-cloudflare

help:
	@echo "Anon OHTTP relay"
	@echo ""
	@echo "  make check              Run Go and Worker tests and compile the Worker"
	@echo "  make deploy             Validate, deploy to Cloudflare, and verify CORS"
	@echo "  make verify-cloudflare  Verify the currently deployed relay"
	@echo "  make cloudflare-login   Authenticate Wrangler in a browser"

test:
	go test ./...

# Worker source tests: Node 22+, no Wrangler, network or credentials.
worker-test:
	node --experimental-strip-types --test $(WORKER_DIR)/test/*.test.mjs

worker-check:
	cd $(WORKER_DIR) && $(WRANGLER) deploy --dry-run

check: test worker-test worker-check

cloudflare-login:
	cd $(WORKER_DIR) && $(WRANGLER) login

# Friendly aliases: the relay repository has only one production deployment.
deploy deploy-worker: deploy-cloudflare

deploy-cloudflare: check
	cd $(WORKER_DIR) && $(WRANGLER) deploy
	@$(MAKE) --no-print-directory verify-cloudflare

verify-cloudflare:
	@echo "Verifying $(RELAY_URL)..."
	@health="$$(curl --retry 8 --retry-delay 2 --retry-all-errors -fsS "$(RELAY_URL)/health")"; \
		if [ "$$health" != "ok" ]; then \
			echo "Expected $(RELAY_URL)/health to return 'ok', got '$$health'" >&2; \
			exit 1; \
		fi
	@headers="$$(curl --retry 8 --retry-delay 2 --retry-all-errors -fsS -D - -o /dev/null \
		-X OPTIONS "$(RELAY_URL)/gateway" \
		-H "Origin: https://anon.buzz" \
		-H "Access-Control-Request-Method: POST" \
		-H "Access-Control-Request-Headers: Content-Type" | tr -d '\r')"; \
		echo "$$headers" | grep -qi '^access-control-allow-origin: \*$$' || { \
			echo "Relay preflight is missing Access-Control-Allow-Origin: *" >&2; \
			exit 1; \
		}; \
		echo "$$headers" | grep -qi '^access-control-allow-methods:.*POST' || { \
			echo "Relay preflight is missing POST in Access-Control-Allow-Methods" >&2; \
			exit 1; \
		}
	@echo "Relay health and browser CORS preflight passed."
