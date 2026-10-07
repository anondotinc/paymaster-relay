// Worker tests for the public gas reads. Run from the repository root with
// Node 22 (no Wrangler, network or credentials):
//   node --experimental-strip-types --test worker/test/*.test.mjs
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, test } from "node:test";
import worker, { createRelayHandler } from "../src/index.ts";

const env = { TARGET: "https://api.invalid" };
const FRESH = "public, s-maxage=5, max-age=5";
const realFetch = globalThis.fetch;
let calls;
let stored;
let pending;
let upstream;

const ctx = { waitUntil(promise) { pending.push(promise); } };
const req = (path, init) => new Request("https://relay.invalid" + path, init);
const reply = (status = 200, headers = {}, body = '{"chainId":"42161"}') => new Response(body, {
  status,
  headers: { "Content-Type": "application/json", "Vary": "Origin, accept-encoding", "Set-Cookie": "upstream=secret", "X-Upstream-Trace": "trace", ...headers },
});
async function send(request) {
  const response = await worker.fetch(request, env, ctx);
  await Promise.all(pending.splice(0));
  return response;
}

beforeEach(() => {
  calls = [];
  stored = new Map();
  pending = [];
  upstream = () => reply(200, { "Cache-Control": FRESH });
  globalThis.caches = { default: {
    async match(key) { return stored.get(key.url)?.clone(); },
    async put(key, value) { stored.set(key.url, value); },
  } };
  globalThis.fetch = async (url, init) => { calls.push({ url, init }); return upstream(url, init); };
});
afterEach(() => {
  globalThis.fetch = realFetch;
  delete globalThis.caches;
});

test("forwards only the canonical chain to the fixed API and rebuilds response headers", async () => {
  const identity = {};
  for (const name of ["Authorization", "Cookie", "Origin", "Referer", "User-Agent", "Forwarded", "X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP", "Traceparent", "X-Anon-Client"]) identity[name] = "synthetic-identity";
  for (const [path, target] of [
    ["/api/v1/paymaster/gas-tiers?chainId=42161", "https://api.invalid/api/v1/paymaster/gas-tiers?chainId=42161"],
    ["/api/v1/paymaster/gas-tiers?chain_id=1", "https://api.invalid/api/v1/paymaster/gas-tiers?chainId=1"],
    ["/api/v1/paymaster/gas-history?chainId=56", "https://api.invalid/api/v1/paymaster/gas-history?chainId=56"],
    ["/api/v1/tx/gas-fee/42161", "https://api.invalid/api/v1/tx/gas-fee/42161"],
    ["/api/v1/tx/gas-fee/137", "https://api.invalid/api/v1/tx/gas-fee/137"],
  ]) {
    const response = await send(req(path, { headers: identity }));
    assert.equal(response.status, 200, path);
    assert.equal(calls.at(-1).url, target);
    assert.deepEqual(Object.fromEntries(new Headers(calls.at(-1).init.headers)), { "x-real-ip": "0.0.0.0" });
    assert.equal(calls.at(-1).init.redirect, "manual");
    assert.equal(await response.text(), '{"chainId":"42161"}');
    assert.equal(response.headers.get("Content-Type"), "application/json");
    assert.equal(response.headers.get("Cache-Control"), "public, max-age=5");
    assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
    assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
    assert.equal(response.headers.get("Access-Control-Allow-Methods"), "GET, POST, OPTIONS");
    for (const name of ["Set-Cookie", "Vary", "X-Upstream-Trace", "X-Relay-Expires"]) assert.equal(response.headers.get(name), null, name);
  }
  // Cache keys are canonical: the caller's spelling never reaches them.
  assert.deepEqual([...stored.keys()].sort(), [
    "https://relay.invalid/api/v1/paymaster/gas-history?chainId=56",
    "https://relay.invalid/api/v1/paymaster/gas-tiers?chainId=1",
    "https://relay.invalid/api/v1/paymaster/gas-tiers?chainId=42161",
    "https://relay.invalid/api/v1/tx/gas-fee/137",
    "https://relay.invalid/api/v1/tx/gas-fee/42161",
  ]);
});

test("rejects missing, repeated, unknown or unsupported fields and odd paths without an API call", async () => {
  for (const query of ["", "chainId=999", "chainId=01", "chainId=-1", "chainId=1.0", "chainId=+1", "chainId=", "chainId=1&chainId=1", "chainId=1&chain_id=1", "chainId=&chain_id=1", "chainId=1&priority=fast", "chainId=1&account=secret", "chainId=1;account=secret", "chainId=1&url=https://attacker.invalid", "chainId=1%zz", "chainId=1" + "&".repeat(128)]) {
    for (const path of ["/api/v1/paymaster/gas-tiers?", "/api/v1/paymaster/gas-history?"]) {
      const response = await send(req(path + query));
      assert.equal(response.status, 400, path + query);
      assert.equal(response.headers.get("Cache-Control"), "no-store");
      assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
    }
  }
  for (const path of ["/999", "/01", "/-1", "/secret", "/%34%32161", "/42161%2Fextra", "/42161?blockCount=5", "/42161?chainId=1"]) {
    const response = await send(req("/api/v1/tx/gas-fee" + path));
    assert.equal(response.status, 400, path);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
  }
  for (const path of ["/api/v1/tx/gas-fee/", "/api/v1/tx/gas-fee/42161/", "/api/v1/tx/gas-fee/42161/extra", "/api/v1/tx/gas-fee", "/api/v1/tx/gas-fees/42161", "/api/v1/paymaster/gas-tiers/1", "/api/v1/paymaster/gas-history/1"]) {
    assert.equal((await send(req(path))).status, 404, path);
  }
  for (const path of ["/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/paymaster/gas-history?chainId=1", "/api/v1/tx/gas-fee/1", "/api/v1/tx/gas-fee/999"]) {
    for (const method of ["POST", "HEAD"]) {
      const response = await send(req(path, method === "POST" ? { method, body: "x" } : { method }));
      assert.equal(response.status, 405, `${method} ${path}`);
      assert.equal(response.headers.get("Allow"), "GET, OPTIONS");
      assert.equal(response.headers.get("Cache-Control"), "no-store");
    }
  }
  assert.equal((await send(req("/api/v1/tx/gas-fee/42161/extra", { method: "OPTIONS" }))).status, 404);
  assert.equal(calls.length, 0);
  assert.equal(stored.size, 0);
});

test("serves a cached read without contacting the API", async () => {
  await send(req("/api/v1/tx/gas-fee/1"));
  const response = await send(req("/api/v1/tx/gas-fee/1"));
  assert.equal(calls.length, 1);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
});

// The upstream-response rules live in testdata/gas_read_vectors.json, shared
// with gas_reads_test.go so the Go relay and the Worker keep one rule set.
const vectors = JSON.parse(readFileSync(new URL("../../testdata/gas_read_vectors.json", import.meta.url), "utf8"));

test("shared gas-read vectors", async () => {
  assert.equal(vectors.maxBody, 1 << 20);
  const realNow = Date.now;
  // Both attempts happen at the same instant, so a hit advertises the full
  // remaining freshness ("hits advertise only the freshness left" moves the clock).
  Date.now = () => 1_700_000_000_000;
  try {
    for (const vector of vectors.vectors) {
      stored.clear();
      calls = [];
      upstream = () => {
        const given = vector.upstream;
        if (!given) return reply(200, { "Cache-Control": FRESH }, "{}");
        let body = given.body ?? "x".repeat(given.bodyBytes ?? 0);
        const headers = { "Content-Type": "application/json", "Set-Cookie": "upstream=secret", "X-Upstream-Trace": "trace", ...given.headers };
        if (given.contentLength !== undefined) headers["Content-Length"] = String(given.contentLength);
        if (given.bodyError) {
          const bytes = new TextEncoder().encode(body);
          body = new ReadableStream({ start(controller) { controller.enqueue(bytes); controller.error(new Error("connection reset")); } });
        }
        return new Response(body === "" ? null : body, { status: given.status, headers });
      };
      for (const attempt of [1, 2]) {
        const label = `${vector.name} (#${attempt})`;
        const response = await send(req(vectors.path, { method: vector.method ?? "GET" }));
        assert.equal(response.status, vector.expect.status, label);
        assert.equal(response.headers.get("Cache-Control"), vector.expect.cacheControl, label);
        assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff", label);
        assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*", label);
        for (const name of ["Location", "Set-Cookie", "Vary", "Age", "X-Upstream-Trace", "X-Relay-Expires"]) assert.equal(response.headers.get(name), null, `${label}: ${name}`);
        if (vector.expect.allow) assert.equal(response.headers.get("Allow"), vector.expect.allow, label);
        const text = await response.text();
        if (vector.expect.body !== undefined) assert.equal(text, vector.expect.body, label);
        if (vector.expect.bodyBytes) assert.equal(text.length, vector.expect.bodyBytes, label);
      }
      for (const call of calls) assert.equal(call.url, "https://api.invalid" + vectors.target, vector.name);
      const expected = vector.expect.upstreamCalls ?? (vector.expect.cached ? 1 : 2);
      assert.equal(calls.length, expected, `${vector.name}: upstream calls`);
      if (!vector.expect.cached) assert.equal(stored.size, 0, `${vector.name}: stored`);
    }
  } finally {
    Date.now = realNow;
  }
});

test("hits advertise only the freshness left, never the stored max-age", async () => {
  const realNow = Date.now;
  let now = 1_700_000_000_000;
  Date.now = () => now;
  try {
    upstream = () => reply(200, { "Cache-Control": FRESH, Age: "1" });
    assert.equal((await send(req("/api/v1/tx/gas-fee/1"))).headers.get("Cache-Control"), "public, max-age=4");
    // What the edge cache holds: the remaining lifetime and when it ends.
    const entry = stored.get("https://relay.invalid/api/v1/tx/gas-fee/1");
    assert.equal(entry.headers.get("Cache-Control"), "public, max-age=4");
    assert.equal(entry.headers.get("X-Relay-Expires"), String(now + 4_000));
    now += 2_500;
    const hit = await send(req("/api/v1/tx/gas-fee/1"));
    assert.equal(calls.length, 1);
    assert.equal(hit.headers.get("Cache-Control"), "public, max-age=1");
    assert.equal(hit.headers.get("Age"), null);
    assert.equal(hit.headers.get("X-Relay-Expires"), null);
    assert.equal(hit.headers.get("X-Content-Type-Options"), "nosniff");
    assert.equal(await hit.text(), '{"chainId":"42161"}');
    // Under a second left (or an entry the edge still returns after expiry, or
    // one without an expiry): refetched, never re-served with its old max-age.
    now += 1_000;
    assert.equal((await send(req("/api/v1/tx/gas-fee/1"))).headers.get("Cache-Control"), "public, max-age=4");
    assert.equal(calls.length, 2);
    stored.set("https://relay.invalid/api/v1/tx/gas-fee/1", new Response("{}", { headers: { "Cache-Control": "public, max-age=5", "Content-Type": "application/json" } }));
    await send(req("/api/v1/tx/gas-fee/1"));
    assert.equal(calls.length, 3);
  } finally {
    Date.now = realNow;
  }
});

test("upstream fetches get a 20 s timeout and follow caller cancellation", async () => {
  const delays = [];
  const realSetTimeout = globalThis.setTimeout;
  globalThis.setTimeout = (fn, delay, ...rest) => { delays.push(delay); return realSetTimeout(fn, delay, ...rest); };
  try { await send(req("/api/v1/tx/gas-fee/1")); } finally { globalThis.setTimeout = realSetTimeout; }
  assert.deepEqual(delays, [20_000]);
  assert.ok(calls[0].init.signal instanceof AbortSignal);

  upstream = () => { throw new TypeError("network down"); };
  const failed = await send(req("/api/v1/paymaster/gas-tiers?chainId=1"));
  assert.equal(failed.status, 502);
  assert.equal(failed.headers.get("Cache-Control"), "no-store");
  assert.equal(stored.size, 1);

  let signal;
  const stalled = createRelayHandler({ cache: null, timeoutMs: 20, fetch: (_url, init) => { signal = init.signal; return new Promise(() => {}); } });
  const timedOut = await stalled.fetch(req("/api/v1/tx/gas-fee/1"), env, ctx);
  assert.equal(timedOut.status, 502);
  assert.equal(timedOut.headers.get("Cache-Control"), "no-store");
  assert.equal(signal.aborted, true);
  // A body that stops arriving is cut by the same deadline.
  const slowBody = createRelayHandler({ cache: null, timeoutMs: 20, fetch: async () => new Response(new ReadableStream(), { headers: { "Content-Type": "application/json", "Cache-Control": FRESH } }) });
  assert.equal((await slowBody.fetch(req("/api/v1/tx/gas-fee/1"), env, ctx)).status, 502);
  const controller = new AbortController();
  const cancelled = createRelayHandler({ cache: null, fetch: (_url, init) => { signal = init.signal; controller.abort(); return new Promise(() => {}); } });
  assert.equal((await cancelled.fetch(req("/api/v1/tx/gas-fee/1", { signal: controller.signal }), env, ctx)).status, 502);
  assert.equal(signal.aborted, true);
});

test("gas reads have their own per-source bucket: 240 per minute, cache hits included", async () => {
  const handler = createRelayHandler({ cache: null, fetch: async (url) => url.endsWith("/gateway")
    ? new Response(new Uint8Array([1]), { headers: { "Content-Type": "message/ohttp-res" } })
    : reply(200, { "Cache-Control": FRESH }) });
  const from = (ip, path = "/api/v1/tx/gas-fee/1") => req(path, { headers: { "CF-Connecting-IP": ip } });
  for (let i = 0; i < 240; i += 1) assert.equal((await handler.fetch(from("192.0.2.1"), env, ctx)).status, 200, `read ${i + 1}`);
  const limited = await handler.fetch(from("192.0.2.1", "/api/v1/paymaster/gas-tiers?chainId=1"), env, ctx);
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get("Retry-After"), "60");
  assert.equal(limited.headers.get("Cache-Control"), "no-store");
  assert.equal(limited.headers.get("Access-Control-Allow-Origin"), "*");
  assert.equal((await handler.fetch(from("192.0.2.2"), env, ctx)).status, 200);
  // The gas bucket never consumes the sealed paymaster budget.
  const sealed = req("/gateway", { method: "POST", headers: { "Content-Type": "message/ohttp-req", "CF-Connecting-IP": "192.0.2.1" }, body: new Uint8Array([1]), duplex: "half" });
  assert.equal((await handler.fetch(sealed, env, ctx)).status, 200);
});

test("answers browser preflights for the gas reads", async () => {
  for (const path of ["/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1"]) {
    const response = await send(req(path, { method: "OPTIONS", headers: { Origin: "https://app.invalid", "Access-Control-Request-Method": "GET" } }));
    assert.equal(response.status, 204);
    assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
  }
  assert.equal(calls.length, 0);
});
