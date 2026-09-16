#!/usr/bin/env bash
# Local loopback matrix: echosim (Amazon-like offer) -> rtcdiag, one run per video mode.
# Usage: scripts/localtest.sh [seconds] [extra rtcdiag flags...]
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
SECS="${1:-12}"; shift || true
mkdir -p runs

# rtcdiag and the LIVE BRIDGE share a default listen address. Started without --listen, rtcdiag
# silently fails to bind and echosim - whose own default is that same address - sends its offer to
# the live assistant, which opens a real GPT-Live session and spends money. That happened once
# already. Both sides are pinned to a port proven free before anything starts.
source "$ROOT/scripts/lib/safe-port.sh"
DIAG_PORT="$(pick_free_port 8811)"
assert_not_live "$DIAG_PORT"
echo "diagnostics on 127.0.0.1:$DIAG_PORT (the live bridge is on 8787 and is not being touched)"

for mode in inactive reject sendonly recvonly; do
  name="local-$mode-$(date -u +%H%M%S)"
  ./bin/rtcdiag --run "$name" --listen "127.0.0.1:$DIAG_PORT" --udp-port 50000 --stun "" --video "$mode" --audio reflect --hold-after-disconnect 2s "$@" > "runs/$name.stdout" 2>&1 &
  RD=$!
  sleep 1
  # Prove rtcdiag is the thing on that port before sending it an offer.
  if ! ss -lnt "sport = :$DIAG_PORT" 2>/dev/null | grep -q ":$DIAG_PORT"; then
    echo "rtcdiag never listened on $DIAG_PORT; refusing to send an offer that could land elsewhere" >&2
    kill -INT $RD 2>/dev/null; wait $RD 2>/dev/null
    exit 3
  fi
  echo "=============== video=$mode"
  ./bin/echosim --url "http://127.0.0.1:$DIAG_PORT" --duration "${SECS}s" 2>"runs/$name.sim.err" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("answer_ms", d["answer_ms"], "| ice", d["ice_states"], "| audio out/in", d["audio_pkts_sent"], d["audio_pkts_received"], "| video in", d["video_pkts_received"], "| first_in_ms", d["first_audio_in_ms_after_offer"])
for l in d["answer_lines"]: print("   ", l)
'
  kill -INT $RD 2>/dev/null; wait $RD 2>/dev/null
  python3 - "runs/$name/events.jsonl" <<'PY'
import json,sys
evs=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
keep=("remote_track","local_sender","selected_pair","first_rtp_in","first_video_rtp_in","peer_connection_state","dtls_state","alexa_session_disconnected","session_close","audio_file_error","video_file_error","video_frame_sent","ice_gathering_complete","ice_gathering_timeout","answer_summary","rtcp_in")
seen={}
for e in evs:
    if e["ev"] in keep:
        if e["ev"]=="rtcp_in":
            k=(e.get("side"),e.get("type")); seen[k]=seen.get(k,0)+1; continue
        if e["ev"]=="video_frame_sent" and e.get("n",0)>1: continue
        print("   ", e["mono_ms"], e["ev"], {k:v for k,v in e.items() if k not in ("t","mono_ms","ev","sess","lines")})
print("    rtcp_in counts:", seen)
last=[e for e in evs if e["ev"]=="stats_final"]
if last: print("    stats_final:", {k:v for k,v in last[-1].items() if k in ("in_pkts","out_audio","in_audio","pair","ice","dtls","pc")})
PY
done
