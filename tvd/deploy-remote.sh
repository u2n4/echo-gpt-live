#!/usr/bin/env bash
# Put the TV remote by voice live in the bridge (run from the Windows repo, Git Bash).
#   tvd/deploy-remote.sh            back up, build, add --tv-url, restart the bridge, check it
#   tvd/deploy-remote.sh rollback   put the backed-up binary and bridge.env back, restart
# Refuses to restart while a call is open (the ledger of billed sessions is not empty): a
# restart in the middle of a call hangs up on him.
set -euo pipefail
HOST="${HOST:?set HOST to your ssh host (e.g. HOST=bridge-host)}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REMOTE_BACKUPS='~/.local/state/rtcdiag/backups'
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=8 "$HOST")

no_call_open() {
  "${SSH[@]}" 'L=~/.local/state/rtcdiag/open-live-sessions; [ ! -s "$L" ]' \
    || { echo "a call is open (the session ledger is not empty): not restarting the bridge now"; exit 1; }
}

restart_and_check() {
  no_call_open
  "${SSH[@]}" 'set -e
    systemctl --user restart bridge
    sleep 4
    systemctl --user is-active bridge
    d=$(ls -1dt ~/echo-gpt-live/runs/svc-* | head -1)
    grep -o "\"tv_remote\":[a-z]*" "$d/events.jsonl" | head -1
    U=$(cat ~/.config/acv/funnel-url)
    echo "public /health: $(curl -s -o /dev/null -w %{http_code} -m 10 $U/health)"'
}

if [ "${1:-}" = rollback ]; then
  B="$("${SSH[@]}" "ls -1dt $REMOTE_BACKUPS/tv-remote-* 2>/dev/null | head -1")"
  [ -n "$B" ] || { echo "no backup to roll back to"; exit 1; }
  no_call_open
  "${SSH[@]}" "set -e; cp '$B/bridge' ~/echo-gpt-live/bin/bridge.rollback && mv ~/echo-gpt-live/bin/bridge.rollback ~/echo-gpt-live/bin/bridge && cp '$B/bridge.env' ~/echo-gpt-live/bridge.env && echo 'restored from $B'"
  restart_and_check
  exit 0
fi

# 1. Back up what runs now. A new timestamped folder; refuse rather than overwrite.
TS="$(date -u +%Y%m%dT%H%M%SZ)"
"${SSH[@]}" "set -e; B=$REMOTE_BACKUPS/tv-remote-$TS; [ ! -e \$B ] || { echo \"\$B exists, refusing\"; exit 1; }
  (umask 077; mkdir -p \$B); cp -p ~/echo-gpt-live/bin/bridge ~/echo-gpt-live/bridge.env \$B/; echo \"backed up to \$B\""

# 2. Build on the host (the running process keeps its own copy until the restart).
bash "$ROOT/scripts/sync.sh" "$HOST" | tail -3

# 3. Turn the remote on in the service flags, once.
"${SSH[@]}" 'python3 -I - ~/echo-gpt-live/bridge.env' < "$ROOT/tvd/add_tv_flag.py"

# 4. Restart (only with no call open) and check.
restart_and_check
