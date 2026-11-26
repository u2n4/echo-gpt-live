#!/usr/bin/env bash
# Run one rtcdiag experiment with a packet capture on the UDP mux port.
# Usage: scripts/run.sh <run-name> [rtcdiag flags...]
# Example: scripts/run.sh E3-video-sendonly --video sendonly --audio file
# Stops with Ctrl-C (or `scripts/stop.sh`). Artifacts: runs/<run-name>/{events.jsonl,*.sdp,capture.pcap}
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
NAME="${1:?run name}"; shift || true
[[ "$NAME" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo "run name must contain only letters, digits, - or _" >&2; exit 2; }
PORT=50000
for ((i=1;i<=$#;i++)); do
  if [ "${!i}" = "--udp-port" ]; then j=$((i+1)); PORT="${!j}"; fi
done
# rtcdiag's default listen address is the LIVE BRIDGE's. Without --listen it fails to bind and any
# simulator pointed at the default reaches the production assistant instead, which costs money.
source "$ROOT/scripts/lib/safe-port.sh"
DIAG_PORT="$(pick_free_port 8821)"
assert_not_live "$DIAG_PORT"
for arg in "$@"; do
  case "$arg" in --listen|--listen=*|--udp-port=*) echo "use the diagnostic-owned listen port and --udp-port VALUE" >&2; exit 2;; esac
done
case " $* " in *" --udp-port "*) ;; *) set -- "$@" --udp-port "$PORT";; esac
set -- "$@" --listen "127.0.0.1:$DIAG_PORT"
if port_busy "$PORT" udp; then echo "UDP port busy or unverified" >&2; exit 3; fi
echo "rtcdiag signalling on 127.0.0.1:$DIAG_PORT"

mkdir -p "runs/$NAME"
# scoped firewall rule for the mux port (idempotent)
sudo -n ufw status 2>/dev/null | grep -q "^${PORT}/udp" || sudo -n ufw allow "${PORT}/udp" comment rtcdiag >/dev/null 2>&1 || true
# packet capture: full length, before signaling, on all interfaces (UDP mux + STUN)
sudo -n tcpdump -i any -s 0 -w "runs/$NAME/capture.pcap" "udp port $PORT or udp port 3478 or udp port 19302" >"runs/$NAME/capture.log" 2>&1 &
TCPDUMP=$!
echo "[run] $NAME  pcap pid=$TCPDUMP  flags: $*"
trap 'sudo -n kill -INT $TCPDUMP 2>/dev/null; wait $TCPDUMP 2>/dev/null; echo "[run] stopped; artifacts in runs/$NAME"' EXIT INT TERM
for ((attempt=0;attempt<50;attempt++)); do
  kill -0 "$TCPDUMP" 2>/dev/null || { echo "capture failed to start" >&2; exit 3; }
  grep -q 'listening on' "runs/$NAME/capture.log" && break
  sleep 0.1
done
grep -q 'listening on' "runs/$NAME/capture.log" || { echo "capture not ready" >&2; exit 3; }
./bin/rtcdiag --run "$NAME" "$@"
