# anon-ohttp-relay (Cloudflare Worker)

A **keyless** Oblivious HTTP (RFC 9458) relay for the Anon paymaster and optional
scheduled-payment gateway, deployed as
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

This relay holds **no gateway keys** and cannot decrypt sealed requests. It
does see the client IP, timing, sizes, selected service, and any public fee-read
query. Request headers are reconstructed from a fixed allowlist, not copied
from the caller.

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

Cloudflare documents different same-zone, cross-zone, and non-Cloudflare
subrequest behavior. Review the [current header rules](https://developers.cloudflare.com/fundamentals/reference/http-headers/)
for the actual deployed path; the source-only tests below are not deployment
verification.

> **The one non-negotiable rule (RFC 9458 §6):** the relay and the paymaster
> **MUST be operated by different, non-colluding parties.** If Anon runs both,
> the split buys nothing (either party — or a joint subpoena/compromise — can
> re-link IP ↔ content). Deploy this relay under an operator that is genuinely
> independent of the party running the paymaster.

## Endpoints

- `GET /health` → `200 ok` (relay/LB health check; not forwarded).
- `GET /ohttp-configs` → proxied from the paymaster (the signed HPKE keyconfig).
  Clients must verify the signature against their independently pinned paymaster
  key. Fetching a key from a custom relay does not make the relay trustworthy.
- `POST /gateway` (`Content-Type: message/ohttp-req`) → the paymaster's
  `message/ohttp-res`, relayed back unchanged.
- `GET /scheduler/ohttp-configs` and `POST /scheduler/gateway` → the **separate**
  scheduler gateway's `/ohttp-configs` and `/gateway` under `SCHEDULER_TARGET`.
  Without a distinct, valid scheduler target these return `503`, never a
  paymaster response. Scheduler clients must use their own signing-key pin.
- `GET /api/v1/paymaster/gas-quote?chainId=42161&priority=normal` and
  `GET /api/v1/paymaster/supported-tokens?chainId=42161` remain available for
  compatibility. These public reads are **not sealed**. Only `chainId` (or the
  protobuf spelling `chain_id`) and optional quote `priority` are accepted.
  Chains are exactly `1`, `56`, `137`, `42161` (no leading zeroes); priorities
  are `slow`, `normal`, `fast` (case-insensitive). Raw public query strings are
  limited to 128 characters.
  Parameters are normalized before forwarding; unknown/duplicate parameters
  are rejected. No query parameters are accepted on other endpoints.
- `GET /api/v1/paymaster/gas-tiers?chainId=<id>`, `GET /api/v1/paymaster/gas-history?chainId=<id>`
  and `GET /api/v1/tx/gas-fee/<id>` → the gas tiers, gas history and network base fee. Fresh-only public reads: `<id>` must be
  `1`, `56`, `137` or `42161`; `gas-tiers` and `gas-history` accept exactly one `chainId` (or
  `chain_id`) and nothing else; `gas-fee` accepts no query. Invalid requests get
  a `400 no-store` without reaching the API. Only the canonical target is
  forwarded and used as the edge-cache key; a `200` is cached for what is left
  of the API's `s-maxage`/`max-age` after `Age`, errors are relayed `no-store`
  and never cached, redirects become `502`, and cookies/`Vary`/tracing headers
  are dropped. See the [root README](../README.md#endpoints).

All responses include permissive CORS headers, and `OPTIONS` preflights on
known routes are accepted, so browser clients can call the keyless relay. The
relay does not use cookies or browser credentials.

## Limits and failure behavior

- Paymaster sealed uploads and all upstream responses: **1 MiB**. Scheduler
  uploads: **4 MiB**, allowing overhead for a 2 MiB serialized payment bundle.
- Streams are bounded during consumption, including when `Content-Length` is
  absent. Wrong MIME, truncation, unsupported encoding, redirects, and
  non-success upstream responses fail closed. Upstream requests explicitly ask
  for identity encoding.
- Concurrency is reserved before reading uploads (128 requests per isolate).
  An additional conservative 32 MiB buffered-work reservation limits large
  uploads sooner; this leaves headroom within the [Worker memory limit](https://developers.cloudflare.com/workers/platform/limits/#memory).
  Accepted requests have a 20-second deadline and follow caller cancellation.
  A process-local, salted per-source bucket permits 60 requests per minute;
  this is load protection, not a global rate-limit guarantee.
- Sealed responses and errors are `no-store`. Only validated public reads and
  signed configs can enter the edge cache, respecting the gateway's freshness
  directives and Age. Cache keys include target, service, and format version.
  The returned header set excludes cookies, redirects, and tracing identifiers.
- A timeout after submission is **not proof that the operation failed**. Clients
  must reconcile with the original operation/capability, not create a new
  payment or resubmit a different transaction.

## Configuration

| Var | Meaning |
|---|---|
| `TARGET` | base URL of the paymaster's OHTTP gateway (e.g. `https://paymaster.internal`) |
| `SCHEDULER_TARGET` | optional, distinct base URL of the backend's scheduler OHTTP gateway; **unset by default** |

Set `TARGET` in [`wrangler.toml`](wrangler.toml) under `[vars]`, or override it
per-environment with `npx wrangler secret put TARGET` / the Cloudflare dashboard.

Use HTTPS for production targets. Operator-controlled internal HTTP targets
are accepted, but do not encrypt that relay-to-gateway hop. Base paths are
supported; credentials, query strings, fragments, encoded/traversal paths, and
duplicate slashes are rejected. The targets can never be selected by a caller.

Scheduler activation is a separate release gate: provision its backend gateway
and client signing-key pin first, then configure `SCHEDULER_TARGET`. Do not reuse
the paymaster target or its encryption/signing keys. The checked-in Wrangler
configuration intentionally does not activate scheduler routing.

The corresponding backend `MountScheduler` helper is staged under its own
`/scheduler` namespace. Its server call-site switch, ingress route to the
backend (not the paymaster or shard router), production configuration, and
client activation must ship together in a later approved backend phase. If
using that namespace, `SCHEDULER_TARGET` ends in `/scheduler`; the relay appends
`/gateway` or `/ohttp-configs`. Do not enable it based on the helper alone.

## Local verification

From the relay repository, Node 22.23.1 runs the Worker source tests without
Wrangler, production requests, or credentials:

```bash
node --experimental-strip-types --test worker/test/*.test.mjs
```

These tests (`relay.test.mjs` for the sealed routes and hardening,
`gas-reads.test.mjs` for the public gas reads) exercise mocked fetch/cache and real in-memory streams. They do
**not** verify Cloudflare-added headers, deployed ingress rules, or production
IP separation. Before activation, send synthetic canary identifiers through
the intended deployment and verify removal before **every** gateway ingress
and logging layer, including the scheduler. Never use a real wallet or payment
payload for that check.

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
