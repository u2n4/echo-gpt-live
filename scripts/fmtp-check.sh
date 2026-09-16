#!/usr/bin/env bash
# Prove that --opus-fmtp reaches the SDP answer the Echo would receive, over a real loopback.
#
# Runs on its own signaling port and its own UDP port. The first version of this script used
# rtcdiag's defaults, which are the SAME port the live bridge listens on: echosim therefore talked
# to the running assistant instead of the test process, opened a real GPT-Live session and spent
# money. Never let a diagnostic bind a port the service uses.
#
# Usage: scripts/fmtp-check.sh [fmtp-string]
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
FMTP="${1:-minptime=10;useinbandfec=1;usedtx=0;stereo=0;sprop-stereo=0;maxplaybackrate=48000;maxaveragebitrate=64000}"
PORT=8799
UDP=50011

if ss -ltn 2>/dev/null | grep -q "127.0.0.1:$PORT "; then
  echo "port $PORT is busy; refusing to run so this cannot hit the live bridge" >&2
  exit 1
fi

RUN="fmtp-check-$(date -u +%H%M%S)"
./bin/rtcdiag --run "$RUN" --listen "127.0.0.1:$PORT" --udp-port "$UDP" --stun "" \
  --video sendonly --audio reflect --hold-after-disconnect 2s --opus-fmtp "$FMTP" >/tmp/fmtp-rtcdiag.log 2>&1 &
RD=$!
sleep 2
if grep -q '"ev":"http_error"' /tmp/fmtp-rtcdiag.log; then
  echo "rtcdiag failed to listen:" >&2; grep '"ev":"http_error"' /tmp/fmtp-rtcdiag.log >&2
  kill -INT $RD 2>/dev/null; exit 1
fi

./bin/echosim --url "http://127.0.0.1:$PORT" --duration 6s 2>/tmp/fmtp-echosim.err > /tmp/fmtp-answer.json
kill -INT $RD 2>/dev/null; wait $RD 2>/dev/null

python3 - <<'PY'
import json
d = json.load(open("/tmp/fmtp-answer.json"))
print("answer_ms", d["answer_ms"],
      "| audio sent/received", d["audio_pkts_sent"], d["audio_pkts_received"],
      "| video received", d["video_pkts_received"])
for l in d["answer_lines"]:
    if l.startswith(("m=", "a=rtpmap", "a=fmtp")):
        print("   ", l)
PY
