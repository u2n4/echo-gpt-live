#!/usr/bin/env bash
# One guard, sourced by every script that starts rtcdiag, so the fix cannot rot in one copy while
# the other copies keep the hole.
#
# THE INCIDENT THIS EXISTS FOR, 12 September 2026:
#
#   rtcdiag and the live bridge have the SAME default listen address, 127.0.0.1:8787. A diagnostic
#   script started rtcdiag without --listen, rtcdiag could not bind because the bridge already had
#   the port, the script did not check, and echosim - whose own default is that same address -
#   sent its offer to the LIVE ASSISTANT. It opened a real GPT-Live session, spent real money, and
#   the answer it measured came from the wrong build.
#
#   scripts/fmtp-check.sh was fixed. scripts/localtest.sh and scripts/run.sh were not, and were
#   still able to do exactly the same thing today. A 42-agent audit found them.
#
# Usage:
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/safe-port.sh"
#   DIAG_PORT="$(pick_free_port 8801)"      # dies if it cannot find one
#   ./bin/rtcdiag --listen "127.0.0.1:$DIAG_PORT" ...
#   ./bin/echosim --url "http://127.0.0.1:$DIAG_PORT" ...

# port_busy says whether anything is listening on a loopback TCP port.
port_busy() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -lnt "sport = :$port" 2>/dev/null | grep -q ":$port"
  else
    # No ss: refuse rather than guess. Guessing here is how the incident happened.
    echo "safe-port: ss is not available, so I cannot prove port $port is free; refusing" >&2
    return 0
  fi
}

# pick_free_port prints the first free port at or above the one given, and exits the script if
# every candidate is taken. It NEVER falls through to "use it anyway".
pick_free_port() {
  local start="${1:-8801}" port
  for port in $(seq "$start" $((start + 20))); do
    if ! port_busy "$port"; then
      echo "$port"
      return 0
    fi
  done
  echo "safe-port: no free port in $start..$((start + 20)); refusing to run so this cannot reach the live bridge" >&2
  exit 3
}

# assert_not_live refuses to continue if the port is one the production service uses. Belt and
# braces: pick_free_port already avoids a busy port, but naming the live port explicitly means a
# script that hardcodes it is stopped rather than merely unlucky.
assert_not_live() {
  local port="$1"
  if [ "$port" = "8787" ]; then
    echo "safe-port: 8787 is the live bridge's port; a diagnostic must never use it" >&2
    exit 3
  fi
}
