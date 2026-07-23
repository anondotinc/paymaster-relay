# Anon Paymaster Relay

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
implementation forwards only the encapsulated body and its `Content-Type`,
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
  → proxied (path + query) from the paymaster. These are **public, identity-free,
  cacheable** reads — proxying them through the relay lets the client fetch its fee
  quote without exposing its IP to the paymaster, so the *whole* flow (quote →
  execute → status) goes through the relay. The relay caches them itself, honoring
  the paymaster's TTL (see **Caching**); `Cache-Control` is preserved so a CDN can
  front them too. (No sealing — they carry no secrets.)

  These two are the paymaster service's only public GET reads. The set lives in
  `publicReadPaths` (`relay.go`) / `PUBLIC_READ_PATHS` (`worker/src/index.ts`) —
  add a new public paymaster read by appending one line to each. The allowlist is
  explicit on purpose: the relay is never an open proxy, and the JWT-scoped
  `GET /status/{job_id}` is deliberately not exposed (its oblivious counterpart is
  the sealed `POST /gateway` path).

## Caching

The public GET reads (`gas-quote`, `supported-tokens`) and the KeyConfig
(`/ohttp-configs`) are cached by the relay itself, **honoring the paymaster's
`Cache-Control`**:

- A `200` response with `Cache-Control: max-age=N` is cached for `N` seconds and
  served from cache within that window (with an `Age` header). The relay never
  serves a response staler than the paymaster's own `max-age` — so a cached quote
  can't outlive the window the paymaster is willing to honor at execute time.
- `no-store` / `no-cache` / `private`, or a missing / zero `max-age`, disable
  caching for that response — the relay refetches every time.
- The cache is **shared across clients** (the responses are identity-free), which
  also reduces the request volume the paymaster sees.

The **Go** relay uses a small in-memory TTL cache (`cache.go`); the **Worker**
uses the Cloudflare edge cache (`caches.default`), which honors origin
`Cache-Control` automatically. To set the TTL, set `Cache-Control: max-age=…` on
the paymaster's `gas-quote` / `supported-tokens` responses (≤ its quote validity
window).

Both implementations allow public browser access with `Access-Control-Allow-Origin: *`
and handle `OPTIONS` preflights for the OHTTP `Content-Type`. They do not use
cookies or browser credentials.

## Configuration

| Env | Meaning |
|---|---|
| `OHTTP_GATEWAY_URL` | **required** — base URL of the paymaster's OHTTP gateway (e.g. `https://paymaster.internal`) |
| `RELAY_LISTEN` | bind address (default `:8080`) |

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
```

## Cloudflare Worker variant

For a serverless deployment, see [`worker/`](worker/) — the same keyless relay
as a Cloudflare Worker.

From this repository, deploy the production Worker and verify its browser CORS
preflight with:

```bash
make deploy
```

The target runs `go test ./...`, performs a Wrangler dry run, deploys the custom
domain from `worker/wrangler.toml`, and checks the live `/health` and
`OPTIONS /gateway` responses. Run `make cloudflare-login` first if Wrangler is
not authenticated.

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

## See also

- [Relay architecture, operator choices, and security guarantees](https://docs.anon.inc/advanced/paymaster-relay)
- [Oblivious HTTP (RFC 9458)](https://www.rfc-editor.org/rfc/rfc9458.html)
- [HPKE (RFC 9180)](https://www.rfc-editor.org/rfc/rfc9180.html)
