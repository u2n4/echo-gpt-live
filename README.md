# echo-gpt-live

Turn an Amazon Echo into the microphone and speaker of an OpenAI **gpt-live-1** voice session.
Say "Alexa, call Sol", and a full-duplex WebRTC call runs between the Echo and OpenAI with this
Go bridge in the middle: Opus audio forwarded verbatim both ways, transcripts on a control
socket, a cheap router model for tools (web search, weather, prayer times, notes, memory), and hard money
guards because gpt-live-1 bills every minute the line is open.

Everything in this repository was measured on a real Echo Dot 5 over 28+ calls; the numbers in
the comments are those measurements, not guesses.

> **بالعربي:** جسر يخلي الـEcho Dot ميكروفون وسماعة لصوت OpenAI الحي. تقول «أليكسا، كلّم سول»
> وتصير مكالمة كاملة (تسمع وتتكلم بنفس الوقت) مع أدوات: بحث، طقس، ملاحظات، ذاكرة. فيه حراسة
> للفلوس لأن الخدمة تحاسب بالدقيقة. الإعداد كامل تحت، بالإنجليزي.

## How it works

```
Echo Dot ──(Alexa cloud)──► AWS Lambda (lambda/index.mjs)
                              │  Alexa.RTCSessionController: the Echo's SDP offer, signed with HMAC
                              ▼
                     HTTPS ingress to your Linux box (Tailscale Funnel here)
                              │
                              ▼
                     cmd/bridge  ◄──WebRTC/Opus──►  Echo            (internal/bridge/peer.go)
                     cmd/bridge  ◄──WebRTC/Opus──►  OpenAI gpt-live-1 (internal/openai/live.go,
                              │                                       internal/bridge/liveleg.go)
                              └── control websocket: transcripts, delegation → router model + tools
```

- The Lambda is the only cloud component and carries **no media**: it forwards the offer/answer
  text and the session lifecycle directives to the bridge.
- The bridge answers the Echo, dials OpenAI, and forwards RTP packets verbatim (no transcoding).
  While the OpenAI leg is coming up it **holds** every packet the Echo sends and flushes them the
  moment the session is ready, so the first words are not lost.
- A 1 fps black H.264 stream keeps Amazon's camera-category session alive (Amazon tears a
  session down after 26-28 s without video).
- Questions that need the outside world are delegated by the voice model to a router model
  (`gpt-4.1-mini`, `--search-model`) that can call tools: free web search, opening a page the
  search found (`open_result`, by result number only, never by address), weather, prayer times,
  host status, today's spend, notes on your PC (via `cmd/vaultd`), and a plain-text memory file. When the
  words call for it (a price, a score, the news) the search runs before the router is asked, so
  the first round already has results. `cmd/answertest` measures this path without a call.
- Goodbye phrases end the call, and so does a goodbye in any other words: the router has an
  `end_call` tool, guarded by a check that the words really contain a farewell. Idle, a maximum
  call length and a daily budget end it too. A ledger of open OpenAI sessions on disk lets a
  restarted process close what a crashed one left.

## What you need

| Thing | Notes |
|---|---|
| An Echo that supports `Alexa.RTCSessionController` | Tested on Echo Dot 5. Device language English (US); this interface is not offered in Arabic. |
| Amazon developer account + a Smart Home skill | Discovery advertises one endpoint (CAMERA category by default; `lambda/set-category.sh` switches). |
| AWS account for the Lambda | Node.js 22+ (`nodejs22.x`), us-east-1 for en-US. `lambda/deploy.sh` creates the role, function and env. |
| A Linux host reachable over HTTPS | Only signaling goes through it (a few KB per call). Tailscale Funnel is what this project uses; any HTTPS reverse proxy to `127.0.0.1:8787` works. Media is direct UDP from the host. |
| Go 1.27.3+ on the host | Pion WebRTC v4. `go build ./...` |
| An OpenAI API key with access to `gpt-live-1` | Passed via `OPENAI_API_KEY` or stored in `~/.config/acv/openai.key` (never in git). |

Cost: `gpt-live-1` is a paid realtime model that bills **$0.05 per minute of wall clock** while a
session is open, with a 15 s minimum per call. Silence bills at the same rate as speech until the
session is hung up. The router model (`gpt-4.1-mini`) and free search add a few hundred tokens per
delegated question.

## Setup

### 1. Secrets and config on the host

```bash
mkdir -p ~/.config/acv && chmod 700 ~/.config/acv
echo -n 'sk-...'            > ~/.config/acv/openai.key     # OpenAI API key (or set OPENAI_API_KEY)
openssl rand -hex 32        > ~/.config/acv/signing.key    # shared HMAC key, also set on the Lambda
echo 'https://your-host.example.com' > ~/.config/acv/funnel-url   # public HTTPS base URL of the bridge
chmod 600 ~/.config/acv/*
```

### 2. Build and run the bridge

```bash
go build -o bin/bridge ./cmd/bridge          # on the host; or from a workstation: bash scripts/sync.sh <ssh-host>
cp deploy/bridge.env.example bridge.env      # or copy .env.example; edit flags, place, coordinates, timezone
mkdir -p ~/.config/systemd/user && cp deploy/bridge.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now bridge
```

`scripts/sync.sh <ssh-host>` requires an SSH host argument (there is no default host) and builds
all binaries under `cmd/*` on that remote Linux host.

Environment variables (`.env.example` and `deploy/bridge.env.example`) configure the bridge and
keep personal settings out of the code:

- `OPENAI_API_KEY`: OpenAI API key (if not using `--openai-key-file` / `~/.config/acv/openai.key`).
- `BRIDGE_FLAGS`: CLI flags passed to `bin/bridge` by `deploy/bridge.service`.
- `BRIDGE_PLACE`: location description included in the assistant prompt (`--place`).
- `BRIDGE_LAT`, `BRIDGE_LON`: coordinates for the weather and prayer-time tools (`--lat`, `--lon`; `0` disables).
- `BRIDGE_TIMEZONE`: IANA timezone name (`--timezone`, defaults to host local zone).

Example `bridge.env`:

```
BRIDGE_FLAGS=--video sendonly --video-fps 1 --idle-close 60s --max-live 10m --daily-budget-usd 2.0 --tools --web-search=free --delegation client
BRIDGE_PLACE="the neighbourhood and city, written the way you would say it (City)"
BRIDGE_LAT=0.0
BRIDGE_LON=0.0
BRIDGE_TIMEZONE=Asia/Riyadh
```

`bin/bridge -h` lists every flag with the measurement behind its default. Notice that `--greeting`
defaults to empty (off) and should remain off unless testing. Other flags people change:
`--instructions` (her personality, Gulf Arabic by default), `--farewell` (phrases that end
the call), `--memory-file`, `--vault-url` + `cmd/vaultd` for notes, `--home-endpoints` for
read-only things on your LAN, `--record-dir` to keep both audio directions as Ogg/Opus.

### 3. Deploy the Lambda and the skill

```bash
aws login --profile alexa-chatgpt-voice --region us-east-1     # or any profile; edit lambda/deploy.sh
bash lambda/deploy.sh                                          # creates/updates the function and its env
```

The Lambda is configured via environment variables:
- `ACV_UBUNTU_URL`: HTTPS base URL of the bridge (required).
- `ACV_SIGNING_KEY`: shared HMAC secret matching `signing.key` (required).
- `ACV_FRIENDLY_NAME`: invocation name shown in the Alexa app (optional, default `"ChatGPT Voice"`).
- `ACV_ENDPOINT_ID`: stable Alexa endpoint ID (optional, default `"acv-chatgpt-voice-1"`).
- `ACV_DISPLAY_CATEGORIES`: comma-separated Alexa display categories (optional, default `"CAMERA"`).
- `ACV_ENDPOINT_HEALTH`: set to `0` to omit the `Alexa.EndpointHealth` capability (optional).

Point your Smart Home skill at the function ARN, enable the skill in the
Alexa app, run device discovery, then say **"Alexa, call &lt;friendly name&gt;"**.

### 4. Prove it without touching the Echo

```bash
bash scripts/preflight.sh 20          # echosim -> a second bridge on a free port -> real OpenAI, then hangs up
bash scripts/prove.sh 5 20            # five sessions: audio path, seq/ts rebase, control-socket recovery
bin/openaitest --burst 3s --duration 20s   # one OpenAI session from the host, transcript in the run log
```

Every harness picks a port that is proven free (`scripts/lib/safe-port.sh`) and refuses to start
beside the live bridge on 8787, because a simulator that reaches the live assistant spends money.

## Money guards

- `--daily-budget-usd`: refuses to open a session once today's spend (counted on disk per UTC
  day, `--budget-file`) plus one maximum call would pass the limit.
- `--max-live` (10 min default) and `--idle-close` (silence, driven by the transcript) hang up.
- Goodbye: built-in Arabic and English phrases, `--farewell` to change, plus the router's
  `end_call` for a goodbye said in other words (it refuses words with no farewell in them, so
  "خلاص" or "thanks" alone never hang up). The transport is dropped after she finishes her
  goodbye; Alexa ends the Echo's session 6-8 s later.
- The ledger (`--ledger`) records every open OpenAI session with its start time and pid. A sweep
  runs every 5 minutes and at shutdown; a restarted process closes sessions a crashed one left.
- The test tools hang up on exit and never bind the live port.

## Measured facts worth knowing

- Call start (ms after the Echo's offer): answer 140 · Echo's first audio 1900-2200 · OpenAI
  session created 600-900 (2300 on the first call after a restart when `--fork` is on) · OpenAI
  leg connected ~1300 after that · her first packet ~500 later. Held Echo packets: 0-118, 0 dropped.
- OpenAI's first RTP packet is a probe with random seq/timestamp; the real stream restarts on the
  same SSRC. The bridge widens the SRTP replay window on that leg and rebases seq/ts once so the
  Echo sees one continuous timeline (a backward restart used to make every packet drop).
- `session.output_audio.delta` streams silence at 50 packets/s; voice is a payload > 90 bytes.
- The control websocket can go silent without an error; a 10 s stall watcher reattaches it.
- Alexa delivers directives at least once (identical offer twice, 1.17 s apart): `/rtc/offer` is
  idempotent and serialised.
- Audio sent to OpenAI before `session.started` is transcribed (measured 2026-09-16).

## Repository layout

```
cmd/bridge       the service (main.go: flags, call lifecycle; assist.go/confirm.go: delegation + spoken yes/no)
cmd/answertest   router + tool latency benchmark without opening a live voice call
cmd/echosim      an Alexa-shaped WebRTC client for tests without an Echo
cmd/openaitest   one OpenAI live session from the command line, with timing and transcript
cmd/vaultd       tiny HTTP agent that serves your notes folder to the bridge (search + append)
cmd/rtcdiag      the original Echo-side experiment harness
internal/bridge  Pion peer for the Echo (peer.go), the OpenAI leg (liveleg.go), hold/pacer/sequence rebase, farewell detector, recording
internal/openai  live session client, sideband websocket, budget, ledger, router model
internal/tools   web search, weather, prayer times, host status, spend, notes, memory tools
internal/memory  the plain-text memory file
internal/alexa   HMAC-signed signaling server
lambda/          the Alexa Smart Home Lambda and its deploy script
deploy/          systemd unit and a sample env file
scripts/         sync/build, preflight, prove, call reports, log followers
```

## Privacy

Nothing you say is written to disk by default. `--log-transcript` (off) puts the transcript in
the run log; `--record-dir` (off) keeps the audio. The memory file is plain text you can read and
edit. Search queries go to the free search backends; the voice goes to OpenAI.

## Status

Measured and used daily on one Echo Dot 5. In the 2026-11-05 public-readiness refresh, Go and Pion
dependencies were updated and verified with offline unit tests (`go test -count=1 ./...`),
`go vet ./...`, and `govulncheck ./...`; **it was not tested live against an Echo or OpenAI after
this refresh**. Released under the MIT License (`LICENSE`).
