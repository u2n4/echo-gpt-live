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
 */
import crypto from "node:crypto";

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
  const target = new URL(UBUNTU_URL);
  if (target.protocol !== "https:" || target.username || target.password || !SIGNING_KEY) {
    throw new Error("gateway requires HTTPS without embedded credentials and a signing key");
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
      redirect: "error",
    });
    const text = await res.text();
    let json;
    try { json = JSON.parse(text); } catch { json = { raw: text }; }
    return { status: res.status, json };
  } finally {
    clearTimeout(t);
  }
}

async function lifecycle(type, sessionId, timeoutMs = 2000) {
  const attempts = type === "disconnected" ? 2 : 1;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      const { status } = await callUbuntu("/rtc/" + type, {
        v: 1, type, sessionId, ts: Date.now() / 1000, nonce: uuid(),
      }, timeoutMs);
      console.log(JSON.stringify({ ev: "gateway_lifecycle", type, status, attempt }));
      if (status >= 200 && status < 300) return true;
    } catch (e) {
      console.log(JSON.stringify({ ev: "gateway_lifecycle_failed", type, err: e?.name || "Error", attempt }));
    }
  }
  return false;
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
    ev: "directive", ns, name,
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
        await lifecycle("disconnected", sessionId, 350);
        return errorResponse(
          correlationToken, endpointId, "INTERNAL_ERROR",
          "gateway did not return an answer (status " + status + ")");
      }

      if (name === "SessionConnected") {
        await lifecycle("connected", sessionId);
        return sessionAck("SessionConnected", correlationToken, endpointId, scope, sessionId);
      }

      if (name === "SessionDisconnected") {
        await lifecycle("disconnected", sessionId);
        return sessionAck("SessionDisconnected", correlationToken, endpointId, scope, sessionId);
      }
    }

    return errorResponse(correlationToken, endpointId, "INVALID_DIRECTIVE",
      "unhandled " + ns + "/" + name);
  } catch (e) {
    if (ns === "Alexa.RTCSessionController" && name === "InitiateSessionWithOffer") {
      await lifecycle("disconnected", directive.payload?.sessionId, 350);
    }
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
