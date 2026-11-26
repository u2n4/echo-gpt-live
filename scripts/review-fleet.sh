#!/usr/bin/env bash
# Run the external review fleet: every brief in docs/review-briefs/ goes to one model.
#
# grok-*.md  -> Grok Build CLI, grok-4.6, high reasoning effort, web search ON (its whole job is to
#               check claims against live documentation rather than memory).
# codex-*.md -> Codex CLI, gpt-6-astra, high reasoning effort, read-only sandbox (it reviews the Go
#               source and must not write to the repo).
#
# Grok Build 1.0.30 headless WRITES ITS ANSWER AND THEN DOES NOT EXIT: measured on a one-line
# prompt, the answer was on stdout within seconds and the process was still alive at 240 s. So a
# Grok run is not waited on; it is watched, and killed once its output file has stopped growing.
# Killing on a plain timeout instead would either truncate a long answer or waste the full cap.
#
# Never pass --debug-file to a vendor CLI on this machine: Grok Build dumped every API key from
# ~/.claude.json into such a log once already.
#
# Usage: scripts/review-fleet.sh [brief-glob]
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
BRIEFS="${1:-docs/review-briefs/*.md}"
OUT="docs/review-out"
mkdir -p "$OUT"

GROK="$HOME/.grok/bin/grok.exe"
[ -x "$GROK" ] || { echo "grok CLI missing at $GROK" >&2; exit 1; }
command -v codex >/dev/null || { echo "codex CLI missing" >&2; exit 1; }

# GROK_NO_WEB=1 turns off web search. Two runs WITH it produced nothing: each reviewer narrated a
# plan to fetch pages and then went silent until it was killed. The sources are pasted into the
# briefs instead, in docs/review-briefs/SOURCES.md.
GROK_EXTRA=()
[ "${GROK_NO_WEB:-0}" = "1" ] && GROK_EXTRA+=(--disable-web-search)
# GROK_NO_TOOLS=1 also removes file and shell access, for a brief that is entirely self-contained.
[ "${GROK_NO_TOOLS:-0}" = "1" ] && GROK_EXTRA+=(--disallowed-tools "read,write,edit,bash,glob,grep")

QUIET_S=300
MAX_S=1800
MIN_BYTES=400   # below this it has not really answered yet

watch_and_reap() {
  # $1 = pid, $2 = output file, $3 = name
  local pid="$1" f="$2" name="$3"
  local waited=0 last=-1 quiet=0
  while kill -0 "$pid" 2>/dev/null; do
    sleep 15
    waited=$((waited + 15))
    local size
    size="$(wc -c <"$f" 2>/dev/null || echo 0)"
    if [ "$size" -eq "$last" ]; then
      quiet=$((quiet + 15))
    else
      quiet=0
      last="$size"
    fi
    if [ "$size" -ge "$MIN_BYTES" ] && [ "$quiet" -ge "$QUIET_S" ]; then
      echo "  $name: answer stable at ${size}B after ${waited}s, closing it"
      kill "$pid" 2>/dev/null
      sleep 2
      kill -9 "$pid" 2>/dev/null
      return 0
    fi
    if [ "$waited" -ge "$MAX_S" ]; then
      echo "  $name: hit the ${MAX_S}s ceiling at ${size}B, closing it"
      kill -9 "$pid" 2>/dev/null
      return 1
    fi
  done
  return 0
}

pids=()
names=()
cleanup() {
  for child in "${pids[@]}"; do kill -TERM "$child" 2>/dev/null || true; done
  for child in "${pids[@]}"; do wait "$child" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for f in $BRIEFS; do
  [ -f "$f" ] || continue
  name="$(basename "$f" .md)"
  case "$name" in
    grok-*)
      echo "launching $name  (grok-4.6, effort high, web search on)"
      (
        "$GROK" --prompt-file "$(cygpath -w "$f" 2>/dev/null || echo "$f")" \
          --output-format plain --no-subagents --permission-mode dontAsk "${GROK_EXTRA[@]}" \
          -m grok-4.6 --reasoning-effort high --max-turns 40 \
          >"$OUT/$name.md" 2>"$OUT/$name.err" </dev/null &
        gp=$!
        trap 'kill -KILL "$gp" 2>/dev/null || true; exit 143' TERM INT
        watch_and_reap "$gp" "$OUT/$name.md" "$name"
        echo "$?" >"$OUT/$name.rc"
      ) &
      ;;
    codex-*)
      echo "launching $name  (gpt-6-astra, effort high, read-only)"
      (
        codex exec -m gpt-6-astra -c model_reasoning_effort=high -s read-only \
          -C "$ROOT" --skip-git-repo-check - <"$f" >"$OUT/$name.md" 2>"$OUT/$name.err"
        echo "$?" >"$OUT/$name.rc"
      ) &
      ;;
    *)
      echo "skipping $name: the filename must start with grok- or codex-" >&2
      continue
      ;;
  esac
  pids+=($!)
  names+=("$name")
done

echo "waiting for ${#pids[@]} reviewers..."
for i in "${!pids[@]}"; do
  wait "${pids[$i]}"
  n="${names[$i]}"
  rc="$(cat "$OUT/$n.rc" 2>/dev/null || echo '?')"
  bytes="$(wc -c <"$OUT/$n.md" 2>/dev/null || echo 0)"
  err="$(wc -c <"$OUT/$n.err" 2>/dev/null || echo 0)"
  echo "  $n: exit=$rc  answer=${bytes}B  stderr=${err}B"
done
echo "answers in $OUT/"
