package bridge

import "github.com/pion/webrtc/v4"

// On the 2026-09-14 silent call, OpenAI sent sequence 28048 followed by 19331,
// 19332, ... on the same SSRC. Pion's default 64-packet replay window rejected
// all of the subsequent audio as too old. Allow an unseen packet within half
// the 16-bit sequence space, while retaining authentication and duplicate
// rejection. This applies only to the OpenAI receiver, not the Alexa leg.
const liveReplayWindow = 1 << 15

func configureLiveReplayProtection(se *webrtc.SettingEngine) {
	se.SetSRTPReplayProtectionWindow(liveReplayWindow)
}
