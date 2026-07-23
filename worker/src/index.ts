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
      // Forward only the encapsulated body + content-type; no client headers.
      // (X-Real-IP override suppresses X-Forwarded-For; see the CF-Connecting-IP
      // caveat at the top of this file.)
      const resp = await fetch(env.TARGET + "/gateway", {
        method: "POST",
        headers: { "content-type": "message/ohttp-req", ...STRIP_IP_HEADERS },
        body: req.body,
      });
      return withCors(new Response(resp.body, { status: resp.status, headers: { "content-type": "message/ohttp-res" } }));
    }
    return withCors(new Response("not found", { status: 404 }));
  },
};
