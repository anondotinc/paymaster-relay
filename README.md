# Anon OHTTP Relay

A **keyless** Oblivious HTTP (RFC 9458) relay for the Anon paymaster. It sits in
front of the paymaster (which holds the HPKE key and terminates OHTTP) and
forwards encapsulated requests to it:

```
Client ──HPKE-seal(to paymaster key)──▶  Relay (this)  ──▶  Paymaster (OHTTP gateway + target)
        message/ohttp-req                sees client IP,      decrypts; sees the
                                          NOT the content      content + relay IP,
                                                               NOT the client IP
```

This relay holds **no paymaster keys** and sees **no request plaintext**. The Go
implementation forwards the encapsulated body and minimal server-owned protocol headers,
never client headers or other PII. The Cloudflare Worker has an additional
client-IP caveat described below. Either implementation can be operated without
access to paymaster secrets.

> **The one non-negotiable rule (RFC 9458 §6):** the relay and the paymaster
> **MUST be operated by different, non-colluding parties.** If Anon runs both,
> the split buys nothing (either party — or a joint subpoena/compromise — can
> re-link IP ↔ content). Deploy this relay under an operator that is genuinely
> independent of the party running the paymaster.

## Endpoints

- `GET /health` → `200 ok` (relay/LB health check; not forwarded).
- `GET /ohttp-configs` → proxied from the paymaster (the signed HPKE keyconfig).
  Proxy the keyconfig fetch through the relay too, so the client never connects
  to the paymaster directly (a direct fetch would leak the client IP).
- `POST /gateway` (`Content-Type: message/ohttp-req`) → the paymaster's
  `message/ohttp-res`, relayed back unchanged.
- `GET /api/v1/paymaster/gas-quote` and `GET /api/v1/paymaster/supported-tokens`
  → proxied with validated, canonical queries from the paymaster. These are **public, identity-free,
  cacheable** reads — proxying them through the relay lets the client fetch its fee
  quote without exposing its IP to the paymaster, so the *whole* flow (quote →
  execute → status) goes through the relay. The relay caches them itself, honoring
  the paymaster's remaining TTL (see **Caching**); safe cache metadata lets a CDN
  front them too. (No sealing — they carry no secrets.)

  These two are the paymaster service's only public GET reads. The set lives in
  `publicReadPaths` (`relay.go`) / the Worker route policy. A new read requires
  reviewing both its path and allowed query fields/values. The allowlist is
  explicit on purpose: the relay is never an open proxy, and the JWT-scoped
  `GET /status/{job_id}` is deliberately not exposed (its oblivious counterpart is
  the sealed `POST /gateway` path).
- `GET /api/v1/paymaster/gas-tiers?chainId=<id>`, `GET /api/v1/paymaster/gas-history?chainId=<id>`
  and `GET /api/v1/tx/gas-fee/<id>` → the paymaster's gas tiers and history and
  the API's network base fee. Both are public,
  identity-free and **fresh-only** (the API answers
  `Cache-Control: public, s-maxage=5, max-age=5`). They are strictly validated
  before anything is forwarded: `<id>` must be exactly `1`, `56`, `137` or
  `42161`; `gas-tiers` and `gas-history` take exactly one `chainId` (or its protobuf spelling
  `chain_id`) and no other field; `gas-fee` takes no query and one path segment.
  Anything else is a `400` (`404` for other paths), never cached. The relay
  forwards only the canonical target (`?chainId=<id>` / `/<id>`), so no caller
  query reaches the API or the cache key. API errors (`400` for a chain the
  paymaster doesn't serve, `503` with no base fee) pass through as `no-store`.
  The rules live in `gas_reads.go` / `gasReadTarget` (`worker/src/index.ts`);
  change both together.
  They are not behind the per-source sealed-route limits (identity-free, cached
  for seconds) and keep their own fresh-only cache path.
- `GET /ppoi/ohttp-configs` and `POST /ppoi/gateway` are available **only when
  `PPOI_TARGET` is set** (404 otherwise). They forward to
  `${PPOI_TARGET}/ohttp-configs` and `${PPOI_TARGET}/gateway` (production:
  `https://proxy.anon.inc/ohttp`) with the same keyless, no-log handling
  as the paymaster's sealed route (the Worker variant applies smaller PPOI body
  caps; see `worker/README.md`), and cacheable key configs. The target must
  not share a base with the paymaster or scheduler; the PPOI gateway holds its
  own keys, so clients pin a separate PPOI signing key.
- `GET /scheduler/ohttp-configs` and `POST /scheduler/gateway` are available
  **only when a separate scheduler target is configured**. They append
  `/ohttp-configs` and `/gateway` to that target; they never fall back to the
  paymaster. The relay holds neither service's keys. Clients must independently
  pin the scheduler signing key, not reuse the paymaster pin.

Only exact paths and declared methods are accepted, including preflights.
Public reads require `chainId` (or the protobuf alias `chain_id`) equal to
`1`, `56`, `137`, or `42161`. Gas quotes also accept optional `priority`:
`slow`, `normal`, or `fast` (case-insensitive; empty means the default).
Duplicates, both chain spellings together, other fields, and query parameters
on gateway/config/health paths are rejected before forwarding.

The Go relay reconstructs headers, disables cookie jars and redirects, and
requests identity encoding. It accepts only HTTP 200 with the expected MIME
type and rejects truncated, oversized, or unexpectedly compressed responses.
Gateway requests are limited to 1 MiB for paymaster and 4 MiB for scheduler;
all responses remain limited to 1 MiB. GET and POST upstream work shares the
bounded concurrency and per-route source-rate buckets (paymaster 60/min,
scheduler 60/min, PPOI 180/min per hashed source, each independent). Sealed
routes have a 40-second deadline (above the backend gateway ladder: handler
25 s < forwarder 28 s < gateway 30-35 s < relay 40 s); public reads and key
configs keep 20 seconds. Gateway
responses and errors are `no-store`; no plaintext diagnostic body is forwarded.

## Caching

The public GET reads (`gas-quote`, `supported-tokens`, `gas-tiers`, `gas-history`, `gas-fee`)
and the KeyConfig (`/ohttp-configs`) are cached by the relay itself, **honoring
the paymaster's `Cache-Control`**:

- A `200` response with `Cache-Control: max-age=N` is cached for `N` seconds and
  served from cache within that window (with an `Age` header). The relay never
  serves a response staler than the paymaster's own `max-age` — so a cached quote
  can't outlive the window the paymaster is willing to honor at execute time.
- `no-store` / `no-cache` / `private`, or a missing / zero `max-age`, disable
  caching for that response — the relay refetches every time.
- Errors are never cached by the relay (only `200`s are stored).
- The Go relay (every read) and the Worker's gas reads also count an upstream
  `Age` against that lifetime, so neither the relay nor a cache behind it
  extends the freshness (an invalid or exhausted `Age` makes the response
  `no-store`), and relay non-`200` answers with `Cache-Control: no-store`.
- The cache is **shared across clients** (the responses are identity-free), which
  also reduces the request volume the paymaster sees.
- Cache keys include the fixed target, separating scheduler and paymaster key
  configurations. Upstream `Age` reduces the remaining lifetime; malformed or
  exhausted ages and `Vary` disable caching. Only rebuilt cache metadata and the
  expected content type are returned, not cookies, redirects, or trace headers.

The **Go** relay uses a small in-memory TTL cache (`cache.go`); the **Worker**
uses the Cloudflare edge cache (`caches.default`), which honors origin
`Cache-Control` automatically. For the gas reads the Worker rebuilds the
response headers and stores `public, max-age=<remaining>` (s-maxage or max-age
minus `Age`), keyed by the canonical target. To set the TTL, set
`Cache-Control: max-age=…` on the paymaster's `gas-quote` / `supported-tokens`
responses (≤ its quote validity window).

Both implementations allow public browser access with `Access-Control-Allow-Origin: *`
and handle `OPTIONS` preflights for the OHTTP `Content-Type`. They do not use
cookies or browser credentials.

## Configuration

| Env | Meaning |
|---|---|
| `OHTTP_GATEWAY_URL` | **required** — base URL of the paymaster's OHTTP gateway (e.g. `https://paymaster.internal`) |
| `PPOI_TARGET` | Optional — distinct PPOI gateway base, e.g. `https://proxy.anon.inc/ohttp`. Unset disables `/ppoi/*` (404). |
| `OHTTP_SCHEDULER_GATEWAY_URL` | Optional — distinct scheduler gateway base, e.g. `http://backend:8080/scheduler`. Unset disables scheduler routes. |
| `RELAY_LISTEN` | bind address (default `:8080`) |

Gateway bases are operator configuration, never request parameters. Credentials,
query strings, fragments, and encoded/traversing paths are rejected. Use HTTPS
for external gateways; plain HTTP is supported only for an operator-managed
trusted internal hop. The caller remains responsible for deployment TLS and
network isolation. `New(gatewayURL, client)` remains paymaster-only;
`NewWithScheduler(gatewayURL, schedulerURL, client)` returns configuration errors.

Scheduler activation requires a separately provisioned gateway, signing pin,
and ingress route. The public API's existing root `/ohttp-configs` and `/gateway`
point to the paymaster; they are **not** valid scheduler targets. Merely adding
these relay routes does not activate scheduled payments or change deployment
configuration. The proposed external namespace needs its own deployment gate.

## Paymaster discovery

The production relay is [`https://relay.anon.inc`](https://relay.anon.inc).

Anon publishes the production relay configuration and Paymaster signing key at
[`https://anon.inc/.well-known/anon-paymaster.json`](https://anon.inc/.well-known/anon-paymaster.json).

The production Ed25519 public key used to verify `/ohttp-configs` is:

```text
83771473963247d1924fa9af8d3ef9c745e1be1fca0f6907ae5ea8b42954adda
```

Applications should pin this key in their build. The well-known document is a
discovery mechanism, not a substitute for an independently pinned trust anchor.

## Build & run (Docker)

```bash
docker build -t anon-ohttp-relay .
docker run --rm -p 8080:8080 \
  -e OHTTP_GATEWAY_URL="https://<your-paymaster-gateway>" \
  anon-ohttp-relay
```

## Build & test (local)

```bash
go build .
go test ./...
node --experimental-strip-types --test worker/test/*.test.mjs   # Worker, Node 22+
```

## Cloudflare Worker variant

For a serverless deployment, see [`worker/`](worker/) — the same keyless relay
as a Cloudflare Worker.

From this repository, deploy the production Worker and verify its browser CORS
preflight with:

```bash
make deploy
```

The target runs `go test ./...` and the Worker tests, performs a Wrangler dry
run, deploys the custom domain from `worker/wrangler.toml`, and checks the live
`/health` and `OPTIONS /gateway` responses. Run `make cloudflare-login` first if
Wrangler is not authenticated.

The checked-in Worker configuration deploys Anon's production relay. To deploy
from another Cloudflare account, fork this repository and change all three of:

1. `name` in `worker/wrangler.toml` to a unique Worker name.
2. `routes[].pattern` to a custom domain in a Cloudflare zone you control.
3. `TARGET` to the intended paymaster's HTTPS OHTTP gateway.

Then deploy and verify your domain:

```bash
make cloudflare-login
make deploy RELAY_URL=https://relay.example.com
```

Cloudflare creates the custom-domain DNS record and certificate during deploy.
The target is fixed by configuration, so this relay cannot be used as an open
proxy.

> ⚠️ **IP-privacy caveat (Worker only):** Cloudflare injects the real client IP
> into `CF-Connecting-IP` on Worker subrequests to a non-Cloudflare origin, and
> it can't be suppressed at the Worker. **This Go relay does not have that
> problem** (it forwards no client headers), so it is the reference for the
> strongest guarantee. If you deploy the Worker, the paymaster ingress must strip
> `CF-Connecting-IP`/`X-Forwarded-For`/`X-Real-IP`/`True-Client-IP`. See
> [`worker/README.md`](worker/README.md).

Local synthetic tests verify the Go application boundary, not a deployed CDN or
ingress. Before claiming deployed IP separation, inspect receiver headers with
synthetic non-account probes before access-log retention. Keep wallet payloads,
capabilities, addresses, and real client identifiers out of diagnostic captures.

## See also

- [Relay architecture, operator choices, and security guarantees](https://docs.anon.inc/advanced/paymaster-relay)
- [Oblivious HTTP (RFC 9458)](https://www.rfc-editor.org/rfc/rfc9458.html)
- [HPKE (RFC 9180)](https://www.rfc-editor.org/rfc/rfc9180.html)
