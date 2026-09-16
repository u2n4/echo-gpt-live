#!/usr/bin/env bash
# End-to-end preflight: a second bridge instance on its own ports, driven by echosim, so a change
# to the session configuration or the SDP is proven against the real OpenAI API before the Echo
# ever dials in. Costs a few seconds of GPT-Live time and always hangs up.
#
# It must never bind the live bridge's port: echosim would then open a real session on the running
# assistant, which is how an earlier version of this harness spent money by accident.
#
# The simulated microphone sends real speech, not a tone: a tone gives gpt-live-1 nothing to answer,
# so the recording of its voice comes back as encoded silence and proves nothing about quality.
#
# Usage: scripts/preflight.sh [seconds] [audio-file]
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
SECS="${1:-8}"
SAY="${2:-media/hello.ogg}"
PORT=8798
UDP=50012
RUN="preflight-$(date -u +%H%M%S)"

if ss -ltn 2>/dev/null | grep -q "127.0.0.1:$PORT "; then
  echo "port $PORT is busy; refusing to run so this cannot hit the live bridge" >&2
  exit 1
fi

./bin/bridge --run "$RUN" --listen "127.0.0.1:$PORT" --udp-port "$UDP" \
  --video sendonly --video-fps 1 --hold-after-disconnect 2s --max-session 2m \
  --delegation client --record-dir "/tmp/preflight-rec" >/tmp/preflight-bridge.log 2>&1 &
BR=$!
sleep 2
if grep -q '"ev":"http_error"' /tmp/preflight-bridge.log; then
  echo "bridge failed to listen:" >&2; grep '"ev":"http_error"' /tmp/preflight-bridge.log >&2
  kill -INT $BR 2>/dev/null; exit 1
fi

./bin/echosim --url "http://127.0.0.1:$PORT" --audio-file "$SAY" --duration "${SECS}s" 2>/tmp/preflight-echosim.err >/tmp/preflight-answer.json
kill -INT $BR 2>/dev/null; wait $BR 2>/dev/null

python3 - "runs/$RUN/events.jsonl" <<'PY'
import json, sys

evs = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by = {}
for e in evs:
    by.setdefault(e["ev"], []).append(e)

def one(name, *fields):
    for e in by.get(name, []):
        print("  ", name, {k: e[k] for k in fields if k in e})

print("--- OpenAI session ---")
one("live_create_session", "status", "ms", "billed_init_seconds")
one("live_error", "code", "message")
one("live_storage_not_allowed", "action")
one("live_session_started", "ms_after_dial")
one("live_first_audio", "ms_after_dial")
one("live_hangup", "status")
one("live_cost_estimate", "seconds", "usd")

print("--- what we asked each encoder for ---")
for e in by.get("bridge_start", []):
    print("   opus_fmtp:", e.get("opus_fmtp"))
    print("   noise_reduction:", e.get("noise_reduction"))

print("--- recordings ---")
one("record_open", "path")
one("record_closed", "path", "packets", "approx_seconds", "bytes")

errs = [e for e in evs if "err" in e and e["ev"] not in ("live_storage_not_allowed",)]
print("--- errors ---" if errs else "--- no errors ---")
for e in errs:
    print("  ", e["ev"], e["err"])
PY
