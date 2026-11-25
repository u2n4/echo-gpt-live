#!/usr/bin/env bash
# Everything measurable about one real Echo call, in one command.
#
# Run it after a call and it finds the newest recording, decodes both directions verbatim, and
# prints the four things that decide what the owner heard: the codec modes actually used, the
# spectrum, the arrival and departure rhythm, and Amazon's own view of loss and jitter.
#
# Usage: scripts/call-report.sh [session-id-prefix]   (default: the newest recording)
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
REC="$HOME/.local/state/rtcdiag/rec"
LOG="$HOME/.local/state/rtcdiag/bridge.log"

if [ $# -ge 1 ]; then
  DIR="$(find "$REC" -maxdepth 1 -type d -name "$1*" | head -1)"
else
  DIR="$(ls -dt "$REC"/*/ 2>/dev/null | head -1)"
fi
[ -n "${DIR:-}" ] && [ -d "$DIR" ] || { echo "no recording found under $REC" >&2; exit 1; }
SESS="$(basename "$DIR" | cut -c1-8)"
echo "################ call $SESS ################"
echo "recording: $DIR"
echo

for f in from-openai from-echo; do
  [ -f "$DIR/$f.ogg" ] || { echo "missing $f.ogg"; continue; }
  case "$f" in
    from-openai) what="SOL — exactly what the Echo was asked to play";;
    *)           what="MIC — exactly what the Echo's microphone sent";;
  esac
  echo "======== $what ========"
  python3 scripts/oggdump.py "$DIR/$f.ogg" 0 | tail -3
  ffmpeg -v error -i "$DIR/$f.ogg" -ar 48000 -ac 1 "/tmp/$SESS-$f.wav" -y
  python3 scripts/audio-report.py "/tmp/$SESS-$f.wav"
  echo
done

echo "======== rhythm: what arrived, and what we sent ========"
grep -h "\"sess\":\"$SESS\"" "$LOG" 2>/dev/null | grep '"ev":"pace_' | python3 -c '
import json, sys
rows = {}
for line in sys.stdin:
    e = json.loads(line)
    rows.setdefault(e["ev"], []).append(e)
order = ["pace_echo", "pace_openai", "pace_to_echo"]
label = {"pace_echo": "Echo -> us   ", "pace_openai": "OpenAI -> us ", "pace_to_echo": "us -> Echo   "}
for ev in order:
    for e in rows.get(ev, []):
        print("  %s pkts=%4d %5.1f/s median=%6.2f p95=%7.2f max=%8.2f burst=%5.1f%% starved=%5.1f%%" % (
            label[ev], e["pkts"], e["pkts_per_s"], e["gap_median_ms"], e["gap_p95_ms"],
            e["gap_max_ms"], 100 * e["burst_frac"], 100 * e["starved_frac"]))
    if rows.get(ev):
        print()
'

echo "======== the pacer's own report ========"
grep -h "\"sess\":\"$SESS\"" "$LOG" 2>/dev/null | grep '"ev":"pacer_closed"' | python3 -c '
import json, sys
for line in sys.stdin:
    e = json.loads(line)
    print("  " + "  ".join(f"{k}={v}" for k, v in e.items()
                           if k not in ("t", "mono_ms", "ev", "sess", "note")))
' || echo "  (no pacer in this call)"
echo

echo "======== transport, from Amazon's receiver reports ========"
python3 scripts/loss-report.py "$LOG" "$SESS" 2>/dev/null | tail -8
