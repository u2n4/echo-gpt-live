/**
 * ACV Alexa Smart Home Lambda adapter.
 *
 * The ONLY cloud component. Handles Alexa Smart Home directives and forwards
 * RTC signaling to the Ubuntu gateway over HTTPS (Tailscale Funnel). NO media
 * ever passes through Lambda — only SDP offer/answer text and lifecycle events.
 *
 * Responsibilities (per docs/DECISIONS.md ADR-004):
 *   - Alexa.Discovery            -> advertise ONE RTC endpoint (CAMERA category)
 *   - Alexa.RTCSessionController  -> InitiateSessionWithOffer / SessionConnected / SessionDisconnected
 *   - Alexa.EndpointHealth        -> connectivity OK
 *   - Alexa.Authorization         -> AcceptGrant (store LWA grant for async events)
 *
 * Contract verified from developer.amazon.com 2026-07-16 (docs/CONTRACT.md, SOURCES.md).
 *
 * Env (set on the Lambda, NOT in git):
 *   ACV_UBUNTU_URL     https base URL of the Tailscale Funnel (e.g. https://host.tailXXXX.ts.net)
 *   ACV_SIGNING_KEY    shared HMAC secret (same bytes as Ubuntu ~/.config/acv/signing.key)
 *   ACV_ENDPOINT_ID    stable endpoint id (default "acv-chatgpt-voice-1")
 *   ACV_FRIENDLY_NAME  friendly name shown in Alexa app (default "ChatGPT Voice")
 *   ACV_TV_KEY         tvd's own HMAC key (Ubuntu ~/.config/tvd/signing.key). When set, Discovery
 *                      also advertises the TV endpoint; when empty the TV does not exist.
 *   ACV_TV_NAME        the TV's friendly name (default "TV")
 *
 * The TV endpoint (docs/TV-PLAN.md) is a second, separate device: its directives go to tvd at
 * {ACV_UBUNTU_URL}/tv/cmd with tvd's key and a scoped signature, never to the bridge's /rtc routes.
 */
import crypto from "node:crypto";
import http from "node:http";
import https from "node:https";

const UBUNTU_URL = (process.env.ACV_UBUNTU_URL || "").replace(/\/$/, "");
const SIGNING_KEY = process.env.ACV_SIGNING_KEY || "";
const ENDPOINT_ID = process.env.ACV_ENDPOINT_ID || "acv-chatgpt-voice-1";
const FRIENDLY_NAME = process.env.ACV_FRIENDLY_NAME || "ChatGPT Voice";
// Display category is an EXPERIMENTAL VARIABLE, not a constant.
//
// Amazon documents only "CAMERA, or both CAMERA and DOORBELL", and the July 2026 sessions used
// CAMERA and were torn down 26-28 s after the answer. Amazon's Smart Home debugger documents
// NO_VIDEO_ERROR as "Couldn't establish connection because timeout occurred waiting on video",
// which a camera-category endpoint that sends no video would hit.
//
// The only known prior art that bridged an Echo to a realtime voice model
// (shinyoshiaki/alexa-chat-gpt, Jan 2025) declared DOORBELL ALONE and demoed successfully.
// Set ACV_DISPLAY_CATEGORIES=DOORBELL to reproduce that, then re-run device discovery.
const DISPLAY_CATEGORIES = (process.env.ACV_DISPLAY_CATEGORIES || "CAMERA")
  .split(",").map((s) => s.trim()).filter(Boolean);
// EndpointHealth is optional: the prior art omits it. ACV_ENDPOINT_HEALTH=0 drops it.
const WITH_ENDPOINT_HEALTH = process.env.ACV_ENDPOINT_HEALTH !== "0";
const MANUFACTURER = "ACV";

const TV_KEY = process.env.ACV_TV_KEY || "";
const TV_ENDPOINT_ID = "acv-tv-1";
const TV_NAME = process.env.ACV_TV_NAME || "TV";
// Alexa allows 8 s per directive. Every TV command carries an absolute deadline 7 s after the
// directive reached us; tvd fits its work (queue + command) inside it minus its reply margin,
// and the fetch is aborted at the same moment, leaving ~1 s for our own response.
const TV_DEADLINE_MS = 7000;
// Opening an app may switch the TV on first (wake ~2.5-4 s, then the app): it gets 0.5 s more,
// still answering Alexa ~0.5 s inside its 8 s.
const TV_LAUNCH_DEADLINE_MS = 7500;
// From Amazon's Predefined Launch Target Reference (checked 2026-10-18). Names are matched too,
// in case Alexa resolves the app by name only.
const TV_APPS = Object.assign(Object.create(null), {
  "amzn1.alexa-ask-target.app.70045": "youtube",
  "amzn1.alexa-ask-target.app.36377": "netflix",
});
// Launcher shortcut (same reference): "Home" jumps to the home screen.
const TV_HOME_SHORTCUT = "amzn1.alexa-ask-target.shortcut.33122";
// Alexa.KeypadController keystroke -> tvd key name (tvd/controller.py NAV_KEYS)
const TV_KEYS = { UP: "up", DOWN: "down", LEFT: "left", RIGHT: "right", SELECT: "select", BACK: "back" };
const TV_MAX_STEPS = 5;
const TV_DEFAULT_STEPS = 1; // bare "volume up" is one step

const uuid = () => crypto.randomUUID();

function hmacHeaders(rawBody) {
  const ts = (Date.now() / 1000).toFixed(3);
  const mac = crypto
    .createHmac("sha256", SIGNING_KEY)
    .update(ts + ".")
    .update(rawBody)
    .digest("hex");
  return { "X-ACV-Timestamp": ts, "X-ACV-Signature": mac, "Content-Type": "application/json" };
}

async function callUbuntu(path, obj, timeoutMs = 8000) {
  if (!UBUNTU_URL || !SIGNING_KEY) {
    throw new Error("ACV_UBUNTU_URL / ACV_SIGNING_KEY not configured");
  }
  const raw = JSON.stringify(obj);
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const res = await fetch(UBUNTU_URL + path, {
      method: "POST",
      headers: hmacHeaders(raw),
      body: raw,
      signal: ctrl.signal,
    });
    const text = await res.text();
    let json;
    try { json = JSON.parse(text); } catch { json = { raw: text }; }
    return { status: res.status, json };
  } finally {
    clearTimeout(t);
  }
}

// ---------- TV (tvd) ----------

// Must match tvd/auth.py: HMAC-SHA256 over SCOPE + ts + "." + body, ts in whole seconds.
const TV_SCOPE = "tv-v1\nPOST\n/tv/cmd\n";

export function tvSign(key, ts, raw) {
  return crypto.createHmac("sha256", key).update(TV_SCOPE + ts + ".").update(raw).digest("hex");
}

// One kept-alive connection to tvd, reused across warm invocations. Measured 2026-10-18: a
// round trip on a fresh connection costs ~0.7-1.8 s (TCP + TLS all the way to the house through
// the Funnel relay); on a connection that was already open, 0.2 s.
// A connection idle longer than this is never reused: a frozen Lambda cannot see the far end
// close it, and a reset socket cannot prove the request was not executed. Measured 2026-10-18:
// the Funnel kept an idle connection open past 200 s (20/45/70/100/150/200 s all reused), so two
// minutes stays well inside it. With lambda/keepwarm.sh pinging every minute, the connection
// is open when a voice command arrives.
const TV_REUSE_IDLE_MS = 120000;
// (http only ever matters for the unit tests, which run tvd's side on a local plain server.)
const makeAgent = (mod) => new mod.Agent({ keepAlive: true, maxSockets: 2, scheduling: "lifo" });
const tvAgents = { "https:": makeAgent(https), "http:": makeAgent(http) };
let tvLastUseMs = 0;

export function _setLastUseForTest(ms) { tvLastUseMs = ms; }

function postOnce(url, headers, raw, untilMs) {
  if (untilMs - Date.now() <= 0) return Promise.reject(new Error("tv deadline passed before sending"));
  return new Promise((resolve, reject) => {
    const { protocol } = new URL(url);
    const mod = protocol === "https:" ? https : http;
    if (Date.now() - tvLastUseMs > TV_REUSE_IDLE_MS) {
      tvAgents[protocol].destroy();  // drop every pooled socket; this request opens a new one
      tvAgents[protocol] = makeAgent(mod);
    }
    tvLastUseMs = Date.now();
    const req = mod.request(url, { method: "POST", agent: tvAgents[protocol], headers }, (res) => {
      const chunks = [];
      let size = 0;
      res.on("data", (c) => {
        size += c.length;
        if (size > 65536) { req.destroy(new Error("tv reply too large")); return; }
        chunks.push(c);
      });
      res.on("end", () => {
        let json;
        try { json = JSON.parse(Buffer.concat(chunks).toString("utf8")); } catch { json = {}; }
        tvLastUseMs = Date.now();
        resolve({ status: res.statusCode, json, reused: req.reusedSocket });
      });
      res.on("error", reject);
    });
    const t = setTimeout(() => req.destroy(new Error("tv deadline")), Math.max(0, untilMs - Date.now()));
    req.on("close", () => clearTimeout(t));
    req.on("error", (e) => { e.reusedSocket = req.reusedSocket; reject(e); });
    req.end(raw);
  });
}

// Only the read-only state query is ever retried: a reset socket does not prove a command was
// not executed, and tvd's msg-id record does not survive a restart (Codex review). The idle
// cap above keeps stale sockets rare; a command that does hit one fails, and he says it again.
const TV_RETRY_SAFE = new Set(["state"]);

async function callTv(action, args, msgId, deadlineMs) {
  if (!UBUNTU_URL || !TV_KEY) throw new Error("ACV_UBUNTU_URL / ACV_TV_KEY not configured");
  const attempt = () => {
    // A fresh nonce and signature per attempt; the msg_id stays the same, so a retry that
    // reaches the same tvd process is answered from its record instead of acting again.
    const raw = JSON.stringify({
      v: 1, nonce: crypto.randomBytes(18).toString("base64url"), msg_id: msgId || undefined,
      action, args: args || {}, deadline: deadlineMs / 1000,
    });
    const ts = String(Math.floor(Date.now() / 1000));
    return postOnce(UBUNTU_URL + "/tv/cmd", {
      "Content-Type": "application/json", "Content-Length": Buffer.byteLength(raw),
      "X-TV-Timestamp": ts, "X-TV-Signature": tvSign(TV_KEY, ts, raw),
    }, raw, deadlineMs);
  };
  try {
    return await attempt();
  } catch (e) {
    // Node's documented case: a kept-alive socket the far end closed while we were frozen
    // fails at once with ECONNRESET / socket hang up. Retry once on a new socket if there is
    // still time; anything else (timeouts included) is a real failure.
    const stale = e?.reusedSocket && (e.code === "ECONNRESET" || e.code === "EPIPE" || /socket hang up/.test(e.message || ""));
    if (!stale || !TV_RETRY_SAFE.has(action) || deadlineMs - Date.now() < 2000) throw e;
    console.log(JSON.stringify({ ev: "tv_retry_stale_socket", action, code: e.code || null }));
    return await attempt();
  }
}

// tvd error code -> Alexa ErrorResponse type
function tvErrorType(code) {
  if (code === "off" || code === "cancelled") return "NOT_IN_OPERATION";
  if (code === "busy") return "ENDPOINT_BUSY";
  if (code === "bad_request") return "INVALID_VALUE";
  return "ENDPOINT_UNREACHABLE";
}

// Properties as tvd observed them: powerState carries the moment the box last reported it and
// how old that report is; connectivity is whether tvd holds a live connection to the box (or the
// box's last word was "off" and it has been asleep since, which is the normal standby case).
export function tvProperties(state, nowMs) {
  const props = [];
  const now = new Date(nowMs).toISOString();
  if (state && (state.power === "on" || state.power === "standby")) {
    const seenMs = Number(state.observed_at) > 0 ? Number(state.observed_at) * 1000 : nowMs;
    props.push({ namespace: "Alexa.PowerController", name: "powerState",
      value: state.power === "on" ? "ON" : "OFF",
      timeOfSample: new Date(Math.min(seenMs, nowMs)).toISOString(),
      uncertaintyInMilliseconds: Math.max(0, Math.round(nowMs - seenMs)) });
  }
  const reachable = !!state && (state.connected === true || state.power === "standby");
  props.push({ namespace: "Alexa.EndpointHealth", name: "connectivity",
    value: { value: reachable ? "OK" : "UNREACHABLE" }, timeOfSample: now, uncertaintyInMilliseconds: 0 });
  return props;
}

function tvEndpoint() {
  return {
    endpointId: TV_ENDPOINT_ID,
    manufacturerName: MANUFACTURER,
    description: "Xiaomi TV Box (living room) via tvd",
    friendlyName: TV_NAME,
    displayCategories: ["TV"],
    cookie: {},
    capabilities: [
      { type: "AlexaInterface", interface: "Alexa", version: "3" },
      { type: "AlexaInterface", interface: "Alexa.PowerController", version: "3",
        properties: { supported: [{ name: "powerState" }], proactivelyReported: false, retrievable: true } },
      { type: "AlexaInterface", interface: "Alexa.StepSpeaker", version: "3" },
      { type: "AlexaInterface", interface: "Alexa.PlaybackController", version: "3",
        supportedOperations: ["Play", "Pause", "Stop"] },
      { type: "AlexaInterface", interface: "Alexa.Launcher", version: "3" },
      { type: "AlexaInterface", interface: "Alexa.KeypadController", version: "3", keys: Object.keys(TV_KEYS) },
      { type: "AlexaInterface", interface: "Alexa.EndpointHealth", version: "3.1",
        properties: { supported: [{ name: "connectivity" }], proactivelyReported: false, retrievable: true } },
    ],
  };
}

// Directive -> tvd action. Returns { action, args } or { error: [type, message] }.
export function tvAction(ns, name, payload) {
  if (ns === "Alexa.PowerController") {
    if (name === "TurnOn") return { action: "power_on" };
    if (name === "TurnOff") return { action: "power_off" };
  }
  if (ns === "Alexa.StepSpeaker") {
    if (name === "AdjustVolume") {
      const raw = Number(payload?.volumeSteps);
      if (!Number.isInteger(raw) || raw === 0) return { error: ["INVALID_VALUE", "volumeSteps"] };
      // A bare "turn it up" arrives as a default step count; keep that to a small nudge.
      const isDefault = payload?.volumeStepsDefault === true || Math.abs(raw) === 10;
      const n = isDefault ? TV_DEFAULT_STEPS : Math.min(Math.abs(raw), TV_MAX_STEPS);
      return { action: "volume_step", args: { steps: Math.sign(raw) * n } };
    }
    if (name === "SetMute") {
      if (typeof payload?.mute !== "boolean") return { error: ["INVALID_VALUE", "mute"] };
      return { action: "set_mute", args: { mute: payload.mute } };
    }
  }
  if (ns === "Alexa.PlaybackController") {
    const op = { Play: "play", Pause: "pause", Stop: "stop" }[name];
    if (op) return { action: op };
  }
  if (ns === "Alexa.KeypadController" && name === "SendKeystroke") {
    const key = Object.hasOwn(TV_KEYS, payload?.keystroke) ? TV_KEYS[payload.keystroke] : null;
    if (!key) return { error: ["INVALID_VALUE", "keystroke not supported"] };
    return { action: "key", args: { key } };
  }
  if (ns === "Alexa.Launcher" && name === "LaunchTarget") {
    if (payload?.identifier === TV_HOME_SHORTCUT || String(payload?.name || "").toLowerCase() === "home") {
      return { action: "key", args: { key: "home" } };
    }
    const byId = TV_APPS[payload?.identifier];
    const lname = String(payload?.name || "").toLowerCase();
    const byName = lname === "youtube" ? "youtube" : lname === "netflix" ? "netflix" : null;
    const app = byId || byName;
    if (!app) return { error: ["INVALID_VALUE", "app not supported"] };
    return { action: "launch", args: { app } };
  }
  if (ns === "Alexa" && name === "ReportState") return { action: "state" };
  return { error: ["INVALID_DIRECTIVE", "unsupported on TV: " + ns + "/" + name] };
}

async function handleTv(directive, correlationToken, scope, startedMs) {
  const ns = directive.header?.namespace;
  const name = directive.header?.name;
  const mapped = tvAction(ns, name, directive.payload);
  if (mapped.error) return errorResponse(correlationToken, TV_ENDPOINT_ID, ...mapped.error);

  let status = 0, json = {}, reused = null;
  try {
    ({ status, json, reused } = await callTv(mapped.action, mapped.args, directive.header?.messageId,
      startedMs + (mapped.action === "launch" ? TV_LAUNCH_DEADLINE_MS : TV_DEADLINE_MS)));
  } catch (e) {
    console.log(JSON.stringify({ ev: "tv_unreachable", action: mapped.action, err: e?.name || "Error" }));
    return errorResponse(correlationToken, TV_ENDPOINT_ID, "ENDPOINT_UNREACHABLE", "tv controller unreachable");
  }
  console.log(JSON.stringify({ ev: "tv_reply", action: mapped.action, status, ok: !!json?.ok,
    error: json?.error || null, power: json?.state?.power || null, ms: json?.ms ?? null,
    reused, total_ms: Date.now() - startedMs }));
  if (status !== 200) {
    return errorResponse(correlationToken, TV_ENDPOINT_ID, "INTERNAL_ERROR", "tv controller refused (" + status + ")");
  }
  if (!json.ok) {
    return errorResponse(correlationToken, TV_ENDPOINT_ID, tvErrorType(json.error), "tv: " + (json.error || "failed"));
  }
  const now = Date.now();
  if (mapped.action === "state") {
    if (json.state?.power === "unknown") {
      return errorResponse(correlationToken, TV_ENDPOINT_ID, "ENDPOINT_UNREACHABLE", "tv state unknown");
    }
    return {
      event: { header: header("Alexa", "StateReport", correlationToken),
        endpoint: { scope, endpointId: TV_ENDPOINT_ID }, payload: {} },
      context: { properties: tvProperties(json.state, now) },
    };
  }
  return {
    event: { header: header("Alexa", "Response", correlationToken),
      endpoint: { scope, endpointId: TV_ENDPOINT_ID }, payload: {} },
    context: { properties: tvProperties(json.state, now) },
  };
}

// ---------- response builders ----------

function header(namespace, name, correlationToken) {
  const h = { namespace, name, messageId: uuid(), payloadVersion: "3" };
  if (correlationToken) h.correlationToken = correlationToken;
  return h;
}

function discoverResponse() {
  return {
    event: {
      header: header("Alexa.Discovery", "Discover.Response"),
      payload: {
        endpoints: [
          {
            endpointId: ENDPOINT_ID,
            manufacturerName: MANUFACTURER,
            description: "ChatGPT Voice bridge (audio intercom via WebRTC)",
            friendlyName: FRIENDLY_NAME,
            // Only documented display category for RTCSessionController is CAMERA.
            // This is an audio intercom-class endpoint; NOT a real camera. See ADR-005/010.
            displayCategories: DISPLAY_CATEGORIES,
            cookie: {},
            capabilities: [
              { type: "AlexaInterface", interface: "Alexa", version: "3" },
              {
                type: "AlexaInterface",
                interface: "Alexa.RTCSessionController",
                version: "3",
                configuration: { isFullDuplexAudioSupported: true },
              },
              ...(WITH_ENDPOINT_HEALTH
                ? [{
                    type: "AlexaInterface",
                    interface: "Alexa.EndpointHealth",
                    version: "3.1",
                    properties: {
                      supported: [{ name: "connectivity" }],
                      proactivelyReported: true,
                      retrievable: true,
                    },
                  }]
                : []),
            ],
          },
          ...(TV_KEY ? [tvEndpoint()] : []),
        ],
      },
    },
  };
}

function errorResponse(correlationToken, endpointId, type, message) {
  return {
    event: {
      header: header("Alexa", "ErrorResponse", correlationToken),
      endpoint: { endpointId: endpointId || ENDPOINT_ID },
      payload: { type: type || "INTERNAL_ERROR", message: message || "error" },
    },
  };
}

function answerEvent(correlationToken, endpointId, scope, answerSdp) {
  return {
    event: {
      header: header("Alexa.RTCSessionController", "AnswerGeneratedForSession", correlationToken),
      endpoint: { scope, endpointId },
      // NOTE: payload carries answer only, NO sessionId (verified contract).
      payload: { answer: { format: "SDP", value: answerSdp } },
    },
  };
}

function sessionAck(name, correlationToken, endpointId, scope, sessionId) {
  return {
    event: {
      header: header("Alexa.RTCSessionController", name, correlationToken),
      endpoint: { scope, endpointId },
      payload: { sessionId },
    },
  };
}

function healthResponse(correlationToken, endpointId, scope) {
  return {
    event: {
      header: header("Alexa", "StateReport", correlationToken),
      endpoint: { scope, endpointId },
      payload: {},
    },
    context: {
      properties: [
        {
          namespace: "Alexa.EndpointHealth",
          name: "connectivity",
          value: { value: "OK" },
          timeOfSample: new Date(0).toISOString(), // stamped at runtime below
          uncertaintyInMilliseconds: 0,
        },
      ],
    },
  };
}

// ---------- main handler ----------

export const handler = async (event) => {
  const startedMs = Date.now();
  // lambda/keepwarm.sh: an EventBridge rule sends {"warm":"tv"} every minute, so this container
  // and its connection to tvd are already open when he speaks. A read-only state query.
  if (event && event.warm === "tv" && !event.directive) {
    if (!TV_KEY) return { warmed: false };
    try {
      const { status, reused } = await callTv("state", {}, null, startedMs + 5000);
      return { warmed: status === 200, reused };
    } catch (e) {
      return { warmed: false, err: e?.code || e?.name || "Error" };
    }
  }
  const directive = event?.directive;
  if (!directive) return errorResponse(null, null, "INVALID_DIRECTIVE", "no directive");

  const ns = directive.header?.namespace;
  const name = directive.header?.name;
  const correlationToken = directive.header?.correlationToken;
  const endpoint = directive.endpoint || {};
  const endpointId = endpoint.endpointId || ENDPOINT_ID;
  const scope = endpoint.scope || directive.payload?.scope || { type: "BearerToken", token: "" };

  // Safe logging: directive identity + shape only. NEVER SDP/tokens/secrets.
  const hasOffer = !!(directive.payload?.offer?.value);
  console.log(JSON.stringify({
    ev: "directive", ns, name, endpointId: endpoint.endpointId || null,
    sessionId: directive.payload?.sessionId || null,
    hasOffer, offerLen: hasOffer ? directive.payload.offer.value.length : 0,
  }));

  try {
    // Discovery
    if (ns === "Alexa.Discovery" && name === "Discover") {
      console.log(JSON.stringify({ ev: "discovery", categories: DISPLAY_CATEGORIES, endpointHealth: WITH_ENDPOINT_HEALTH }));
      return discoverResponse();
    }

    // Authorization (AcceptGrant) — acknowledge; async event tokens handled out of band
    if (ns === "Alexa.Authorization" && name === "AcceptGrant") {
      // grant.code could be exchanged with LWA for Event Gateway tokens (async path).
      return {
        event: {
          header: header("Alexa.Authorization", "AcceptGrant.Response"),
          payload: {},
        },
      };
    }

    // The TV is a separate device with its own controller; nothing below this applies to it.
    if (endpoint.endpointId === TV_ENDPOINT_ID) {
      if (!TV_KEY) return errorResponse(correlationToken, TV_ENDPOINT_ID, "NO_SUCH_ENDPOINT", "tv not configured");
      return await handleTv(directive, correlationToken, scope, startedMs);
    }
    if (endpoint.endpointId && endpoint.endpointId !== ENDPOINT_ID) {
      return errorResponse(correlationToken, endpoint.endpointId, "NO_SUCH_ENDPOINT", "unknown endpoint");
    }

    // EndpointHealth / ReportState
    if ((ns === "Alexa" && name === "ReportState") || ns === "Alexa.EndpointHealth") {
      const r = healthResponse(correlationToken, endpointId, scope);
      r.context.properties[0].timeOfSample = new Date().toISOString();
      return r;
    }

    // RTC session control
    if (ns === "Alexa.RTCSessionController") {
      const sessionId = directive.payload?.sessionId;

      if (name === "InitiateSessionWithOffer") {
        const offer = directive.payload?.offer;
        if (!offer || offer.format !== "SDP" || !offer.value) {
          return errorResponse(correlationToken, endpointId, "INVALID_VALUE", "bad offer");
        }
        // Forward to Ubuntu gateway. Cap the wait BELOW Alexa's hard 6s directive
        // deadline (5s) so we return a clean error before Alexa times out, rather
        // than racing past it and having Alexa discard a late answer.
        const { status, json } = await callUbuntu("/rtc/offer", {
          v: 1, type: "offer", sessionId, sdp: offer.value,
          ts: Date.now() / 1000, nonce: uuid(),
        }, 5000);
        console.log(JSON.stringify({
          ev: "gateway_reply", status,
          type: json?.type || null,
          answerLen: json?.sdp ? json.sdp.length : 0,
          code: json?.code || null,
        }));
        if (status === 200 && json?.type === "answer" && json.sdp) {
          return answerEvent(correlationToken, endpointId, scope, json.sdp);
        }
        return errorResponse(
          correlationToken, endpointId, "INTERNAL_ERROR",
          "gateway did not return an answer (status " + status + ")");
      }

      if (name === "SessionConnected") {
        await callUbuntu("/rtc/connected", {
          v: 1, type: "connected", sessionId,
          ts: Date.now() / 1000, nonce: uuid(),
        }).catch(() => {});
        return sessionAck("SessionConnected", correlationToken, endpointId, scope, sessionId);
      }

      if (name === "SessionDisconnected") {
        await callUbuntu("/rtc/disconnected", {
          v: 1, type: "disconnected", sessionId,
          ts: Date.now() / 1000, nonce: uuid(),
        }).catch(() => {});
        return sessionAck("SessionDisconnected", correlationToken, endpointId, scope, sessionId);
      }
    }

    return errorResponse(correlationToken, endpointId, "INVALID_DIRECTIVE",
      "unhandled " + ns + "/" + name);
  } catch (e) {
    // never leak secrets — log the error identity plus the CAUSE CHAIN. Node's fetch reports
    // every network problem as the same opaque "fetch failed"; the reason (ENOTFOUND,
    // ECONNREFUSED, ECONNRESET, UND_ERR_CONNECT_TIMEOUT, a TLS error) is only in e.cause.
    const causes = [];
    for (let c = e?.cause, i = 0; c && i < 4; c = c?.cause, i++) {
      causes.push({
        name: c?.name || undefined,
        code: c?.code || undefined,
        errno: c?.errno || undefined,
        msg: (c?.message || "").slice(0, 160),
      });
    }
    let host = "";
    try { host = new URL(UBUNTU_URL).host; } catch { host = "<unparseable>"; }
    console.log(JSON.stringify({
      ev: "handler_error", ns, name,
      err: e?.name || "Error",
      msg: (e?.message || "").slice(0, 160),
      target_host: host,
      causes,
    }));
    return errorResponse(correlationToken, endpointId, "INTERNAL_ERROR", "handler failure");
  }
};
