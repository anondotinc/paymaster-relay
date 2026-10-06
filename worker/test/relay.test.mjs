import assert from "node:assert/strict";
import { mock, test } from "node:test";
import { createRelayHandler } from "../src/index.ts";

const CONFIG_TYPE = "application/ohttp-keys-signed";
const REQUEST_TYPE = "message/ohttp-req";
const RESPONSE_TYPE = "message/ohttp-res";
const MAX = 1 << 20;
const env = { TARGET: "https://paymaster.invalid", SCHEDULER_TARGET: "https://scheduler.invalid/ohttp" };
const ctx = { waitUntil(promise) { void promise.catch(() => {}); } };
const req = (path, options) => new Request("https://relay.invalid" + path, options);
const post = (path = "/gateway", body = new Uint8Array([1, 2, 3]), options = {}) => req(path, {
  method: "POST", headers: { "Content-Type": REQUEST_TYPE }, body, duplex: "half", ...options,
});
const response = (body = new Uint8Array([4, 5, 6]), type = RESPONSE_TYPE, options = {}) => new Response(body, {
  status: 200, ...options, headers: { "Content-Type": type, ...options.headers },
});
function fixture(responder = () => response(), options = {}) {
  const calls = [];
  const handler = createRelayHandler({
    cache: null,
    fetch: async (url, init) => { calls.push({ url, init }); return responder(url, init); },
    ...options,
  });
  return { calls, handler, send: (request, config = env) => handler.fetch(request, config, ctx) };
}

test("routes sealed bodies and configs to distinct fixed gateways", async () => {
  const { send, calls } = fixture(url => response(undefined, url.endsWith("ohttp-configs") ? CONFIG_TYPE : RESPONSE_TYPE));
  for (const [path, target, config] of [
    ["/gateway", "https://paymaster.invalid/gateway", false],
    ["/ohttp-configs", "https://paymaster.invalid/ohttp-configs", true],
    ["/scheduler/gateway", "https://scheduler.invalid/ohttp/gateway", false],
    ["/scheduler/ohttp-configs", "https://scheduler.invalid/ohttp/ohttp-configs", true],
  ]) {
    const result = await send(config ? req(path) : post(path));
    assert.equal(result.status, 200);
    assert.equal(calls.at(-1).url, target);
    assert.equal(result.headers.get("Content-Type"), config ? CONFIG_TYPE : RESPONSE_TYPE);
    if (!config) assert.equal(result.headers.get("Cache-Control"), "no-store");
  }
});

test("handler construction performs no request-context crypto work", () => {
  const stub = mock.method(crypto, "getRandomValues", () => { throw new Error("outside request context"); });
  try { assert.doesNotThrow(() => createRelayHandler()); } finally { stub.mock.restore(); }
});

test("scheduler remains unavailable without a distinct valid target and never falls back", async () => {
  for (const target of [undefined, "", env.TARGET, env.TARGET + "/", "https://user:pass@host.invalid", "https://host.invalid?key=x", "https://host.invalid/#fragment", "https://host.invalid/%2e%2e", "https://host.invalid/../wrong", "https://host.invalid\\wrong"]) {
    const { send, calls } = fixture();
    for (const request of [post("/scheduler/gateway"), req("/scheduler/ohttp-configs")]) {
      assert.equal((await send(request, { TARGET: env.TARGET, SCHEDULER_TARGET: target })).status, 503);
    }
    assert.equal(calls.length, 0);
  }
});

test("forwards no caller headers and only allowlisted response headers", async () => {
  const { send, calls } = fixture(() => response(undefined, RESPONSE_TYPE, { headers: {
    "Set-Cookie": "tracking=id", "Location": "https://tracker.invalid", "X-Identifier": "identity", "Cache-Control": "public, max-age=600",
  } }));
  const headers = { "Content-Type": REQUEST_TYPE };
  for (const header of ["Authorization", "Cookie", "Origin", "Referer", "User-Agent", "Forwarded", "X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP", "Traceparent", "Baggage", "X-Anon-Client"]) headers[header] = "synthetic-identity";
  const result = await send(post("/gateway", undefined, { headers }));
  assert.equal(result.status, 200);
  assert.deepEqual(Object.fromEntries(calls[0].init.headers), {
    accept: RESPONSE_TYPE, "accept-encoding": "identity", "content-type": REQUEST_TYPE,
    "user-agent": "", "x-real-ip": "0.0.0.0",
  });
  assert.equal(calls[0].init.redirect, "manual");
  assert.equal(calls[0].init.credentials, "omit");
  assert.equal(result.headers.get("Set-Cookie"), null);
  assert.equal(result.headers.get("Location"), null);
  assert.equal(result.headers.get("X-Identifier"), null);
  assert.equal(result.headers.get("Cache-Control"), "no-store");
  assert.equal(result.headers.get("Access-Control-Allow-Origin"), "*");
});

test("canonicalizes only protobuf public read fields", async () => {
  const { send, calls } = fixture(() => response("{}", "application/json; charset=utf-8"));
  assert.equal((await send(req("/api/v1/paymaster/gas-quote?priority=FAST&chain_id=42161"))).status, 200);
  assert.equal(calls[0].url, env.TARGET + "/api/v1/paymaster/gas-quote?chainId=42161&priority=fast");
  assert.equal((await send(req("/api/v1/paymaster/gas-quote?chainId=1&priority="))).status, 200);
  assert.equal(calls[1].url, env.TARGET + "/api/v1/paymaster/gas-quote?chainId=1");
  assert.equal((await send(req("/api/v1/paymaster/supported-tokens?chainId=56"))).status, 200);
});

test("rejects unknown, repeated, missing, or invalid public fields without an upstream call", async () => {
  const { send, calls } = fixture();
  for (const query of ["", "chainId=1&address=identity", "chainId=1&chainId=1", "chain_id=1&chainId=1", "chainId=999", "chainId=-1", "chainId=1.0", "chainId=01", "chainId=1&priority=fast&priority=slow", "chainId=1&priority=tracking", "chainId=1&url=https://evil.invalid", "chainId=1;address=identity", "chainId=1" + "&".repeat(128)]) {
    assert.equal((await send(req("/api/v1/paymaster/gas-quote?" + query))).status, 400, query);
  }
  assert.equal((await send(req("/api/v1/paymaster/supported-tokens?chainId=1&priority=fast"))).status, 400);
  for (const path of ["/gateway", "/ohttp-configs", "/scheduler/gateway", "/scheduler/ohttp-configs", "/health"]) {
    assert.equal((await send(req(path + "?address=identity"))).status, 400);
  }
  assert.equal(calls.length, 0);
});

test("rejects encoded request bodies before forwarding and accepts identity encoding", async () => {
  const { send, calls } = fixture();
  for (const encoding of ["gzip", "br", "deflate"]) {
    assert.equal((await send(post("/gateway", undefined, { headers: {
      "Content-Type": REQUEST_TYPE, "Content-Encoding": encoding,
    } }))).status, 415);
  }
  assert.equal(calls.length, 0);
  assert.equal((await send(post("/gateway", undefined, { headers: {
    "Content-Type": REQUEST_TYPE, "Content-Encoding": "identity",
  } }))).status, 200);
  assert.equal(calls[0].init.headers.get("Content-Encoding"), null);
});

test("requires exact routes and methods, including preflights", async () => {
  const { send, calls } = fixture();
  for (const path of ["/unknown", "/gateway/", "/scheduler/gateway/", "/%67ateway", "/scheduler/api/v1/paymaster/gas-quote"]) {
    assert.equal((await send(req(path))).status, 404);
    assert.equal((await send(req(path, { method: "OPTIONS" }))).status, 404);
  }
  assert.equal((await send(req("/gateway"))).status, 405);
  assert.equal((await send(post("/health"))).status, 405);
  assert.equal((await send(req("/gateway", { method: "OPTIONS" }))).status, 204);
  assert.equal((await send(req("/health"))).status, 200);
  assert.equal(calls.length, 0);
});

test("bounds both unannounced and announced uploads while allowing the scheduler envelope", async () => {
  const { send, calls } = fixture();
  assert.equal((await send(post("/gateway", new Uint8Array(MAX)))).status, 200);
  assert.equal((await send(post("/gateway", new Uint8Array(MAX + 1)))).status, 413);
  assert.equal((await send(post("/scheduler/gateway", new Uint8Array(4 * MAX)))).status, 200);
  assert.equal((await send(post("/scheduler/gateway", new Uint8Array(4 * MAX + 1)))).status, 413);
  assert.equal((await send(post("/gateway", new Uint8Array(2), { headers: { "Content-Type": REQUEST_TYPE, "Content-Length": String(MAX + 1) } }))).status, 413);
  assert.equal((await send(post("/gateway", new Uint8Array(2), { headers: { "Content-Type": REQUEST_TYPE, "Content-Length": "3" } }))).status, 400);
  assert.equal((await send(post("/gateway", new Uint8Array()))).status, 400);
  assert.equal(calls.length, 2);
});

test("stops oversized upload streams before consuming the entire stream", async () => {
  let pulled = 0;
  let cancelled = false;
  const stream = new ReadableStream({
    pull(controller) { pulled += 1; controller.enqueue(new Uint8Array(MAX / 2)); },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 });
  const { send, calls } = fixture();
  assert.equal((await send(post("/gateway", stream))).status, 413);
  assert.equal(cancelled, true);
  assert.ok(pulled <= 4);
  assert.equal(calls.length, 0);
});

test("rejects non-success responses, redirects, wrong MIME, encodings and truncated/oversized bodies", async () => {
  for (const makeResponse of [
    () => response(undefined, RESPONSE_TYPE, { status: 307, headers: { Location: "https://evil.invalid" } }),
    () => response(undefined, RESPONSE_TYPE, { status: 500 }),
    () => response("oops", "text/plain"),
    () => response(undefined, RESPONSE_TYPE, { headers: { "Content-Encoding": "gzip" } }),
    () => response(undefined, RESPONSE_TYPE, { headers: { "Content-Length": "100" } }),
    () => response(new Uint8Array(MAX + 1)),
    () => response(new ReadableStream({ start(controller) { controller.error(new Error("truncated")); } })),
  ]) {
    const { send, calls } = fixture(makeResponse);
    assert.equal((await send(post())).status, 502);
    assert.equal(calls.length, 1);
  }
  const { send } = fixture(() => response(undefined, "application/json"));
  assert.equal((await send(req("/ohttp-configs"))).status, 502);
});

test("stream limits apply to public GET reads and signed key configs", async () => {
  for (const path of ["/ohttp-configs", "/scheduler/ohttp-configs", "/api/v1/paymaster/gas-quote?chainId=1"]) {
    const { send } = fixture(() => response(new Uint8Array(MAX + 1), path.includes("ohttp-configs") ? CONFIG_TYPE : "application/json"));
    assert.equal((await send(req(path))).status, 502);
  }
});

test("reserves concurrency before reading bodies and releases it after timeout", async () => {
  let reading;
  const entered = new Promise(resolve => { reading = resolve; });
  const stream = new ReadableStream({ pull() { reading(); } }, { highWaterMark: 0 });
  const { send, calls } = fixture(undefined, { maxConcurrency: 1, timeoutMs: 40 });
  const pending = send(post("/gateway", stream));
  await entered;
  assert.equal((await send(post())).status, 503);
  assert.equal((await pending).status, 504);
  assert.equal((await send(post())).status, 200);
  assert.equal(calls.length, 1);
});

test("times out upstream fetches and streams, and propagates caller cancellation", async () => {
  const stalledFetch = fixture(() => new Promise(() => {}), { timeoutMs: 15 });
  assert.equal((await stalledFetch.send(req("/ohttp-configs"))).status, 504);
  let cancelled = false;
  const stalledBody = fixture(() => response(new ReadableStream({ cancel() { cancelled = true; } })), { timeoutMs: 15 });
  assert.equal((await stalledBody.send(post())).status, 504);
  assert.equal(cancelled, true);
  const controller = new AbortController();
  let upstreamSignal;
  const cancelledFetch = fixture((_url, init) => {
    upstreamSignal = init.signal;
    controller.abort();
    return new Promise(() => {});
  });
  assert.equal((await cancelledFetch.send(post("/gateway", undefined, { signal: controller.signal }))).status, 504);
  assert.equal(upstreamSignal.aborted, true);
});

test("scheduler uploads are additionally limited by the isolate buffer budget", async () => {
  const { send, calls } = fixture(undefined, { timeoutMs: 35 });
  const pending = [
    send(post("/scheduler/gateway", new ReadableStream())),
    send(post("/scheduler/gateway", new ReadableStream())),
  ];
  assert.equal((await send(post("/scheduler/gateway"))).status, 503);
  for (const result of await Promise.all(pending)) assert.equal(result.status, 504);
  assert.equal((await send(post("/scheduler/gateway"))).status, 200);
  assert.equal(calls.length, 1);
});

test("public cache is purpose/target scoped and never stores sealed responses", async () => {
  const stored = new Map();
  const cache = {
    async match(key) { return stored.get(key.url)?.clone(); },
    async put(key, value) { stored.set(key.url, value); },
  };
  const { send, calls } = fixture(url => response(undefined, url.endsWith("ohttp-configs") ? CONFIG_TYPE : RESPONSE_TYPE, { headers: { "Cache-Control": "public, max-age=60", "Set-Cookie": "identity=1" } }), { cache });
  await send(req("/ohttp-configs"));
  await send(req("/ohttp-configs"));
  assert.equal(calls.length, 1);
  await send(req("/scheduler/ohttp-configs"));
  assert.equal(calls.length, 2);
  await send(req("/ohttp-configs"), { ...env, TARGET: "https://other-paymaster.invalid" });
  assert.equal(calls.length, 3);
  await send(post());
  await send(post());
  assert.equal(calls.length, 5);
  assert.equal(stored.size, 3);
  for (const value of stored.values()) assert.equal(value.headers.get("Set-Cookie"), null);
});

test("honors non-cacheable public reads and reduces TTL by upstream Age", async () => {
  for (const cc of ["", "no-store", "private, max-age=60", "no-cache, max-age=60", 'private="secret", max-age=60', 'private = "secret", max-age=60', "max-age=0"]) {
    let puts = 0;
    const { send } = fixture(() => response(undefined, CONFIG_TYPE, { headers: { "Cache-Control": cc } }), {
      cache: { async match() {}, async put() { puts += 1; } },
    });
    assert.equal((await send(req("/ohttp-configs"))).headers.get("Cache-Control"), "no-store");
    assert.equal(puts, 0);
  }
  const { send } = fixture(() => response(undefined, CONFIG_TYPE, { headers: { "Cache-Control": "max-age=600, s-maxage=60", Age: "20" } }));
  assert.equal((await send(req("/ohttp-configs"))).headers.get("Cache-Control"), "public, max-age=40");
  let puts = 0;
  const varying = fixture(() => response(undefined, CONFIG_TYPE, { headers: { "Cache-Control": "max-age=60", Vary: "*" } }), {
    cache: { async match() {}, async put() { puts += 1; } },
  });
  assert.equal((await varying.send(req("/ohttp-configs"))).headers.get("Cache-Control"), "no-store");
  assert.equal(puts, 0);
});

test("applies a bounded per-source window without logging identity", async () => {
  const { send, calls } = fixture();
  for (let i = 0; i < 60; i += 1) assert.equal((await send(post())).status, 200);
  const limited = await send(post());
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get("Retry-After"), "60");
  assert.equal(calls.length, 60);
});
