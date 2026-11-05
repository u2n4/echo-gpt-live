#!/usr/bin/env bash
# Sync this repo (minus .git/runs) to the Ubuntu build host and build both binaries.
# Usage: scripts/sync.sh <ssh-host>
set -euo pipefail
HOST="${1:?usage: scripts/sync.sh <ssh-host>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
tar --exclude=.git --exclude=runs --exclude=bin --exclude='*.pcap' -cf - . | \
  ssh -o BatchMode=yes "$HOST" 'mkdir -p ~/echo-gpt-live && tar -xf - -C ~/echo-gpt-live && cd ~/echo-gpt-live && go mod tidy >/dev/null 2>&1; for c in cmd/*/; do go build -o "bin/$(basename $c)" "./$c" || exit 1; done && echo BUILD_OK && ls -la bin/'
# Pull back go.mod/go.sum so the Windows repo matches the build host. This used to end in
# `|| true`, which meant a failed copy left the two trees disagreeing about dependencies with no
# sign that anything had happened - and every later "BUILD_OK" would be about a go.mod that is not
# the one in git. Say so instead of swallowing it.
if ! scp -q "$HOST":~/echo-gpt-live/go.mod "$HOST":~/echo-gpt-live/go.sum "$ROOT/"; then
  echo "sync: WARNING - could not pull go.mod/go.sum back from $HOST." >&2
  echo "sync: the build host may now have dependencies this repo does not record." >&2
fi
