#!/usr/bin/env bash
# Run one rtcdiag experiment with a packet capture on the UDP mux port.
# Usage: scripts/run.sh <run-name> [rtcdiag flags...]
# Example: scripts/run.sh E3-video-sendonly --video sendonly --audio file
# Stops with Ctrl-C (or `scripts/stop.sh`). Artifacts: runs/<run-name>/{events.jsonl,*.sdp,capture.pcap}
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
NAME="${1:?run name}"; shift || true
PORT=50000
for ((i=1;i<=$#;i++)); do
  if [ "${!i}" = "--udp-port" ]; then j=$((i+1)); PORT="${!j}"; fi
done
# rtcdiag's default listen address is the LIVE BRIDGE's. Without --listen it fails to bind and any
# simulator pointed at the default reaches the production assistant instead, which costs money.
source "$ROOT/scripts/lib/safe-port.sh"
DIAG_PORT="$(pick_free_port 8821)"
assert_not_live "$DIAG_PORT"
case " $* " in
  *" --listen "*) ;;                                  # caller chose one; respect it
  *) set -- "$@" --listen "127.0.0.1:$DIAG_PORT" ;;
esac
echo "rtcdiag signalling on 127.0.0.1:$DIAG_PORT"

mkdir -p "runs/$NAME"
# scoped firewall rule for the mux port (idempotent)
sudo -n ufw status 2>/dev/null | grep -q "^${PORT}/udp" || sudo -n ufw allow "${PORT}/udp" comment rtcdiag >/dev/null 2>&1 || true
# packet capture: full length, before signaling, on all interfaces (UDP mux + STUN)
sudo -n tcpdump -i any -s 0 -w "runs/$NAME/capture.pcap" "udp port $PORT or udp port 3478 or udp port 19302" >/dev/null 2>&1 &
TCPDUMP=$!
sleep 0.5
echo "[run] $NAME  pcap pid=$TCPDUMP  flags: $*"
trap 'sudo -n kill -INT $TCPDUMP 2>/dev/null; wait $TCPDUMP 2>/dev/null; echo "[run] stopped; artifacts in runs/$NAME"' EXIT INT TERM
./bin/rtcdiag --run "$NAME" --udp-port "$PORT" "$@"
