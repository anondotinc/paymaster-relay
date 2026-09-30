// Worker tests for the public gas reads. Run from the repository root with
// Node 22 (no Wrangler, network or credentials):
//   node --experimental-strip-types --test worker/test/*.test.mjs
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import worker from "../src/index.ts";

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
    for (const name of ["Set-Cookie", "Vary", "X-Upstream-Trace"]) assert.equal(response.headers.get(name), null, name);
  }
  // Cache keys are canonical: the caller's spelling never reaches them.
  assert.deepEqual([...stored.keys()].sort(), [
    "https://relay.invalid/api/v1/paymaster/gas-tiers?chainId=1",
    "https://relay.invalid/api/v1/paymaster/gas-tiers?chainId=42161",
    "https://relay.invalid/api/v1/tx/gas-fee/137",
    "https://relay.invalid/api/v1/tx/gas-fee/42161",
  ]);
});

test("rejects missing, repeated, unknown or unsupported fields and odd paths without an API call", async () => {
  for (const query of ["", "chainId=999", "chainId=01", "chainId=-1", "chainId=1.0", "chainId=+1", "chainId=", "chainId=1&chainId=1", "chainId=1&chain_id=1", "chainId=&chain_id=1", "chainId=1&priority=fast", "chainId=1&account=secret", "chainId=1;account=secret", "chainId=1&url=https://attacker.invalid", "chainId=1%zz", "chainId=1" + "&".repeat(128)]) {
    const response = await send(req("/api/v1/paymaster/gas-tiers?" + query));
    assert.equal(response.status, 400, query);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
  }
  for (const path of ["/999", "/01", "/-1", "/secret", "/%34%32161", "/42161%2Fextra", "/42161?blockCount=5", "/42161?chainId=1"]) {
    const response = await send(req("/api/v1/tx/gas-fee" + path));
    assert.equal(response.status, 400, path);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
  }
  for (const path of ["/api/v1/tx/gas-fee/", "/api/v1/tx/gas-fee/42161/", "/api/v1/tx/gas-fee/42161/extra", "/api/v1/tx/gas-fee", "/api/v1/tx/gas-fees/42161", "/api/v1/paymaster/gas-tiers/1"]) {
    assert.equal((await send(req(path))).status, 404, path);
  }
  for (const path of ["/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1"]) {
    assert.equal((await send(req(path, { method: "POST", body: "x" }))).status, 404, path);
  }
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

test("never extends freshness: upstream Age counts, stale and uncacheable answers are not stored", async () => {
  upstream = () => reply(200, { "Cache-Control": FRESH, Age: "3" });
  assert.equal((await send(req("/api/v1/tx/gas-fee/1"))).headers.get("Cache-Control"), "public, max-age=2");
  assert.equal(stored.get("https://relay.invalid/api/v1/tx/gas-fee/1").headers.get("Cache-Control"), "public, max-age=2");
  for (const headers of [
    { "Cache-Control": FRESH, Age: "5" },
    { "Cache-Control": FRESH, Age: "60" },
    { "Cache-Control": FRESH, Age: "x" },
    { "Cache-Control": FRESH, Age: "-1" },
    { "Cache-Control": FRESH, Vary: "*" },
    { "Cache-Control": "no-store" },
    { "Cache-Control": "no-cache, max-age=5" },
    { "Cache-Control": "private, max-age=5" },
    { "Cache-Control": "max-age=0" },
    {},
  ]) {
    stored.clear();
    upstream = () => reply(200, headers);
    const response = await send(req("/api/v1/paymaster/gas-tiers?chainId=1"));
    assert.equal(response.status, 200, JSON.stringify(headers));
    assert.equal(response.headers.get("Cache-Control"), "no-store", JSON.stringify(headers));
    assert.equal(stored.size, 0, JSON.stringify(headers));
  }
  // s-maxage (the shared-cache lifetime) wins over a longer max-age.
  stored.clear();
  upstream = () => reply(200, { "Cache-Control": "public, max-age=600, s-maxage=5" });
  assert.equal((await send(req("/api/v1/tx/gas-fee/1"))).headers.get("Cache-Control"), "public, max-age=5");
});

test("passes API errors through uncached and uncacheable, and never follows redirects", async () => {
  for (const status of [400, 404, 500, 503]) {
    stored.clear();
    upstream = () => reply(status, { "Cache-Control": "public, s-maxage=60, max-age=60" }, '{"code":"error"}');
    for (const path of ["/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1"]) {
      const response = await send(req(path));
      assert.equal(response.status, status);
      assert.equal(response.headers.get("Cache-Control"), "no-store");
      assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
      assert.equal(await response.text(), '{"code":"error"}');
    }
    assert.equal(stored.size, 0);
  }
  for (const status of [301, 302, 307, 308]) {
    upstream = () => new Response(null, { status, headers: { Location: "https://attacker.invalid", "Cache-Control": "max-age=60" } });
    const response = await send(req("/api/v1/tx/gas-fee/1"));
    assert.equal(response.status, 502);
    assert.equal(response.headers.get("Location"), null);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
  }
  upstream = () => { throw new TypeError("network down"); };
  const failed = await send(req("/api/v1/paymaster/gas-tiers?chainId=1"));
  assert.equal(failed.status, 502);
  assert.equal(failed.headers.get("Cache-Control"), "no-store");
  assert.equal(stored.size, 0);
});

test("answers browser preflights for the gas reads", async () => {
  for (const path of ["/api/v1/paymaster/gas-tiers?chainId=1", "/api/v1/tx/gas-fee/1"]) {
    const response = await send(req(path, { method: "OPTIONS", headers: { Origin: "https://app.invalid", "Access-Control-Request-Method": "GET" } }));
    assert.equal(response.status, 204);
    assert.equal(response.headers.get("Access-Control-Allow-Origin"), "*");
  }
  assert.equal(calls.length, 0);
});
