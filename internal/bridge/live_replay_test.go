package bridge

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// Exercise encrypted packets through the actual pinned SRTP implementation,
// including the sequence jump recorded on the failed call. A counter or a
// mock receiver would not reproduce the layer that discarded the audio.
func TestLiveReplayWindowAcceptsObservedSequenceRestart(t *testing.T) {
	for _, profile := range []srtp.ProtectionProfile{
		srtp.ProtectionProfileAes128CmHmacSha1_80,
		srtp.ProtectionProfileAeadAes128Gcm,
	} {
		saltLen, err := profile.SaltLen()
		if err != nil {
			t.Fatal(err)
		}
		key, salt := make([]byte, 16), make([]byte, saltLen)
		newContext := func(window uint) *srtp.Context {
			c, err := srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(window))
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		sender, oldReceiver, receiver := newContext(liveReplayWindow), newContext(64), newContext(liveReplayWindow)
		var replay []byte
		for i, seq := range []uint16{28048, 19331, 19332, 19333} {
			p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 3595739525, SequenceNumber: seq, Timestamp: 903880968 + uint32(i)*960}, Payload: []byte{0xf8, 0xff, 0xfe}}
			plain, err := p.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := sender.EncryptRTP(nil, plain, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, oldErr := oldReceiver.DecryptRTP(nil, cipher, nil)
			if (i == 0) != (oldErr == nil) {
				t.Fatalf("default window did not reproduce first-packet-only failure: seq=%d err=%v", seq, oldErr)
			}
			got, err := receiver.DecryptRTP(nil, cipher, nil)
			if err != nil {
				t.Fatalf("valid unseen sequence %d discarded: %v", seq, err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatal("decrypted audio changed")
			}
			replay = cipher
		}
		if _, err := receiver.DecryptRTP(nil, replay, nil); err == nil {
			t.Fatal("an actual duplicate was accepted")
		}
		// A forged unseen packet must not poison the replay window before the
		// original authenticated packet arrives.
		p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 3595739525, SequenceNumber: 19334}, Payload: []byte{0xf8, 0xff, 0xfe}}
		plain, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		cipher, err := sender.EncryptRTP(nil, plain, nil)
		if err != nil {
			t.Fatal(err)
		}
		forged := append([]byte(nil), cipher...)
		forged[len(forged)-1] ^= 1
		if _, err := receiver.DecryptRTP(nil, forged, nil); err == nil {
			t.Fatal("forged audio was accepted")
		}
		if _, err := receiver.DecryptRTP(nil, cipher, nil); err != nil {
			t.Fatalf("forgery poisoned valid packet: %v", err)
		}
	}
}
