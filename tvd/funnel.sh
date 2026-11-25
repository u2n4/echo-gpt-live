#!/usr/bin/env bash
# Add or remove the public /tv path on the existing Tailscale Funnel (run on the host).
#   tvd/funnel.sh add      /tv -> tvd on 127.0.0.1:8831
#   tvd/funnel.sh remove   take /tv away again
# Only ever changes /tv. Never runs "tailscale funnel reset" and never rewrites "/" (the bridge):
# it saves the serve config first and afterwards checks that every other handler is unchanged.
set -euo pipefail
cmd="${1:?usage: funnel.sh add|remove}"
URL="$(cat "$HOME/.config/acv/funnel-url")"
SNAP="$HOME/.config/tvd/serve-before-$(date -u +%Y%m%dT%H%M%SZ).json"
[ -e "$SNAP" ] && { echo "$SNAP exists, refusing"; exit 1; }
(umask 077; tailscale serve status --json > "$SNAP")
echo "saved serve config -> $SNAP"

handlers() {  # the whole serve config minus the /tv handler, canonical JSON, from stdin
  python3 -c '
import json, sys
cfg = json.load(sys.stdin)
for host, v in (cfg.get("Web") or {}).items():
    hs = v.get("Handlers") or {}
    for path in [p for p in hs if p.rstrip("/") == "/tv"]:
        del hs[path]
print(json.dumps(cfg, sort_keys=True, indent=1))
'
}
before="$(handlers < "$SNAP")"

case "$cmd" in
  add)    sudo -n tailscale funnel --bg --https=443 --set-path=/tv http://127.0.0.1:8831 ;;
  remove) sudo -n tailscale funnel --https=443 --set-path=/tv off ;;
  *) echo "usage: funnel.sh add|remove"; exit 2 ;;
esac

after="$(tailscale serve status --json | handlers)"
echo "--- serve config now"
tailscale serve status --json | python3 -c '
import json, sys
for host, v in (json.load(sys.stdin).get("Web") or {}).items():
    for path, h in (v.get("Handlers") or {}).items():
        print(" ", host, path, "->", h.get("Proxy"))
'
if [ "$before" != "$after" ]; then
  echo "OTHER HANDLERS CHANGED (they must not). Before:"; echo "$before"; echo "After:"; echo "$after"
  echo "Nothing was rolled back automatically; compare with $SNAP."
  exit 1
fi
echo "other handlers unchanged"
echo "public /health (bridge): $(curl -s -o /dev/null -w '%{http_code}' -m 10 "$URL/health")"
if [ "$cmd" = add ]; then
  tv="$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST "$URL/tv/cmd" -d '{}')"
  echo "public unsigned POST /tv/cmd: $tv (401 = reached tvd and refused, as it should)"
  [ "$tv" = "401" ] || { echo "unexpected; check the mapping"; exit 1; }
fi
