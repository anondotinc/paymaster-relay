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

export default {
  async fetch(req: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(req.url);
    if (req.method === "OPTIONS") {
      return withCors(new Response(null, { status: 204 }));
    }
    if (url.pathname === "/health") return withCors(new Response("ok"));

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
