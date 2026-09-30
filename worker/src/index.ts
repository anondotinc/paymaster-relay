export interface Env { TARGET: string } // the paymaster gateway base URL

// ⚠️ SECURITY CAVEAT — client IP on Cloudflare subrequests.
// When a Worker fetch() targets a NON-Cloudflare origin, Cloudflare injects the
// real eyeball IP into `CF-Connecting-IP` (and `X-Forwarded-For`), and
// `CF-Connecting-IP` is NOT alterable for non-Cloudflare zones. So THIS Worker
// variant cannot, by itself, fully hide the client IP from the paymaster — the
// whole point of the relay. We override X-Real-IP below (which suppresses
// X-Forwarded-For), but CF-Connecting-IP still carries the client IP.
// Therefore, to use the Worker variant safely you MUST ensure the paymaster's
// ingress STRIPS CF-Connecting-IP / X-Forwarded-For / X-Real-IP / True-Client-IP
// before any logging or processing (the paymaster app already ignores them).
// For the strongest guarantee with no such dependency, run the Go+Docker relay
// (relay.go) — it forwards no client headers at all.
const STRIP_IP_HEADERS: Record<string, string> = { "X-Real-IP": "0.0.0.0" };
const MAX_GATEWAY_BODY = 1 << 20;
const MAX_GATEWAY_CONCURRENCY = 128;
const MAX_SOURCE_REQUESTS_PER_MINUTE = 60;
const GATEWAY_TIMEOUT_MS = 20_000;

const sourceWindows = new Map<string, { started: number; count: number }>();
const sourceSalt = crypto.getRandomValues(new Uint8Array(32));
let activeGatewayRequests = 0;

async function ephemeralSourceKey(req: Request): Promise<string> {
  const source = req.headers.get("CF-Connecting-IP") ?? "unknown";
  const sourceBytes = new TextEncoder().encode(source);
  const material = new Uint8Array(sourceSalt.length + sourceBytes.length);
  material.set(sourceSalt);
  material.set(sourceBytes, sourceSalt.length);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", material));
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function sourceAllowed(req: Request): Promise<boolean> {
  const now = Date.now();
  if (sourceWindows.size > 10_000) {
    for (const [key, window] of sourceWindows) {
      if (now - window.started >= 60_000) sourceWindows.delete(key);
    }
    if (sourceWindows.size > 10_000) {
      sourceWindows.delete(sourceWindows.keys().next().value as string);
    }
  }
  const key = await ephemeralSourceKey(req);
  const window = sourceWindows.get(key);
  if (!window || now - window.started >= 60_000) {
    sourceWindows.set(key, { started: now, count: 1 });
    return true;
  }
  if (window.count >= MAX_SOURCE_REQUESTS_PER_MINUTE) return false;
  window.count += 1;
  return true;
}

const CORS_HEADERS: Record<string, string> = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
  "Access-Control-Allow-Headers": "Content-Type",
  "Access-Control-Max-Age": "86400",
};

function withCors(response: Response): Response {
  const headers = new Headers(response.headers);
  for (const [name, value] of Object.entries(CORS_HEADERS)) headers.set(name, value);
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

// Public, cacheable, identity-free GET reads the relay proxies so the client
// fetches them via the relay, never the paymaster directly. Keep in sync with
// publicReadPaths in relay.go.
const PUBLIC_READ_PATHS = new Set<string>([
  "/api/v1/paymaster/gas-quote",
  "/api/v1/paymaster/supported-tokens",
]);

// Public gas reads: the paymaster's gas tiers and the API's network base fee.
// Both are identity-free and fresh-only (the API answers
// "public, s-maxage=5, max-age=5"). Unlike the verbatim reads above, the relay
// forwards ONLY a validated, canonical chainId on a fixed path: no caller
// query, path segment or header reaches the API or the cache key. Keep in sync
// with gas_reads.go.
const GAS_TIERS_PATH = "/api/v1/paymaster/gas-tiers";
const GAS_FEE_PREFIX = "/api/v1/tx/gas-fee/";
const GAS_READ_CHAINS = new Set<string>(["1", "56", "137", "42161"]);
const MAX_GAS_READ_QUERY = 128;
const NO_STORE: Record<string, string> = { "Cache-Control": "no-store" };

/**
 * The canonical upstream path + query of a gas read: `undefined` when the path
 * is not a gas read, `null` when it is one but invalid. gas-tiers takes exactly
 * one chain field (chainId, or its protobuf spelling chain_id); gas-fee takes
 * the chain as its only path segment and no query at all.
 */
function gasReadTarget(url: URL): string | null | undefined {
  if (url.pathname === GAS_TIERS_PATH) {
    const raw = url.search.slice(1);
    if (raw === "" || raw.length > MAX_GAS_READ_QUERY) return null;
    const fields = [...new URLSearchParams(raw)];
    if (fields.length !== 1) return null;
    const [key, chain] = fields[0];
    if ((key !== "chainId" && key !== "chain_id") || !GAS_READ_CHAINS.has(chain)) return null;
    return `${GAS_TIERS_PATH}?chainId=${chain}`;
  }
  if (url.pathname.startsWith(GAS_FEE_PREFIX)) {
    const chain = url.pathname.slice(GAS_FEE_PREFIX.length);
    if (chain === "" || chain.includes("/")) return undefined;
    if (url.search !== "" || !GAS_READ_CHAINS.has(chain)) return null;
    return GAS_FEE_PREFIX + chain;
  }
  return undefined;
}

/**
 * Seconds of shared-cache freshness left on an upstream response: s-maxage (or
 * max-age) minus Age. 0 means it must not be stored or reused.
 */
function remainingFreshness(headers: Headers): number {
  if ((headers.get("Vary") ?? "").split(",").some((value) => value.trim() === "*")) return 0;
  const directives = (headers.get("Cache-Control") ?? "").toLowerCase().split(",").map((value) => value.trim());
  if (directives.some((value) => ["no-store", "no-cache", "private"].includes(value.split("=")[0].trim()))) return 0;
  const lifetime = (directives.find((value) => value.startsWith("s-maxage=")) ??
    directives.find((value) => value.startsWith("max-age=")))?.split("=")[1];
  const age = headers.get("Age") ?? "0";
  if (!lifetime || !/^[0-9]+$/.test(lifetime) || !/^[0-9]+$/.test(age)) return 0;
  const remaining = Number(lifetime) - Number(age);
  return Number.isSafeInteger(remaining) && remaining > 0 ? remaining : 0;
}

/**
 * Serve a validated gas read through the edge cache. A 200 is cached for what
 * is left of the API's freshness (never extended); errors pass through with
 * `no-store` and are never cached. Response headers are rebuilt: no cookies,
 * Vary or tracing headers from the API reach the client or the cache.
 */
async function gasRead(target: string, url: URL, env: Env, ctx: ExecutionContext): Promise<Response> {
  const cache = caches.default;
  const cacheKey = new Request(url.origin + target, { method: "GET" });
  const cached = await cache.match(cacheKey);
  if (cached) return withCors(cached);
  let upstream: Response;
  try {
    upstream = await fetch(env.TARGET + target, { headers: STRIP_IP_HEADERS, redirect: "manual" });
  } catch {
    return withCors(new Response("bad gateway", { status: 502, headers: NO_STORE }));
  }
  if (upstream.status < 200 || upstream.status >= 300 && upstream.status < 400) {
    void upstream.body?.cancel().catch(() => {});
    return withCors(new Response("bad gateway", { status: 502, headers: NO_STORE }));
  }
  const fresh = upstream.status === 200 ? remainingFreshness(upstream.headers) : 0;
  const headers = new Headers({
    "Cache-Control": fresh > 0 ? `public, max-age=${fresh}` : "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  const contentType = upstream.headers.get("Content-Type");
  if (contentType) headers.set("Content-Type", contentType);
  const response = new Response(upstream.body, { status: upstream.status, headers });
  if (fresh > 0) ctx.waitUntil(cache.put(cacheKey, response.clone()));
  return withCors(response);
}

export default {
  async fetch(req: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(req.url);
    if (req.method === "OPTIONS") {
      return withCors(new Response(null, { status: 204 }));
    }
    if (url.pathname === "/health") return withCors(new Response("ok"));

    if (req.method === "GET") {
      const target = gasReadTarget(url);
      if (target === null) return withCors(new Response("invalid gas read", { status: 400, headers: NO_STORE }));
      if (target !== undefined) return gasRead(target, url, env, ctx);
    }

    // /ohttp-configs + the public reads are cacheable GETs. Serve them through
    // the edge cache, which honors the paymaster's Cache-Control (max-age /
    // no-store) automatically — so we never serve a quote staler than the
    // paymaster permits. Forward path+query only; no client headers, no PII.
    if (
      req.method === "GET" &&
      (url.pathname === "/ohttp-configs" || PUBLIC_READ_PATHS.has(url.pathname))
    ) {
      const cache = caches.default;
      const cacheKey = new Request(url.toString(), { method: "GET" });
      let resp = await cache.match(cacheKey);
      if (!resp) {
        resp = await fetch(env.TARGET + url.pathname + url.search, { headers: STRIP_IP_HEADERS });
        if (resp.status === 200) {
          ctx.waitUntil(cache.put(cacheKey, resp.clone()));
        }
      }
      return withCors(resp);
    }
    if (url.pathname === "/gateway" && req.method === "POST") {
      if (req.headers.get("content-type") !== "message/ohttp-req")
        return withCors(new Response("unsupported media type", { status: 415 }));
      const contentLength = Number(req.headers.get("content-length") ?? "0");
      if (Number.isFinite(contentLength) && contentLength > MAX_GATEWAY_BODY)
        return withCors(new Response("request too large", { status: 413 }));
      if (!(await sourceAllowed(req)))
        return withCors(new Response("rate limited", { status: 429, headers: { "Retry-After": "60" } }));
      if (activeGatewayRequests >= MAX_GATEWAY_CONCURRENCY)
        return withCors(new Response("relay busy", { status: 503 }));
      const body = await req.arrayBuffer();
      if (body.byteLength > MAX_GATEWAY_BODY)
        return withCors(new Response("request too large", { status: 413 }));
      // Forward only the encapsulated body + content-type; no client headers.
      // (X-Real-IP override suppresses X-Forwarded-For; see the CF-Connecting-IP
      // caveat at the top of this file.)
      activeGatewayRequests += 1;
      try {
        const resp = await fetch(env.TARGET + "/gateway", {
          method: "POST",
          headers: { "content-type": "message/ohttp-req", ...STRIP_IP_HEADERS },
          body,
          signal: AbortSignal.timeout(GATEWAY_TIMEOUT_MS),
        });
        return withCors(new Response(resp.body, { status: resp.status, headers: { "content-type": "message/ohttp-res" } }));
      } catch {
        return withCors(new Response("bad gateway", { status: 502 }));
      } finally {
        activeGatewayRequests -= 1;
      }
    }
    return withCors(new Response("not found", { status: 404 }));
  },
};
