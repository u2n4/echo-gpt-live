#!/usr/bin/env bash
# Prove that --opus-fmtp reaches the SDP answer the Echo would receive, over a real loopback.
#
# Runs on its own signaling port and its own UDP port. The first version of this script used
# rtcdiag's defaults, which are the SAME port the live bridge listens on: echosim therefore talked
# to the running assistant instead of the test process, opened a real GPT-Live session and spent
# money. Never let a diagnostic bind a port the service uses.
#
# Usage: scripts/fmtp-check.sh [fmtp-string]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
FMTP="${1:-minptime=10;useinbandfec=1;usedtx=0;stereo=0;sprop-stereo=0;maxplaybackrate=48000;maxaveragebitrate=64000}"
PORT=8799
UDP=50011

source "$ROOT/scripts/lib/safe-port.sh"
assert_not_live "$PORT"
if port_busy "$PORT" || port_busy "$UDP" udp; then
  echo "diagnostic TCP or UDP port is busy or unverified; refusing" >&2
  exit 3
fi
TMP="$(mktemp -d)"
CHILD=""
trap 'stop_child "$CHILD"; rm -rf -- "$TMP"' EXIT


RUN="fmtp-check-$(date -u +%H%M%S)"
./bin/rtcdiag --run "$RUN" --listen "127.0.0.1:$PORT" --udp-port "$UDP" --stun "" \
  --video sendonly --audio reflect --hold-after-disconnect 2s --opus-fmtp "$FMTP" >"$TMP/fmtp-rtcdiag.log" 2>&1 &
RD=$!
CHILD=$RD
wait_owned_tcp "$RD" "$PORT" || exit 3

./bin/echosim --url "http://127.0.0.1:$PORT" --duration 6s 2>"$TMP/fmtp-echosim.err" > "$TMP/fmtp-answer.json"
stop_child "$RD"; CHILD=""

python3 - "$TMP/fmtp-answer.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
print("answer_ms", d["answer_ms"],
      "| audio sent/received", d["audio_pkts_sent"], d["audio_pkts_received"],
      "| video received", d["video_pkts_received"])
for l in d["answer_lines"]:
    if l.startswith(("m=", "a=rtpmap", "a=fmtp")):
        print("   ", l)
PY
