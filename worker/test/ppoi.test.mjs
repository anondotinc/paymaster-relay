import assert from "node:assert/strict";
import { test } from "node:test";
import { MAX_BUFFERED_WORK, ROUTE_LIMITS, bufferReservation, createRelayHandler } from "../src/index.ts";

const CONFIG_TYPE = "application/ohttp-keys-signed";
const REQUEST_TYPE = "message/ohttp-req";
const RESPONSE_TYPE = "message/ohttp-res";
const env = {
  TARGET: "https://paymaster.invalid",
  SCHEDULER_TARGET: "https://scheduler.invalid/ohttp",
  PPOI_TARGET: "https://proxy.invalid/ohttp",
};
const ctx = { waitUntil(promise) { void promise.catch(() => {}); } };
const req = (path, options) => new Request("https://relay.invalid" + path, options);
const post = (path, ip = "192.0.2.1") => req(path, {
  method: "POST", headers: { "Content-Type": REQUEST_TYPE, "CF-Connecting-IP": ip }, body: new Uint8Array([1, 2, 3]), duplex: "half",
});
const get = (path, ip = "192.0.2.1") => req(path, { headers: { "CF-Connecting-IP": ip } });
const response = (type = RESPONSE_TYPE, options = {}) => new Response(new Uint8Array([4, 5, 6]), {
  status: 200, ...options, headers: { "Content-Type": type, ...options.headers },
});
function fixture(options = {}) {
  const calls = [];
  const handler = createRelayHandler({
    cache: null,
    fetch: async (url, init) => {
      calls.push({ url, init });
      return url.endsWith("ohttp-configs") ? response(CONFIG_TYPE, { headers: { "Cache-Control": "public, max-age=60" } }) : response();
    },
    ...options,
  });
  return { calls, send: (request, config = env) => handler.fetch(request, config, ctx) };
}

test("ppoi routes forward to the PPOI target, keyless and unlogged", async () => {
  const { send, calls } = fixture();
  const configs = await send(get("/ppoi/ohttp-configs"));
  assert.equal(configs.status, 200);
  assert.equal(configs.headers.get("Content-Type"), CONFIG_TYPE);
  assert.equal(configs.headers.get("Cache-Control"), "public, max-age=60");
  assert.equal(calls.at(-1).url, "https://proxy.invalid/ohttp/ohttp-configs");
  const sealed = await send(post("/ppoi/gateway"));
  assert.equal(sealed.status, 200);
  assert.equal(sealed.headers.get("Content-Type"), RESPONSE_TYPE);
  assert.equal(sealed.headers.get("Cache-Control"), "no-store");
  const call = calls.at(-1);
  assert.equal(call.url, "https://proxy.invalid/ohttp/gateway");
  assert.equal(call.init.method, "POST");
  assert.equal(call.init.redirect, "manual");
  assert.equal(call.init.credentials, "omit");
  assert.deepEqual(Object.fromEntries(new Headers(call.init.headers)), {
    "accept": RESPONSE_TYPE, "accept-encoding": "identity", "content-type": REQUEST_TYPE, "user-agent": "", "x-real-ip": "0.0.0.0",
  });
  assert.equal(call.init.body.byteLength, 3);
  // Paymaster and scheduler routes still reach their own targets.
  await send(post("/gateway"));
  assert.equal(calls.at(-1).url, "https://paymaster.invalid/gateway");
  await send(post("/scheduler/gateway"));
  assert.equal(calls.at(-1).url, "https://scheduler.invalid/ohttp/gateway");
  // Wrong methods and a query on a ppoi route never dispatch.
  const before = calls.length;
  assert.equal((await send(post("/ppoi/ohttp-configs"))).status, 405);
  assert.equal((await send(get("/ppoi/gateway"))).status, 405);
  assert.equal((await send(get("/ppoi/ohttp-configs?x=1"))).status, 400);
  assert.equal((await send(get("/ppoi/other"))).status, 404);
  assert.equal(calls.length, before);
});

test("ppoi key config is cached separately from the paymaster's", async () => {
  const stored = new Map();
  const cache = { async match(key) { return stored.get(key.url)?.clone(); }, async put(key, value) { stored.set(key.url, value); } };
  const { send, calls } = fixture({ cache });
  await send(get("/ppoi/ohttp-configs"));
  await send(get("/ppoi/ohttp-configs"));
  assert.equal(calls.length, 1);
  await send(get("/ohttp-configs"));
  assert.equal(calls.length, 2);
  assert.equal(stored.size, 2);
  assert.ok([...stored.keys()].some(key => key.includes("/ppoi/")));
});

test("ppoi routes are 404 when PPOI_TARGET is unset", async () => {
  const { send, calls } = fixture();
  for (const unset of [{ ...env, PPOI_TARGET: undefined }, { ...env, PPOI_TARGET: "" }]) {
    for (const request of [get("/ppoi/ohttp-configs"), post("/ppoi/gateway"), req("/ppoi/gateway", { method: "OPTIONS" })]) {
      assert.equal((await send(request, unset)).status, 404);
    }
  }
  assert.equal(calls.length, 0);
});

test("ppoi refuses to share a base with the paymaster or scheduler", async () => {
  const { send, calls } = fixture();
  for (const shared of [
    { ...env, PPOI_TARGET: "https://paymaster.invalid" },
    { ...env, PPOI_TARGET: "https://paymaster.invalid/" },
    { ...env, PPOI_TARGET: "https://scheduler.invalid/ohttp/" },
    { ...env, PPOI_TARGET: "https://proxy.invalid/ohttp?x=1" },
    { ...env, PPOI_TARGET: "ftp://proxy.invalid" },
  ]) {
    assert.equal((await send(post("/ppoi/gateway"), shared)).status, 503, shared.PPOI_TARGET);
    assert.equal((await send(get("/ppoi/ohttp-configs"), shared)).status, 503, shared.PPOI_TARGET);
  }
  assert.equal(calls.length, 0);
});

test("per-route buckets: paymaster 60, scheduler 60, ppoi 180 per source per minute", async () => {
  const { send, calls } = fixture();
  for (let i = 0; i < 60; i += 1) assert.equal((await send(post("/gateway"))).status, 200);
  const limited = await send(post("/gateway"));
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get("Retry-After"), "60");
  // Exhausting the paymaster bucket leaves ppoi and scheduler untouched.
  assert.equal((await send(post("/ppoi/gateway"))).status, 200);
  assert.equal((await send(post("/scheduler/gateway"))).status, 200);
  for (let i = 1; i < 180; i += 1) assert.equal((await send(post("/ppoi/gateway"))).status, 200, `ppoi ${i + 1}`);
  assert.equal((await send(post("/ppoi/gateway"))).status, 429);
  // The ppoi config read shares the ppoi bucket, so it is limited too.
  assert.equal((await send(get("/ppoi/ohttp-configs"))).status, 429);
  // Another source keeps its own window in every bucket.
  assert.equal((await send(post("/ppoi/gateway", "192.0.2.9"))).status, 200);

  // Heavy ppoi traffic never consumes a (fresh) paymaster budget.
  const second = fixture();
  for (let i = 0; i < 180; i += 1) await second.send(post("/ppoi/gateway"));
  assert.equal((await second.send(post("/gateway"))).status, 200);
  // 180 from the limited source plus the one from the other source reached the gateway.
  assert.equal(calls.filter(call => call.url === "https://proxy.invalid/ohttp/gateway").length, 181);
});

test("sealed routes get a 40 s deadline; configs and public reads keep 20 s", async () => {
  const delays = [];
  const realSetTimeout = globalThis.setTimeout;
  globalThis.setTimeout = (fn, delay, ...rest) => { delays.push(delay); return realSetTimeout(fn, delay, ...rest); };
  try {
    const { send } = fixture();
    await send(post("/ppoi/gateway"));
    await send(post("/gateway"));
    await send(post("/scheduler/gateway"));
    await send(get("/ppoi/ohttp-configs"));
    await send(get("/ohttp-configs"));
  } finally {
    globalThis.setTimeout = realSetTimeout;
  }
  assert.deepEqual(delays, [40_000, 40_000, 40_000, 20_000, 20_000]);
});

// ---- isolate buffer budget (incident 2026-10-06) ---------------------------
// A mobile start fires ~8 concurrent sealed POSTs; the relay answered 4 of them
// "503 relay busy" because every sealed request reserved 5 MiB of a 32 MiB
// per-isolate budget (6 requests). PPOI now reserves from its own, small caps.

const sealedBody = (path, body, headers = {}) => req(path, {
  method: "POST", headers: { "Content-Type": REQUEST_TYPE, "CF-Connecting-IP": "192.0.2.1", ...headers }, body, duplex: "half",
});
const KiB = 1 << 10;
const MiB = 1 << 20;
const pending = (calls, count) => new Promise((resolve, reject) => {
  const deadline = Date.now() + 5_000;
  const poll = () => calls.length >= count ? resolve() : Date.now() > deadline
    ? reject(new Error(`only ${calls.length}/${count} requests reached the upstream`))
    : setTimeout(poll, 1);
  poll();
});
/** A fixture whose upstream answers only when released, like a slow gateway. */
function slowFixture(options = {}) {
  const releases = [];
  const handler = createRelayHandler({
    cache: null,
    fetch: (url, init) => new Promise(resolve => { releases.push(() => resolve(response())); calls.push({ url, init }); }),
    ...options,
  });
  const calls = [];
  return { calls, releases, send: (request, config = env) => handler.fetch(request, config, ctx) };
}

test("reservation per request and the resulting per-isolate capacity", () => {
  const capacity = (bucket, gateway) => Math.floor(MAX_BUFFERED_WORK / bufferReservation(bucket, gateway));
  // Paymaster and scheduler are unchanged: 2 * request + 3 * response.
  assert.equal(bufferReservation("paymaster", true), 5 * MiB);
  assert.equal(bufferReservation("scheduler", true), 11 * MiB);
  assert.equal(bufferReservation("paymaster", false), 3 * MiB);
  assert.equal(capacity("paymaster", true), 6);
  assert.equal(capacity("scheduler", true), 2);
  // PPOI: 2 * 64 KiB + 3 * 256 KiB.
  assert.deepEqual(ROUTE_LIMITS.ppoi, { request: 64 * KiB, response: 256 * KiB });
  assert.equal(bufferReservation("ppoi", true), 896 * KiB);
  assert.equal(bufferReservation("ppoi", false), 768 * KiB);
  assert.equal(capacity("ppoi", true), 36);
  assert.ok(capacity("ppoi", true) >= 32);
  // The 2x / 3x multipliers still apply, and nothing grew.
  assert.equal(MAX_BUFFERED_WORK, 32 * MiB);
  assert.equal(ROUTE_LIMITS.scheduler.request, 4 * MiB);
});

test("32+ concurrent sealed ppoi requests with slow upstreams are all admitted; the budget still trips beyond it", async () => {
  const capacity = Math.floor(MAX_BUFFERED_WORK / bufferReservation("ppoi", true));
  const { send, calls, releases } = slowFixture();
  // Incident shape first: 8 concurrent sealed POSTs from one client.
  // (One source stays well inside the 180/min ppoi bucket.)
  const inflight = [];
  for (let i = 0; i < capacity; i += 1) inflight.push(send(post("/ppoi/gateway")));
  await pending(calls, capacity);
  assert.ok(calls.length >= 32, `only ${calls.length} admitted`);
  // Every admitted request is in flight at the gateway; the next one is the first refused.
  const refused = await send(post("/ppoi/gateway", "192.0.2.77"));
  assert.equal(refused.status, 503);
  assert.equal(await refused.text(), "relay busy");
  assert.equal(calls.length, capacity);
  for (const release of releases) release();
  for (const result of await Promise.all(inflight)) assert.equal(result.status, 200);
  // Capacity is returned after completion.
  const again = send(post("/ppoi/gateway"));
  await pending(calls, capacity + 1);
  releases.at(-1)();
  assert.equal((await again).status, 200);
});

test("slow ppoi uploads hold their reservation, so they cannot evade the cap", async () => {
  const capacity = Math.floor(MAX_BUFFERED_WORK / bufferReservation("ppoi", true));
  const { send, calls } = slowFixture({ timeoutMs: 60 });
  const stalled = () => sealedBody("/ppoi/gateway", new ReadableStream());
  const uploads = Array.from({ length: capacity }, () => send(stalled()));
  assert.equal((await send(post("/ppoi/gateway", "192.0.2.9"))).status, 503);
  for (const result of await Promise.all(uploads)) assert.equal(result.status, 504);
  assert.equal(calls.length, 0);
});

test("ppoi bodies above the route caps fail cleanly; bodies at the cap pass", async () => {
  const { request, response: responseLimit } = ROUTE_LIMITS.ppoi;
  const body = (size) => sealedBody("/ppoi/gateway", new Uint8Array(size));
  const { send, calls } = fixture();
  assert.equal((await send(body(request))).status, 200);
  assert.equal((await send(body(request + 1))).status, 413);
  const announced = sealedBody("/ppoi/gateway", new Uint8Array(2), { "Content-Length": String(request + 1) });
  assert.equal((await send(announced)).status, 413);
  assert.equal(calls.length, 1);
  // The paymaster route keeps its 1 MiB request cap.
  assert.equal((await send(sealedBody("/gateway", new Uint8Array(request + 1)))).status, 200);

  const big = (size, type = RESPONSE_TYPE) => new Response(new Uint8Array(size), { status: 200, headers: { "Content-Type": type } });
  const atCap = fixture({ fetch: async () => big(responseLimit) });
  assert.equal((await atCap.send(post("/ppoi/gateway"))).status, 200);
  const over = fixture({ fetch: async () => big(responseLimit + 1) });
  assert.equal((await over.send(post("/ppoi/gateway"))).status, 502);
  const announcedOver = fixture({ fetch: async () => new Response(new Uint8Array(4), { status: 200, headers: { "Content-Type": RESPONSE_TYPE, "Content-Length": String(responseLimit + 1) } }) });
  assert.equal((await announcedOver.send(post("/ppoi/gateway"))).status, 502);
  const overConfig = fixture({ fetch: async () => big(responseLimit + 1, CONFIG_TYPE) });
  assert.equal((await overConfig.send(get("/ppoi/ohttp-configs"))).status, 502);
  // The paymaster response cap is unchanged: the same body is fine there.
  const paymaster = fixture({ fetch: async () => big(responseLimit + 1) });
  assert.equal((await paymaster.send(post("/gateway"))).status, 200);
});
