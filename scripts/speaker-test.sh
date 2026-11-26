#!/usr/bin/env bash
# Measure what the Echo actually does to audio, without anyone having to judge by ear.
#
# The bridge sends a known FULLBAND signal (an 18 s logarithmic sweep from 80 Hz to 16 kHz, Opus
# CELT fullband at 96 kbit/s CBR) instead of the assistant's voice. The Echo plays it into the room,
# its own microphone picks it up, and that return path is recorded. Measuring where the returned
# sweep stops tells us the playback bandwidth of the real speaker on the real path.
#
# WHAT THIS CANNOT DO, and the first version of this comment claimed it could: identify the
# SPEAKER's cutoff. The return path is playback, then the room, then the Echo's echo cancellation
# and microphone processing, then its own encoder, which is capped at 8 kHz. Any cutoff below
# 8 kHz therefore belongs to the chain, not necessarily to the speaker. What the return can show
# is whether energy that was SENT at a given frequency comes back at all, which is a weaker but
# still useful statement. Codex corrected the stronger one.
#
# No GPT-Live session is created, so this costs nothing.
#
# Usage: scripts/speaker-test.sh   then say "Alexa, talk to the intercom" and stay quiet.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; cd "$ROOT"
source "$ROOT/scripts/lib/safe-port.sh"
REC="$HOME/.local/state/rtcdiag/speaker-test"
SWEEP="media/sweep_18s.ogg"
[ -f "$SWEEP" ] || { echo "missing $SWEEP" >&2; exit 1; }

# The assistant must come back no matter how this exits, including a Ctrl-C or a kill, and the
# diagnostic must be gone first: both want the same signaling port, and a race there would leave the
# assistant unable to bind and the user without an assistant.
RD=""
restore() {
  stop_child "$RD"
  echo
  echo "restoring the assistant"
  systemctl --user start bridge
  sleep 1
  systemctl --user is-active bridge
}
trap restore EXIT

echo "stopping the assistant so the diagnostic can take the signaling port"
systemctl --user stop bridge

rm -rf "$REC"; mkdir -p "$REC"
RUN="speaker-test-$(date -u +%H%M%S)"
./bin/rtcdiag --run "$RUN" --udp-port 50000 --mux-ip auto \
  --video sendonly --video-fps 1 \
  --audio file --audio-file "$SWEEP" \
  --record-dir "$REC" --hold-after-disconnect 3s --max-session 3m \
  >/tmp/speaker-test.log 2>&1 &
RD=$!
sleep 2

echo
echo "  READY. Say: Alexa, talk to the intercom"
echo "  Then stay quiet and let it play for about 20 seconds, and end the call."
echo "  Waiting up to 3 minutes..."
echo

for _ in $(seq 1 180); do
  sleep 1
  if ! kill -0 "$RD" 2>/dev/null; then break; fi
  if grep -q '"ev":"alexa_session_disconnected"' "runs/$RUN/events.jsonl" 2>/dev/null; then
    sleep 5
    break
  fi
done
stop_child "$RD"
RD=""

F="$(find "$REC" -name from-echo.ogg | head -1)"
if [ -z "$F" ]; then
  echo "nothing was recorded; the session never reached this process" >&2
  tail -5 /tmp/speaker-test.log >&2
  exit 1
fi
echo "recorded: $F"
ffmpeg -v error -i "$F" -ar 48000 -ac 1 /tmp/speaker-return.wav -y
python3 scripts/audio-report.py /tmp/speaker-return.wav
echo
echo "the sweep that was played, for comparison:"
ffmpeg -v error -i "$SWEEP" -ar 48000 -ac 1 /tmp/speaker-sent.wav -y
python3 scripts/audio-report.py /tmp/speaker-sent.wav
