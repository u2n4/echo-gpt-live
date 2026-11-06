import { test } from "node:test";
import assert from "node:assert/strict";

process.env.ACV_UBUNTU_URL = "https://gateway.example";
process.env.ACV_SIGNING_KEY = "offline-test-key";
const { handler } = await import("./index.mjs");
const directive = (name) => ({ directive: {
  header: { namespace: "Alexa.RTCSessionController", name },
  payload: { sessionId: "offline-test", offer: { format: "SDP", value: "v=0" } },
} });

test("failed offer requests bounded cleanup", async () => {
  const calls = [];
  const original = globalThis.fetch;
  globalThis.fetch = async (address, options) => {
    calls.push({ path: new URL(address).pathname, type: JSON.parse(options.body).type });
    assert.equal(options.redirect, "error");
    return new Response("{}", { status: calls.length === 1 ? 502 : 200 });
  };
  try {
    const response = await handler(directive("InitiateSessionWithOffer"));
    assert.equal(response.event.header.name, "ErrorResponse");
    assert.deepEqual(calls.map(c => c.path), ["/rtc/offer", "/rtc/disconnected"]);
    assert.equal(calls[1].type, "disconnected");
  } finally { globalThis.fetch = original; }
});

test("offer transport failure still requests cleanup", async () => {
  const calls = [];
  const original = globalThis.fetch;
  globalThis.fetch = async address => {
    calls.push(new URL(address).pathname);
    if (calls.length === 1) throw new Error("offline failure");
    return new Response("{}", { status: 200 });
  };
  try {
    await handler(directive("InitiateSessionWithOffer"));
    assert.deepEqual(calls, ["/rtc/offer", "/rtc/disconnected"]);
  } finally { globalThis.fetch = original; }
});

test("disconnect retries a non-success HTTP reply", async () => {
  let requests = 0;
  const original = globalThis.fetch;
  globalThis.fetch = async () => new Response("{}", { status: ++requests === 1 ? 503 : 200 });
  try {
    const response = await handler(directive("SessionDisconnected"));
    assert.equal(response.event.header.name, "SessionDisconnected");
    assert.equal(requests, 2);
  } finally { globalThis.fetch = original; }
});

test("plaintext gateway is refused before fetch", async () => {
  process.env.ACV_UBUNTU_URL = "http://gateway.example";
  const unsafe = await import("./index.mjs?plaintext-test");
  const original = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = async () => { requests++; throw new Error("must not fetch"); };
  try {
    await unsafe.handler(directive("SessionConnected"));
    assert.equal(requests, 0);
  } finally { globalThis.fetch = original; process.env.ACV_UBUNTU_URL = "https://gateway.example"; }
});
