# anon-ohttp-relay (Cloudflare Worker)

A **keyless** Oblivious HTTP (RFC 9458) relay for the Anon paymaster, deployed as
a serverless [Cloudflare Worker](https://developers.cloudflare.com/workers/). It
is the same relay as the [Go + Docker variant](../README.md) — it sits in front
of the paymaster (which holds the HPKE key and terminates OHTTP) and forwards
encapsulated requests to it:

```
Client ──HPKE-seal(to paymaster key)──▶  Relay (this)  ──▶  Paymaster (OHTTP gateway + target)
        message/ohttp-req                sees client IP,      decrypts; sees the
                                          NOT the content      content + relay IP,
                                                               NOT the client IP
```

This relay holds **no keys** and sees **no plaintext** — it forwards only the
encapsulated body and its `Content-Type`, never any client header it received.

> ⚠️ **Cloudflare adds the client IP to subrequests — read this before deploying.**
> When a Worker `fetch()`es a **non-Cloudflare** origin, Cloudflare **injects the
> real client (eyeball) IP into `CF-Connecting-IP`** (and `X-Forwarded-For`), and
> `CF-Connecting-IP` **cannot be altered** for non-Cloudflare zones. So — unlike
> the [Go + Docker relay](../README.md), which forwards no client headers at all —
> this Worker variant **cannot by itself hide the client IP from the paymaster**.
> The diagram's "NOT the client IP" holds **only if** you also do one of:
> - deploy the paymaster behind an ingress (nginx/Envoy/WAF) that **strips**
>   `CF-Connecting-IP` / `X-Forwarded-For` / `X-Real-IP` / `True-Client-IP`
>   before any logging or processing (the paymaster app already ignores them), or
> - use the **Go + Docker relay** instead (no such dependency).
>
> The Worker sets `X-Real-IP: 0.0.0.0` on subrequests (which suppresses
> `X-Forwarded-For`), but that does **not** remove `CF-Connecting-IP`.

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
  → the paymaster's public quote and fee-token reads (path + query).
- `GET /api/v1/paymaster/gas-tiers?chainId=<id>` and `GET /api/v1/tx/gas-fee/<id>`
  → the gas tiers and network base fee. Fresh-only public reads: `<id>` must be
  `1`, `56`, `137` or `42161`; `gas-tiers` accepts exactly one `chainId` (or
  `chain_id`) and nothing else; `gas-fee` accepts no query. Invalid requests get
  a `400 no-store` without reaching the API. Only the canonical target is
  forwarded and used as the edge-cache key; a `200` is cached for what is left
  of the API's `s-maxage`/`max-age` after `Age`, errors are relayed `no-store`
  and never cached, redirects become `502`, and cookies/`Vary`/tracing headers
  are dropped. See the [root README](../README.md#endpoints).

All responses include permissive CORS headers, and `OPTIONS` preflights are
accepted, so public browser clients can call the keyless relay directly. The
relay does not use cookies or browser credentials.

## Configuration

| Var | Meaning |
|---|---|
| `TARGET` | base URL of the paymaster's OHTTP gateway (e.g. `https://paymaster.internal`) |

Set `TARGET` in [`wrangler.toml`](wrangler.toml) under `[vars]`, or override it
per-environment with `npx wrangler secret put TARGET` / the Cloudflare dashboard.

## Test

From the repository root (Node 22+, no Wrangler, network or credentials):

```bash
node --experimental-strip-types --test worker/test/*.test.mjs
```

The tests stub `fetch` and `caches.default`; they do not verify Cloudflare-added
headers or the deployed route.

## Deploy

From the repository root:

```bash
make deploy
```

This runs the Go and Worker tests, checks the Worker bundle, deploys it, and verifies
the live browser CORS preflight. If Wrangler needs authentication, first run
`make cloudflare-login`.

For a low-level deploy from this `worker/` directory, run
`npx --yes wrangler@4 deploy`.

The checked-in configuration deploys Anon's production relay. For another
Cloudflare account, fork the repository and change the Worker `name`, custom
domain in `routes`, and `TARGET` in `wrangler.toml`. The custom domain must be in
a Cloudflare zone you control. Then run:

```bash
make deploy RELAY_URL=https://relay.example.com
```

Cloudflare manages the custom-domain DNS record and certificate. `TARGET` must
be the intended paymaster's HTTPS OHTTP gateway; the Worker is deliberately a
fixed-target relay, not an open proxy.
