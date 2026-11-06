#!/usr/bin/env bash
# Proves the bridge's audio path and control-socket recovery against the REAL OpenAI service
# without an Echo: echosim plays the Echo, the real bridge binary answers it on a test port.
#
#   scripts/prove.sh [sessions] [seconds]      default 5 sessions of 20 s (~1.7 cents each)
#
# The first session also kicks the control socket mid-call to run the reattach path.
# Never touches the live bridge on 8787 (safe-port refuses to start beside it).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
N="${1:-5}"; SECS="${2:-20}"
[[ "$N" =~ ^[1-9][0-9]*$ && "$SECS" =~ ^[1-9][0-9]*$ ]] || { echo "sessions and seconds must be positive integers" >&2; exit 2; }
source "$ROOT/scripts/lib/safe-port.sh"
P="$(pick_free_port 8811)"; assert_not_live "$P"
OUT=/tmp/prove; mkdir -p "$OUT"
BP=""
trap 'stop_child "$BP"' EXIT
echo "test bridge on 127.0.0.1:$P; the live bridge on 8787 is not touched"
for i in $(seq 1 "$N"); do
  name="prove-$i-$(date -u +%H%M%S)"
  kick=""; [ "$i" -eq 1 ] && kick="--test-kick-sideband-after 8s"
  ./bin/bridge --run "$name" --runs-dir "$OUT" --listen "127.0.0.1:$P" --udp-port 50001 \
    --video sendonly --video-fps 1 --idle-close 60s --hold-after-disconnect 2s --delegation client \
    --daily-budget-usd 2.0 --tools --web-search=free --farewell-grace 2s --memory-file "$OUT/$name.memory.md" \
    $kick > "$OUT/$name.stderr" 2>&1 &
  BP=$!
  wait_owned_tcp "$BP" "$P" || exit 3
  timeout $((SECS + 40)) ./bin/echosim --url "http://127.0.0.1:$P" --duration "${SECS}s" --audio-file media/hello.ogg \
    > "$OUT/$name.sim.json" 2> "$OUT/$name.sim.err"
  stop_child "$BP"; BP=""
  f="$OUT/$name/events.jsonl"
  python3 - "$f" "$OUT/$name.sim.json" "$i" <<'PY'
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
sim=json.load(open(sys.argv[2]))
i=sys.argv[3]
ev=lambda k: [r for r in rows if r.get("ev")==k]
head=[r for r in rows if r.get("ev")=="live_rtp_head"]
seqs=[r["seq"] for r in head[:2]]
close=ev("live_leg_close")[-1] if ev("live_leg_close") else {}
rebased=ev("live_rtp_rebased")
restart="none"
if len(seqs)==2:
    restart="backward" if (seqs[1]-seqs[0])%65536 > 32768 else ("forward" if seqs[1]!=seqs[0]+1 else "contiguous")
print("session %s: probe seq %s -> stream seq %s (%s restart)%s"%(i, seqs[0] if seqs else "?", seqs[1] if len(seqs)>1 else "?", restart, ", REBASED to %d"%rebased[0]["new_seq"] if rebased else ""))
print("   her audio: %d packets from OpenAI, missing-audio alarms: %d"%(close.get("pkts_from_openai",0), len(ev("openai_audio_missing"))))
g=ev("greeting_played"); off=[r for r in rows if r.get("ev")=="offer_received"]
if g: print("   greeting: %d packets played at %d ms after the offer%s"%(g[0]["packets"], g[0]["mono_ms"]-off[0]["mono_ms"] if off else -1, ", gate held %d"%ev("greeting_gate_released")[0]["packets"] if ev("greeting_gate_released") else ""))
elif ev("greeting_failed"): print("   greeting FAILED: %s"%ev("greeting_failed")[0].get("err"))
print("   at the Echo side: %d packets received, seq jumps %d, ts jumps %d"%(sim.get("audio_pkts_received",0), sim.get("audio_seq_jumps",-1), sim.get("audio_ts_jumps",-1)))
print("   control socket: kicked %d, stalled %d, reattached %d, lost %d"%(len(ev("test_sideband_kick")),len(ev("live_sideband_stalled")),len(ev("live_sideband_reattached")),len(ev("live_sideband_lost"))))
if ev("live_sideband_reattached"):
    k=ev("test_sideband_kick")[0]["mono_ms"]; r=ev("live_sideband_reattached")[0]["mono_ms"]
    after=[x for x in rows if x.get("ev")=="live_sideband_event" and x["mono_ms"]>r]
    print("   reattach took %d ms; %d sideband events arrived after it"%(r-k, len(after)))
print("   billed: $%.4f"%close.get("billed_usd_estimate",0))
PY
done
