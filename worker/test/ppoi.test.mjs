import assert from "node:assert/strict";
import { test } from "node:test";
import { createRelayHandler } from "../src/index.ts";

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
