package bridge

import "strings"

import "testing"

// A shortened but structurally real answer: the Opus payload type, a second audio codec, the
// video section, and the lines that must never be touched (ICE, DTLS, mid, port).
const sampleAnswer = "v=0\r\n" +
	"o=- 42 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111 0 8\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:0\r\n" +
	"a=ice-ufrag:abcd\r\n" +
	"a=ice-pwd:secretsecretsecret\r\n" +
	"a=fingerprint:sha-256 AA:BB\r\n" +
	"a=setup:active\r\n" +
	"a=rtcp-mux\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=fmtp:0 something\r\n" +
	"a=sendrecv\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"a=mid:1\r\n" +
	"a=rtpmap:96 H264/90000\r\n" +
	"a=fmtp:96 profile-level-id=42e01f\r\n" +
	"a=sendonly\r\n"

// The whole point of rewriting the finalized SDP rather than the registered codec is that
// negotiation must be unaffected: only the one fmtp line may change.
func TestSetOpusFmtpReplacesOnlyTheOpusLine(t *testing.T) {
	out := setOpusFmtp(sampleAnswer, DefaultOpusFmtp)

	if !strings.Contains(out, "a=fmtp:111 "+DefaultOpusFmtp+"\r\n") {
		t.Fatalf("the Opus fmtp was not applied:\n%s", out)
	}
	if strings.Contains(out, "a=fmtp:111 minptime=10;useinbandfec=1\r\n") {
		t.Fatal("the old Opus fmtp is still present; it must be replaced, not duplicated")
	}
	for _, must := range []string{
		"a=fmtp:0 something\r\n",                // the other audio codec is not ours to touch
		"a=fmtp:96 profile-level-id=42e01f\r\n", // nor is video
		"a=ice-pwd:secretsecretsecret\r\n",
		"a=fingerprint:sha-256 AA:BB\r\n",
		"a=setup:active\r\n",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111 0 8\r\n",
		"a=mid:0\r\n",
		"a=sendonly\r\n",
	} {
		if !strings.Contains(out, must) {
			t.Fatalf("rewriting the fmtp destroyed %q", strings.TrimSpace(must))
		}
	}
	if a, b := strings.Count(sampleAnswer, "\r\n"), strings.Count(out, "\r\n"); a != b {
		t.Fatalf("line count changed %d -> %d; exactly one line should have been replaced", a, b)
	}
}

// Pion writes the fmtp line for us today, but an SDP without one must gain it rather than
// silently keep the bare default.
func TestSetOpusFmtpInsertsWhenAbsent(t *testing.T) {
	in := strings.Replace(sampleAnswer, "a=fmtp:111 minptime=10;useinbandfec=1\r\n", "", 1)
	out := setOpusFmtp(in, "usedtx=0")
	if !strings.Contains(out, "a=rtpmap:111 opus/48000/2\r\na=fmtp:111 usedtx=0\r\n") {
		t.Fatalf("fmtp was not inserted after the Opus rtpmap:\n%s", out)
	}
}

// An empty parameter string is the documented way to keep the browser default, and an SDP with no
// Opus at all must come back byte-identical instead of being half-edited.
func TestSetOpusFmtpLeavesSDPAloneWhenItShould(t *testing.T) {
	if got := setOpusFmtp(sampleAnswer, ""); got != sampleAnswer {
		t.Fatal("empty params must leave the SDP untouched")
	}
	noOpus := strings.NewReplacer(
		"a=rtpmap:111 opus/48000/2\r\n", "",
		"a=fmtp:111 minptime=10;useinbandfec=1\r\n", "",
	).Replace(sampleAnswer)
	if got := setOpusFmtp(noOpus, DefaultOpusFmtp); got != noOpus {
		t.Fatal("an SDP with no Opus must come back unchanged")
	}
}

// The audio section is first in the bundle; a video fmtp must never be mistaken for it, and the
// scan must stop at the next m= line.
func TestSetOpusFmtpStopsAtTheNextMediaSection(t *testing.T) {
	out := setOpusFmtp(sampleAnswer, DefaultOpusFmtp)
	vi := strings.Index(out, "m=video")
	if vi < 0 {
		t.Fatal("the video section disappeared")
	}
	if strings.Contains(out[vi:], DefaultOpusFmtp) {
		t.Fatal("the Opus parameters leaked into the video section")
	}
}

// LF-only SDP is legal even though WebRTC uses CRLF; the rewrite must not convert line endings,
// because a description that changes shape is a description that can fail to parse.
func TestSetOpusFmtpPreservesLineEndings(t *testing.T) {
	lf := strings.ReplaceAll(sampleAnswer, "\r\n", "\n")
	out := setOpusFmtp(lf, "usedtx=0")
	if strings.Contains(out, "\r\n") {
		t.Fatal("LF-only input came back with CRLF")
	}
	if !strings.Contains(out, "a=fmtp:111 usedtx=0\n") {
		t.Fatalf("fmtp not applied to LF input:\n%s", out)
	}
}
