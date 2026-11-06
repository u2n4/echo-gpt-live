# Changelog

All notable changes to this project are documented in this file.

## [Unreleased] - 2026-11-05

### Final verification round - 2026-11-06
- Applied 126 of 299 fleet findings/leads and rejected 173 after checking the cited code; `APPLIED.tmp.md` records every disposition, including duplicates and speculative limitations.
- Prevented paid-generation timeout retries, stale Echo dials/cleanup and late directive redelivery; hangup 404 now ends retries immediately. Idle, maximum-age, budget, ledger and farewell protections remain enabled; greeting remains off.
- Fixed reflector shutdown, sideband Close/Reattach, scanner-producer lifetime, memory-save ordering, confirmation timer replacement, diagnostic callback publication and ICE-state collection; added offline race regressions and Linux race CI.
- Hardened signaling replay/type checks, vault append confinement with `os.Root`, remote vault TLS, URL credentials/redirect handling, partial HTTP reads, Lambda lifecycle cleanup and scoped deployment permissions. Router/backend/reflection Responses requests disable stored responses.
- Corrected live-spend initialization accounting, unknown-model cost reporting, prayer timezone, weather labels and diagnostic RTP statistics. Audio timing, framing, numbering, codec and pacing defaults are unchanged.
- Hardened offline diagnostic port ownership, temporary audio privacy, billing sweep identity matching and corrupt-file/report handling; added offline Lambda and Python regression suites.
- Refreshed Go modules; declared stable releases were already current. Pinned `golang.org/x/crypto` to verified newest upstream commit `v0.58.1-0.20261105095718-a1058eb15227` including the GO-2026-5932 fix. Verified existing Actions `v7` refs against latest checkout v7.0.1/setup-go v7.0.0 releases.
- Offline Windows build, vet, uncached tests, portable-compiler race tests and govulncheck passed; Linux/amd64 cross-build and cross-vet also passed. Real Echo audio, deployed AWS/OpenAI behavior and remote Linux runtime were not exercised. Remote vault users now need HTTPS/certificates or a TLS proxy to loopback HTTP.

### Added
- MIT `LICENSE` file.
- GitHub Actions CI workflow (`.github/workflows/ci.yml`) running `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, and `node --check lambda/index.mjs` on `ubuntu-latest` and `windows-latest`.
- Dependabot configuration (`.github/dependabot.yml`) for `gomod` and `github-actions`.
- Root `.env.example` template with empty values for bridge, Lambda, and diagnostic environment variables.
- Router latency test harness (`cmd/answertest`), `open_result`, `prayer_times`, and guarded `end_call` tools, cross-process file locking for daily budget and session ledger (`internal/openai`).

### Changed
- Upgraded Go in `go.mod` from `1.24.0` to `1.27.3` to resolve standard-library `govulncheck` findings (`GO-2026-6809`, `GO-2026-6617`, `GO-2026-6613`, `GO-2026-6612`, `GO-2026-6611`, `GO-2026-6610`, `GO-2026-6608`, `GO-2026-6607`, `GO-2026-6605`, `GO-2026-6603`).
- Updated Pion (`webrtc/v4` v4.2.26, `ice/v4` v4.4.8, `srtp/v3` v3.1.3, `dtls/v3` v3.1.15, `sctp` v1.12.1, `interceptor` v0.1.51, `rtcp` v1.2.20, `rtp` v1.10.6, `transport/v5` v5.1.4) and `golang.org/x` (`crypto` v0.58.0, `net` v0.62.0, `sys` v0.49.0, `time` v0.16.0) dependencies to current minor versions.
- Required an explicit `<ssh-host>` argument in `scripts/sync.sh` instead of defaulting to a local host alias.
- Updated `README.md` with current toolchain requirements, environment variable configuration, paid model cost notes, `scripts/sync.sh` usage, and offline verification status.

## [0.1.0] - 2026-09-16

### Added
- Initial public snapshot of `echo-gpt-live`: Go + Pion WebRTC full-duplex bridge (`cmd/bridge`) between an Amazon Echo (`Alexa.RTCSessionController`) and OpenAI `gpt-live-1`.
- Alexa Smart Home AWS Lambda signaling adapter (`lambda/index.mjs`, `lambda/deploy.sh`, `lambda/set-category.sh`).
- Hold-and-flush startup buffer, RTP sequence/timestamp rebase, SRTP replay window adjustment, and 1 fps H.264 keepalive (`internal/bridge`).
- Sideband WebSocket control channel with stall detection and automatic reattachment, daily spend budget ceiling, and persistent open-session ledger (`internal/openai`).
- Client-side tool router (`internal/tools`, `internal/memory`) and LAN Obsidian vault service (`cmd/vaultd`).
- Diagnostic and verification harnesses (`cmd/echosim`, `cmd/openaitest`, `cmd/rtcdiag`, `scripts/`).
