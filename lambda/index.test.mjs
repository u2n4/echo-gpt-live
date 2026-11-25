// Tests for the TV endpoint in the Smart Home Lambda: node --test lambda/index.test.mjs
// tvd's side is a real local HTTP server, so keep-alive and the stale-socket retry are exercised.
import test from "node:test";
import assert from "node:assert/strict";
import crypto from "node:crypto";
import http from "node:http";

const TV_KEY = "k".repeat(32);
const SCOPE = "tv-v1\nPOST\n/tv/cmd\n";

// --- fake tvd ---------------------------------------------------------------------------
let handle = null;                 // (body, req, res, perSocketCount) => void
const seen = [];                   // every request tvd received: { body, headers, reqOnSocket }
const server = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    req.socket.served = (req.socket.served || 0) + 1;
    const raw = Buffer.concat(chunks).toString();
    let body = {};
    try { body = JSON.parse(raw); } catch {}
    seen.push({ body, raw, headers: req.headers, reqOnSocket: req.socket.served });
    handle(body, req, res, req.socket.served);
  });
});
server.keepAliveTimeout = 60000;
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const reply = (res, status, json) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(json)); };

process.env.ACV_UBUNTU_URL = `http://127.0.0.1:${server.address().port}`;
process.env.ACV_SIGNING_KEY = "bridge-key-".repeat(4);
process.env.ACV_TV_KEY = TV_KEY;
const L = await import("./index.mjs?tv");

test.beforeEach(() => { seen.length = 0; handle = (b, req, res) => reply(res, 200, { ok: true, state: { power: "on" } }); });
test.after(() => { server.closeAllConnections(); server.close(); });

function directive(ns, name, payload = {}, endpointId = "acv-tv-1", messageId = "MSG-1") {
  return { directive: {
    header: { namespace: ns, name, messageId, correlationToken: "CT", payloadVersion: "3" },
    endpoint: { endpointId, scope: { type: "BearerToken", token: "t" } },
    payload,
  } };
}

// --- signing and discovery --------------------------------------------------------------
test("signature matches tvd/auth.py (shared test vector)", () => {
  assert.equal(L.tvSign(TV_KEY, "1800000000", '{"v":1}'),
    "97a214c1c4f95aa59f0218195ef0677ed5db4c9dddb745d014f5146ac095e353");
});

test("discovery adds the TV and leaves the RTC endpoint as it was", async () => {
  const r = await L.handler({ directive: { header: { namespace: "Alexa.Discovery", name: "Discover" }, payload: {} } });
  const eps = r.event.payload.endpoints;
  assert.equal(eps.length, 2);
  assert.equal(eps[0].endpointId, "acv-chatgpt-voice-1");
  assert.ok(eps[0].capabilities.some((c) => c.interface === "Alexa.RTCSessionController"));
  const tv = eps[1];
  assert.equal(tv.endpointId, "acv-tv-1");
  assert.deepEqual(tv.displayCategories, ["TV"]);
  const ifaces = tv.capabilities.map((c) => c.interface).sort();
  assert.deepEqual(ifaces, ["Alexa", "Alexa.EndpointHealth", "Alexa.KeypadController", "Alexa.Launcher",
    "Alexa.PlaybackController", "Alexa.PowerController", "Alexa.StepSpeaker"]);
  const keypad = tv.capabilities.find((c) => c.interface === "Alexa.KeypadController");
  assert.deepEqual(keypad.keys, ["UP", "DOWN", "LEFT", "RIGHT", "SELECT", "BACK"]);
});

test("no TV key: discovery has only the RTC endpoint and TV directives are refused", async () => {
  process.env.ACV_TV_KEY = "";
  const N = await import("./index.mjs?notv");
  process.env.ACV_TV_KEY = TV_KEY;
  const r = await N.handler({ directive: { header: { namespace: "Alexa.Discovery", name: "Discover" }, payload: {} } });
  assert.equal(r.event.payload.endpoints.length, 1);
  const e = await N.handler(directive("Alexa.PowerController", "TurnOn"));
  assert.equal(e.event.payload.type, "NO_SUCH_ENDPOINT");
  assert.equal(seen.length, 0);
});

// --- the call to tvd --------------------------------------------------------------------
test("TurnOn is signed for tvd and reports ON", async () => {
  const r = await L.handler(directive("Alexa.PowerController", "TurnOn"));
  assert.equal(seen.length, 1);
  const { raw, headers } = seen[0];
  const ts = headers["x-tv-timestamp"];
  assert.match(ts, /^\d+$/);
  const want = crypto.createHmac("sha256", TV_KEY).update(SCOPE + ts + "." + raw).digest("hex");
  assert.equal(headers["x-tv-signature"], want);
  assert.equal(headers["x-acv-signature"], undefined);
  const body = seen[0].body;
  assert.equal(body.action, "power_on");
  assert.equal(body.msg_id, "MSG-1");
  assert.match(body.nonce, /^[A-Za-z0-9_-]{16,64}$/);
  const left = body.deadline - Date.now() / 1000;
  assert.ok(left > 6 && left <= 7, "deadline " + left);
  assert.equal(r.event.header.name, "Response");
  assert.equal(r.event.endpoint.endpointId, "acv-tv-1");
  assert.equal(r.context.properties.find((p) => p.name === "powerState").value, "ON");
});

test("the bridge key is never used for the TV", async () => {
  await L.handler(directive("Alexa.PowerController", "TurnOff"));
  const { raw, headers } = seen[0];
  const withBridgeKey = crypto.createHmac("sha256", process.env.ACV_SIGNING_KEY)
    .update(SCOPE + headers["x-tv-timestamp"] + "." + raw).digest("hex");
  assert.notEqual(headers["x-tv-signature"], withBridgeKey);
});

test("the connection is kept open and reused between commands", async () => {
  await L.handler(directive("Alexa.PowerController", "TurnOn", {}, "acv-tv-1", "A"));
  await L.handler(directive("Alexa.PowerController", "TurnOn", {}, "acv-tv-1", "B"));
  assert.equal(seen.length, 2);
  assert.equal(seen[1].reqOnSocket, seen[0].reqOnSocket + 1, "second request rode the same socket");
});

test("a state query on a dropped kept-alive socket is retried once on a fresh one", async () => {
  await L.handler(directive("Alexa", "ReportState", {}, "acv-tv-1", "warm"));  // leaves a socket open
  seen.length = 0;
  // Next request on that old socket: tvd's side drops it without answering (a stale socket).
  handle = (b, req, res, n) => { if (n > 1) { req.socket.destroy(); return; } reply(res, 200, { ok: true, state: { power: "on" } }); };
  const r = await L.handler(directive("Alexa", "ReportState", {}, "acv-tv-1", "RS-1"));
  assert.equal(r.event.header.name, "StateReport");
  assert.equal(seen.length, 2);
  assert.notEqual(seen[0].body.nonce, seen[1].body.nonce);
  assert.equal(seen[1].reqOnSocket, 1, "retry used a fresh socket");
});

test("the keep-warm ping opens the connection the next command reuses", async () => {
  L._setLastUseForTest(0);  // nothing open
  const w = await L.handler({ warm: "tv" });
  assert.equal(w.warmed, true);
  assert.equal(seen[0].body.action, "state");
  await L.handler(directive("Alexa.PowerController", "TurnOn", {}, "acv-tv-1", "after-warm"));
  assert.equal(seen[1].reqOnSocket, seen[0].reqOnSocket + 1, "the command rode the warmed socket");
});

test("a socket idle longer than 2 minutes is not reused", async () => {
  await L.handler(directive("Alexa", "ReportState", {}, "acv-tv-1", "idle-a"));
  L._setLastUseForTest(Date.now() - 130000);
  seen.length = 0;
  await L.handler(directive("Alexa", "ReportState", {}, "acv-tv-1", "idle-b"));
  assert.equal(seen[0].reqOnSocket, 1, "fresh socket after the idle cap");
});

test("commands are never retried (a reset socket does not prove they did not run)", async () => {
  for (const [ns, name, payload] of [
    ["Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: 1 }],
    ["Alexa.KeypadController", "SendKeystroke", { keystroke: "RIGHT" }],
    ["Alexa.PowerController", "TurnOn", {}],
    ["Alexa.Launcher", "LaunchTarget", { name: "YouTube" }],
  ]) {
    handle = (b, req, res) => reply(res, 200, { ok: true, state: { power: "on" } });
    await L.handler(directive("Alexa", "ReportState", {}, "acv-tv-1", "warm-" + name));  // open a socket
    seen.length = 0;
    handle = (b, req, res, n) => { if (n > 1) { req.socket.destroy(); return; } reply(res, 200, { ok: true }); };
    const r = await L.handler(directive(ns, name, payload, "acv-tv-1", "NR-" + name));
    assert.equal(r.event.payload.type, "ENDPOINT_UNREACHABLE", name);
    assert.equal(seen.length, 1, name + " was sent once");
  }
});

test("a dropped fresh connection is a failure, not a retry", async () => {
  handle = (b, req) => req.socket.destroy();
  const fresh = await import("./index.mjs?fresh");  // own agent: no socket to reuse
  const r = await fresh.handler(directive("Alexa.PowerController", "TurnOn"));
  assert.equal(r.event.payload.type, "ENDPOINT_UNREACHABLE");
  assert.equal(seen.length, 1);
});

test("opening an app gets the longer deadline (it may switch the TV on first)", async () => {
  await L.handler(directive("Alexa.Launcher", "LaunchTarget", { name: "YouTube" }, "acv-tv-1", "L-1"));
  const left = seen[0].body.deadline - Date.now() / 1000;
  assert.ok(left > 7.0 && left <= 7.5, "deadline " + left);
});

test("tvd errors map to Alexa error types", async () => {
  for (const [json, status, type, d] of [
    [{ ok: false, error: "off" }, 200, "NOT_IN_OPERATION", ["Alexa.PlaybackController", "Pause"]],
    [{ ok: false, error: "busy" }, 200, "ENDPOINT_BUSY", ["Alexa.PowerController", "TurnOff"]],
    [{ ok: false, error: "forbidden" }, 403, "INTERNAL_ERROR", ["Alexa.PowerController", "TurnOn"]],
  ]) {
    handle = (b, req, res) => reply(res, status, json);
    const r = await L.handler(directive(...d));
    assert.equal(r.event.payload.type, type);
  }
});

test("ReportState reports observed power; unknown is unreachable", async () => {
  handle = (b, req, res) => reply(res, 200, { ok: true, state: { power: "standby" } });
  let r = await L.handler(directive("Alexa", "ReportState"));
  assert.equal(r.event.header.name, "StateReport");
  assert.equal(r.context.properties.find((p) => p.name === "powerState").value, "OFF");
  handle = (b, req, res) => reply(res, 200, { ok: true, state: { power: "unknown" } });
  r = await L.handler(directive("Alexa", "ReportState"));
  assert.equal(r.event.payload.type, "ENDPOINT_UNREACHABLE");
});

test("unknown endpoint is refused; the RTC endpoint's ReportState is unchanged", async () => {
  let r = await L.handler(directive("Alexa", "ReportState", {}, "someone-else"));
  assert.equal(r.event.payload.type, "NO_SUCH_ENDPOINT");
  r = await L.handler(directive("Alexa", "ReportState", {}, "acv-chatgpt-voice-1"));
  assert.equal(r.event.header.name, "StateReport");
  assert.equal(r.context.properties[0].value.value, "OK");
  assert.equal(seen.length, 0);
});

// --- pure mapping -----------------------------------------------------------------------
test("directive mapping", () => {
  const A = L.tvAction;
  assert.deepEqual(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: 3 }), { action: "volume_step", args: { steps: 3 } });
  assert.deepEqual(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: -40 }), { action: "volume_step", args: { steps: -5 } });
  assert.deepEqual(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: 10 }), { action: "volume_step", args: { steps: 1 } });
  assert.deepEqual(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: -10 }), { action: "volume_step", args: { steps: -1 } });
  assert.deepEqual(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: 4, volumeStepsDefault: true }), { action: "volume_step", args: { steps: 1 } });
  assert.ok(A("Alexa.StepSpeaker", "AdjustVolume", { volumeSteps: 0 }).error);
  assert.ok(A("Alexa.StepSpeaker", "SetMute", { mute: "yes" }).error);
  assert.deepEqual(A("Alexa.PlaybackController", "Pause", {}), { action: "pause" });
  assert.ok(A("Alexa.PlaybackController", "FastForward", {}).error);
  assert.deepEqual(A("Alexa.Launcher", "LaunchTarget", { identifier: "amzn1.alexa-ask-target.app.70045", name: "YouTube" }),
    { action: "launch", args: { app: "youtube" } });
  assert.deepEqual(A("Alexa.Launcher", "LaunchTarget", { name: "Netflix" }), { action: "launch", args: { app: "netflix" } });
  assert.ok(A("Alexa.Launcher", "LaunchTarget", { identifier: "amzn1.alexa-ask-target.app.72095", name: "Prime Video" }).error);
  assert.ok(A("Alexa.RTCSessionController", "InitiateSessionWithOffer", {}).error);
  assert.deepEqual(A("Alexa.KeypadController", "SendKeystroke", { keystroke: "RIGHT" }), { action: "key", args: { key: "right" } });
  assert.deepEqual(A("Alexa.KeypadController", "SendKeystroke", { keystroke: "SELECT" }), { action: "key", args: { key: "select" } });
  assert.deepEqual(A("Alexa.KeypadController", "SendKeystroke", { keystroke: "BACK" }), { action: "key", args: { key: "back" } });
  for (const bad of ["INFO", "PAGE_UP", "right", "__proto__", "constructor", undefined]) {
    assert.ok(A("Alexa.KeypadController", "SendKeystroke", { keystroke: bad }).error, String(bad));
  }
  assert.deepEqual(A("Alexa.Launcher", "LaunchTarget", { identifier: "amzn1.alexa-ask-target.shortcut.33122", name: "Home" }),
    { action: "key", args: { key: "home" } });
  for (const bad of ["__proto__", "constructor", "toString"]) {
    assert.ok(A("Alexa.Launcher", "LaunchTarget", { identifier: bad, name: "x" }).error, bad);
  }
});

test("properties carry the box's own observation time and real connectivity", () => {
  const now = Date.parse("2026-10-18T00:00:10Z");
  let p = L.tvProperties({ power: "on", connected: true, observed_at: now / 1000 - 4 }, now);
  const power = p.find((x) => x.name === "powerState");
  assert.equal(power.timeOfSample, "2026-10-18T00:00:06.000Z");
  assert.equal(power.uncertaintyInMilliseconds, 4000);
  assert.equal(p.find((x) => x.name === "connectivity").value.value, "OK");
  p = L.tvProperties({ power: "standby", connected: false, observed_at: now / 1000 - 60 }, now);
  assert.equal(p.find((x) => x.name === "connectivity").value.value, "OK");
  p = L.tvProperties({ power: "unknown", connected: false }, now);
  assert.equal(p.find((x) => x.name === "powerState"), undefined);
  assert.equal(p.find((x) => x.name === "connectivity").value.value, "UNREACHABLE");
});
