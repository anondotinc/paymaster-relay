export interface Env {
  TARGET: string;
  /** Optional dedicated scheduler gateway. Never inferred from TARGET. */
  SCHEDULER_TARGET?: string;
  /**
   * Optional PPOI gateway base (`/ppoi/*` routes), e.g. https://proxy.anon.inc/ohttp.
   * Unset disables the routes (404). Never inferred; must not share a base with
   * TARGET or SCHEDULER_TARGET.
   */
  PPOI_TARGET?: string;
}

interface WorkerContext { waitUntil(promise: Promise<unknown>): void }
interface RelayCache {
  match(request: Request): Promise<Response | undefined>;
  put(request: Request, response: Response): Promise<void>;
}
interface RelayOptions {
  fetch?: typeof globalThis.fetch;
  cache?: RelayCache | null;
  timeoutMs?: number;
  maxConcurrency?: number;
}

// SECURITY CAVEAT: Cloudflare may inject the visitor IP into subrequests AFTER
// these application-level header choices, particularly to non-Cloudflare
// origins. This override is not proof of IP separation. Both gateways' ingress
// and logging paths must be independently verified before making that claim.
const STRIP_IP_HEADERS = { "X-Real-IP": "0.0.0.0" };
const MAX_BODY = 1 << 20;
const MAX_SCHEDULER_REQUEST = 4 << 20;
const MAX_CONCURRENCY = 128;
// Leave headroom for the runtime/cache and stream-copy overhead within an
// isolate. A request-count cap alone is unsafe for 4 MiB scheduler uploads.
// The budget is per ISOLATE, shared by every caller routed to it.
export const MAX_BUFFERED_WORK = 32 << 20;
// Per-route, per-source rate-limit buckets: PPOI sync (3 concurrent
// 50-commitment batches) must not starve paymaster status polling, and vice versa.
const BUCKET_LIMITS = { paymaster: 60, scheduler: 60, ppoi: 180 } as const;
type Bucket = keyof typeof BUCKET_LIMITS;
// Per-route body caps; each request reserves buffer budget from ITS route's caps.
// paymaster and scheduler match the backend gateway limits (decap.PaymasterRequestLimit
// 1 MiB, SchedulerRequestLimit 4 MiB, ResponseLimit 1 MiB) and are unchanged.
// PPOI carries small sealed JSON-RPC envelopes (SDK batches: 50 blinded
// commitments per ppoi_pois_per_list, 20 legacy proofs per submit; measured
// worst cases: request ~7 KB, response ~68 KB for a 50-proof ppoi_merkle_proofs,
// ~18 KB for the 13-input maximum of a real spend). The caps are 9x / 3.9x
// those worst cases (14x for the real 13-input response). A body above a cap
// still fails cleanly: 413 for a request, 502 for a response, never truncated.
export const ROUTE_LIMITS: Record<Bucket, { request: number; response: number }> = {
  paymaster: { request: MAX_BODY, response: MAX_BODY },
  scheduler: { request: MAX_SCHEDULER_REQUEST, response: MAX_BODY },
  ppoi: { request: 64 << 10, response: 256 << 10 },
};
/**
 * Worst-case bytes one request pins in the isolate while it runs. Both bodies
 * are read with readBounded, which holds the received chunks AND the
 * concatenated copy at once. A sealed request body is therefore reserved 2x
 * (chunks + copy handed to fetch), and a response 3x (chunks + copy + the
 * Response/cache.put clone built from it). Config and public reads have no
 * request body, so only the 3x response term applies.
 */
export function bufferReservation(bucket: Bucket, gateway: boolean): number {
  const { request, response } = ROUTE_LIMITS[bucket];
  return (gateway ? 2 * request : 0) + 3 * response;
}
// Sealed routes nest above the backend gateway ladder (handler 25 s < forwarder
// 28 s < gateway 30-35 s < relay 40 s); public reads and key configs keep 20 s.
const SEALED_TIMEOUT_MS = 40_000;
const TIMEOUT_MS = 20_000;
const CONFIG_TYPE = "application/ohttp-keys-signed";
const REQUEST_TYPE = "message/ohttp-req";
const RESPONSE_TYPE = "message/ohttp-res";
const QUOTE_PATH = "/api/v1/paymaster/gas-quote";
const TOKENS_PATH = "/api/v1/paymaster/supported-tokens";
const CHAINS = new Set(["1", "56", "137", "42161"]);
const CORS_HEADERS = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
  "Access-Control-Allow-Headers": "Content-Type",
  "Access-Control-Max-Age": "86400",
};

class RelayFailure extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function reply(body: BodyInit | null, status = 200, headers: HeadersInit = {}): Response {
  const safeHeaders = new Headers(headers);
  for (const [name, value] of Object.entries(CORS_HEADERS)) safeHeaders.set(name, value);
  if (!safeHeaders.has("Cache-Control")) safeHeaders.set("Cache-Control", "no-store");
  return new Response(body, { status, headers: safeHeaders });
}

function fail(status: number, message: string): Response {
  return reply(message, status, { "Content-Type": "text/plain; charset=utf-8" });
}

function targetBase(value: string | undefined): string | undefined {
  if (!value || value !== value.trim() || /[\\%?#]/.test(value) || /\/(?:\.|\.\.)(?:\/|$)/.test(value)) return undefined;
  try {
    const url = new URL(value);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password ||
        url.search || url.hash || url.pathname.includes("%") || url.pathname.includes("//")) return undefined;
    return url.href.replace(/\/+$/, "");
  } catch {
    return undefined;
  }
}

/** Only fixed protobuf read fields cross the public, unsealed query surface. */
function canonicalQuery(path: string, url: URL): string {
  if (path !== QUOTE_PATH && path !== TOKENS_PATH) {
    if (url.search) throw new RelayFailure(400, "query parameters not allowed");
    return "";
  }
  if (url.search.slice(1).length > 128) throw new RelayFailure(400, "query too large");
  const chainValues = [...url.searchParams.getAll("chainId"), ...url.searchParams.getAll("chain_id")];
  for (const key of url.searchParams.keys()) {
    if (key !== "chainId" && key !== "chain_id" && !(path === QUOTE_PATH && key === "priority")) {
      throw new RelayFailure(400, "unknown query parameter");
    }
  }
  if (chainValues.length !== 1) {
    throw new RelayFailure(400, "invalid chain");
  }
  const chain = chainValues[0];
  if (!CHAINS.has(chain)) throw new RelayFailure(400, "unsupported chain");
  const priorities = url.searchParams.getAll("priority");
  if (priorities.length > 1) throw new RelayFailure(400, "duplicate priority");
  const priority = priorities[0]?.toLowerCase();
  if (priority && !["slow", "normal", "fast"].includes(priority)) throw new RelayFailure(400, "invalid priority");
  const query = new URLSearchParams({ chainId: chain });
  if (priority) query.set("priority", priority);
  return "?" + query.toString();
}

function withCors(response: Response): Response {
  const headers = new Headers(response.headers);
  for (const [name, value] of Object.entries(CORS_HEADERS)) headers.set(name, value);
  return new Response(response.body, { status: response.status, statusText: response.statusText, headers });
}

function declaredLength(headers: Headers, limit: number, status: number): number | undefined {
  const raw = headers.get("Content-Length");
  if (raw == null) return undefined;
  if (!/^[0-9]+$/.test(raw)) throw new RelayFailure(400, "invalid body length");
  const length = Number(raw);
  if (!Number.isSafeInteger(length) || length > limit) throw new RelayFailure(status, "body too large");
  return length;
}

function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) {
    void promise.catch(() => {});
    return Promise.reject(signal.reason);
  }
  return new Promise((resolve, reject) => {
    const abort = () => { cleanup(); reject(signal.reason); };
    const cleanup = () => signal.removeEventListener("abort", abort);
    signal.addEventListener("abort", abort, { once: true });
    promise.then(value => { cleanup(); resolve(value); }, error => { cleanup(); reject(error); });
  });
}

/** Bound the stream while reading; neither request nor response uses arrayBuffer(). */
async function readBounded(
  body: ReadableStream<Uint8Array> | null,
  headers: Headers,
  limit: number,
  signal: AbortSignal,
  tooLargeStatus: number,
): Promise<Uint8Array> {
  let expected: number | undefined;
  try {
    expected = declaredLength(headers, limit, tooLargeStatus);
  } catch (error) {
    void body?.cancel().catch(() => {});
    throw error;
  }
  if (!body) {
    if (expected) throw new RelayFailure(400, "truncated body");
    return new Uint8Array();
  }
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  let completed = false;
  try {
    while (true) {
      const { done, value } = await abortable(reader.read(), signal);
      if (done) break;
      length += value.byteLength;
      if (length > limit) throw new RelayFailure(tooLargeStatus, "body too large");
      chunks.push(value);
    }
    if (expected != null && expected !== length) throw new RelayFailure(400, "truncated body");
    const result = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
    completed = true;
    return result;
  } finally {
    if (!completed) void reader.cancel().catch(() => {});
    try { reader.releaseLock(); } catch { /* pending aborted reads are cancelled above */ }
  }
}

function contentType(response: Response, expected: string): boolean {
  const type = response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase();
  const encoding = response.headers.get("Content-Encoding")?.trim().toLowerCase();
  return type === expected && (!encoding || encoding === "identity");
}

/** Rebuild cache metadata too: never copy Set-Cookie, tracing or identity headers. */
function publicCacheControl(headers: Headers): string | undefined {
  if (headers.get("Vary")) return undefined;
  const directives = (headers.get("Cache-Control") ?? "").toLowerCase().split(",").map(value => value.trim());
  if (directives.some(value => ["no-store", "no-cache", "private"].includes(value.split("=")[0].trim()))) return undefined;
  const max = directives.find(value => value.startsWith("s-maxage=")) ?? directives.find(value => value.startsWith("max-age="));
  const text = max?.split("=")[1];
  if (!text || !/^[0-9]+$/.test(text)) return undefined;
  const ageText = headers.get("Age") ?? "0";
  if (!/^[0-9]+$/.test(ageText)) return undefined;
  const lifetime = Number(text);
  const age = Number(ageText);
  if (!Number.isSafeInteger(lifetime) || !Number.isSafeInteger(age)) return undefined;
  const remaining = lifetime - age;
  return remaining > 0 ? `public, max-age=${remaining}` : undefined;
}

// Public gas reads: the paymaster's gas tiers and gas history and the API's
// network base fee. All are identity-free and fresh-only (the API answers
// "public, s-maxage=5, max-age=5"). Unlike the verbatim reads above, the relay
// forwards ONLY a validated, canonical chainId on a fixed path: no caller
// query, path segment or header reaches the API or the cache key. Keep in sync
// with gas_reads.go.
const GAS_TIERS_PATH = "/api/v1/paymaster/gas-tiers";
// The paymaster's recent gas history (the apps' live gas chart). Same shape
// of read as gas-tiers: one canonical chainId, identity-free, fresh-only.
const GAS_HISTORY_PATH = "/api/v1/paymaster/gas-history";
const CHAIN_QUERY_PATHS = new Set<string>([GAS_TIERS_PATH, GAS_HISTORY_PATH]);
const GAS_FEE_PREFIX = "/api/v1/tx/gas-fee/";
const GAS_READ_CHAINS = new Set<string>(["1", "56", "137", "42161"]);
const MAX_GAS_READ_QUERY = 128;
const NO_STORE: Record<string, string> = { "Cache-Control": "no-store" };

/**
 * The canonical upstream path + query of a gas read: `undefined` when the path
 * is not a gas read, `null` when it is one but invalid. gas-tiers and
 * gas-history take exactly one chain field (chainId, or its protobuf spelling
 * chain_id); gas-fee takes the chain as its only path segment and no query at
 * all.
 */
function gasReadTarget(url: URL): string | null | undefined {
  if (CHAIN_QUERY_PATHS.has(url.pathname)) {
    const raw = url.search.slice(1);
    if (raw === "" || raw.length > MAX_GAS_READ_QUERY) return null;
    const fields = [...new URLSearchParams(raw)];
    if (fields.length !== 1) return null;
    const [key, chain] = fields[0];
    if ((key !== "chainId" && key !== "chain_id") || !GAS_READ_CHAINS.has(chain)) return null;
    return `${url.pathname}?chainId=${chain}`;
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

// Workers refuse random values in global scope (deploy error 10021), so the
// per-isolate salt is drawn on the first request instead of at load.
let sourceSalt: Uint8Array | undefined;

function isolateSalt(): Uint8Array {
  sourceSalt ??= crypto.getRandomValues(new Uint8Array(32));
  return sourceSalt;
}

export function createRelayHandler(options: RelayOptions = {}) {
  const sourceWindows = new Map<string, { started: number; count: number }>();
  const fetchUpstream = options.fetch ?? ((...args: Parameters<typeof fetch>) => globalThis.fetch(...args));
  let activeRequests = 0;
  let reservedBytes = 0;

  async function sourceAllowed(req: Request, bucket: Bucket): Promise<boolean> {
    const salt = isolateSalt();
    const now = Date.now();
    if (sourceWindows.size >= 10_000) {
      for (const [key, window] of sourceWindows) {
        if (now - window.started >= 60_000) sourceWindows.delete(key);
      }
      if (sourceWindows.size >= 10_000) sourceWindows.delete(sourceWindows.keys().next().value as string);
    }
    const source = new TextEncoder().encode(bucket + "\0" + (req.headers.get("CF-Connecting-IP") ?? "unknown"));
    const material = new Uint8Array(salt.length + source.length);
    material.set(salt);
    material.set(source, salt.length);
    const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", material));
    const key = Array.from(digest, byte => byte.toString(16).padStart(2, "0")).join("");
    const window = sourceWindows.get(key);
    if (!window || now - window.started >= 60_000) {
      sourceWindows.set(key, { started: now, count: 1 });
      return true;
    }
    if (window.count >= BUCKET_LIMITS[bucket]) return false;
    window.count += 1;
    return true;
  }

  /**
   * Serve a validated gas read through the edge cache. A 200 is cached for what
   * is left of the API's freshness (never extended); errors pass through with
   * `no-store` and are never cached. Response headers are rebuilt: no cookies,
   * Vary or tracing headers from the API reach the client or the cache.
   */
  async function gasRead(target: string, url: URL, env: Env, ctx: WorkerContext): Promise<Response> {
    const base = targetBase(env.TARGET);
    if (!base) return fail(503, "gateway unavailable");
    const cache = options.cache === undefined
      ? (globalThis as typeof globalThis & { caches?: { default?: RelayCache } }).caches?.default
      : options.cache;
    const cacheKey = new Request(url.origin + target, { method: "GET" });
    const cached = await cache?.match(cacheKey);
    if (cached) return withCors(cached);
    let upstream: Response;
    try {
      upstream = await fetchUpstream(base + target, { headers: STRIP_IP_HEADERS, redirect: "manual" });
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
    if (cache && fresh > 0) ctx.waitUntil(cache.put(cacheKey, response.clone()));
    return withCors(response);
  }

  return {
    async fetch(req: Request, env: Env, ctx: WorkerContext): Promise<Response> {
      const url = new URL(req.url);
      const path = url.pathname;
      // Public gas reads: validated, canonical, fresh-only; not behind the
      // sealed-route limits (identity-free, cached for seconds).
      const gasTarget = gasReadTarget(url);
      if (gasTarget !== undefined && (req.method === "GET" || req.method === "OPTIONS")) {
        if (req.method === "OPTIONS") return reply(null, 204);
        if (gasTarget === null) return withCors(new Response("invalid gas read", { status: 400, headers: NO_STORE }));
        return gasRead(gasTarget, url, env, ctx);
      }
      const scheduler = path === "/scheduler/ohttp-configs" || path === "/scheduler/gateway";
      const ppoi = path === "/ppoi/ohttp-configs" || path === "/ppoi/gateway";
      const keyConfig = path === "/ohttp-configs" || path === "/scheduler/ohttp-configs" || path === "/ppoi/ohttp-configs";
      const gateway = path === "/gateway" || path === "/scheduler/gateway" || path === "/ppoi/gateway";
      const publicRead = path === QUOTE_PATH || path === TOKENS_PATH;
      if (!keyConfig && !gateway && !publicRead && path !== "/health") return fail(404, "not found");
      // PPOI is opt-in: with no PPOI_TARGET the routes do not exist.
      if (ppoi && !env.PPOI_TARGET) return fail(404, "not found");
      let query: string;
      try { query = canonicalQuery(path, url); } catch (error) {
        return fail(error instanceof RelayFailure ? error.status : 400, "invalid query");
      }
      if (req.method === "OPTIONS") return reply(null, 204);
      if (req.method !== (gateway ? "POST" : "GET")) return fail(405, "method not allowed");
      if (path === "/health") return reply("ok");
      if (gateway && req.headers.get("Content-Type") !== REQUEST_TYPE) return fail(415, "unsupported media type");
      const requestEncoding = req.headers.get("Content-Encoding")?.trim().toLowerCase();
      if (requestEncoding && requestEncoding !== "identity") return fail(415, "unsupported content encoding");
      const paymasterBase = targetBase(env.TARGET);
      const schedulerBase = targetBase(env.SCHEDULER_TARGET);
      const ppoiBase = ppoi ? targetBase(env.PPOI_TARGET) : undefined;
      const base = ppoi ? ppoiBase : scheduler ? schedulerBase : paymasterBase;
      const bucket: Bucket = ppoi ? "ppoi" : scheduler ? "scheduler" : "paymaster";
      if (!base || (scheduler && base === paymasterBase) ||
          (ppoi && (base === paymasterBase || base === targetBase(env.SCHEDULER_TARGET)))) return fail(503, "gateway unavailable");
      const { request: requestLimit, response: responseLimit } = ROUTE_LIMITS[bucket];
      const reservation = bufferReservation(bucket, gateway);
      if (activeRequests >= (options.maxConcurrency ?? MAX_CONCURRENCY) ||
          reservedBytes + reservation > MAX_BUFFERED_WORK) return fail(503, "relay busy");

      // Reserve before any awaits/body reads so slow uploads cannot evade the cap.
      activeRequests += 1;
      reservedBytes += reservation;
      const controller = new AbortController();
      const abort = () => controller.abort(req.signal.reason);
      req.signal.addEventListener("abort", abort, { once: true });
      if (req.signal.aborted) abort();
      const timer = setTimeout(() => controller.abort(new Error("relay timeout")), options.timeoutMs ?? (gateway ? SEALED_TIMEOUT_MS : TIMEOUT_MS));
      const signal = controller.signal;
      try {
        if (!(await abortable(sourceAllowed(req, bucket), signal))) {
          return reply("rate limited", 429, { "Retry-After": "60" });
        }
        const upstreamPath = scheduler ? path.slice("/scheduler".length) : ppoi ? path.slice("/ppoi".length) : path;
        const upstreamURL = base + upstreamPath + query;
        // Version + purpose + target isolate caches from old relay behavior and
        // configuration changes. No unvalidated caller query reaches cache keys.
        const cacheURL = new URL(req.url);
        cacheURL.pathname = "/.relay-public-v2/" + (ppoi ? "ppoi/" : scheduler ? "scheduler/" : "paymaster/") + encodeURIComponent(upstreamURL);
        cacheURL.search = "";
        const cacheKey = new Request(cacheURL.toString(), { method: "GET" });
        const cache = options.cache === undefined
          ? (globalThis as typeof globalThis & { caches?: { default?: RelayCache } }).caches?.default
          : options.cache;
        const expectedType = gateway ? RESPONSE_TYPE : keyConfig ? CONFIG_TYPE : "application/json";
        let upstream: Response | undefined;
        if (!gateway && cache) {
          try { upstream = await abortable(cache.match(cacheKey), signal); } catch { signal.throwIfAborted(); }
        }
        const cached = !!upstream;
        if (!upstream) {
          const headers = new Headers(STRIP_IP_HEADERS);
          headers.set("Accept", expectedType);
          headers.set("Accept-Encoding", "identity");
          headers.set("User-Agent", "");
          let body: Uint8Array | undefined;
          if (gateway) {
            try {
              body = await readBounded(req.body, req.headers, requestLimit, signal, 413);
            } catch (error) {
              signal.throwIfAborted();
              return fail(error instanceof RelayFailure ? error.status : 400, "invalid request body");
            }
            if (!body.length) return fail(400, "empty request body");
            headers.set("Content-Type", REQUEST_TYPE);
          }
          upstream = await abortable(fetchUpstream(upstreamURL, {
            method: gateway ? "POST" : "GET", headers, body: body as BodyInit | undefined,
            redirect: "manual", credentials: "omit", signal,
          }), signal);
        }
        if (!upstream.ok || upstream.status !== 200 || !contentType(upstream, expectedType)) {
          void upstream.body?.cancel().catch(() => {});
          return fail(502, "invalid gateway response");
        }
        const body = await readBounded(upstream.body, upstream.headers, responseLimit, signal, 502);
        if (!body.length) return fail(502, "empty gateway response");
        const headers: Record<string, string> = { "Content-Type": expectedType, "Cache-Control": "no-store" };
        if (!gateway) headers["Cache-Control"] = publicCacheControl(upstream.headers) ?? "no-store";
        const response = reply(body as BodyInit, 200, headers);
        if (!gateway && !cached && cache && headers["Cache-Control"] !== "no-store") {
          // Hold the buffer reservation while caching; unbounded background
          // writes would bypass the isolate memory budget under load.
          try { await abortable(cache.put(cacheKey, response.clone()), signal); } catch { signal.throwIfAborted(); }
        }
        return response;
      } catch {
        return fail(signal.aborted ? 504 : 502, "gateway unavailable");
      } finally {
        clearTimeout(timer);
        req.signal.removeEventListener("abort", abort);
        activeRequests -= 1;
        reservedBytes -= reservation;
      }
    },
  };
}

export default createRelayHandler();
