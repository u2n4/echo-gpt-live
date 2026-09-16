package tools

// What the assistant has cost, in the currency the owner actually thinks in.
//
// He was surprised by a bill once, and the number that surprised him already existed on disk: the
// budget guard writes today's connected seconds and session count after every call. Nothing had to
// be collected. Something had to be asked.
//
// The answer is given in Saudi riyals first and dollars second. The riyal has been pegged at 3.75 to
// the dollar since 1986, so this is arithmetic rather than a rate lookup, which means it needs no
// network call, cannot be stale, and cannot fail.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// SARPerUSD is the Saudi riyal's peg. Fixed by SAMA since 1986; it is not a market rate and does not
// need fetching.
const SARPerUSD = 3.75

// spendFile mirrors what internal/openai writes. Kept as its own small struct rather than imported,
// so a tool reading a file can never drag the billing package into the audio process's hot path.
type spendFile struct {
	Day      string  `json:"day"`
	Seconds  float64 `json:"seconds"`
	Sessions int     `json:"sessions"`
	Updated  string  `json:"updated"`
}

// Spend reports today's GPT-Live spending and what is left of the ceiling.
//
// ratePerMinuteUSD and limitUSD come from the same flags the assistant runs with, so the spoken
// answer cannot drift from the guard that actually enforces the limit.
//
// running reports how long the session that is billing RIGHT NOW has been open, or zero when none
// is. Without it the answer is wrong in exactly the moment anyone asks it: the budget file records
// fifteen seconds when a call opens and the real duration only when it closes, so five minutes into
// a call the file says one cent while a quarter of a dollar has accrued.
func Spend(path string, ratePerMinuteUSD, limitUSD float64, running func() time.Duration) *Tool {
	return &Tool{
		Name: "api_spend",
		Description: "How much the assistant has cost today and how much of the daily limit is " +
			"left. Answers questions like 'how much have I spent', 'كم صرفت اليوم', 'كم باقي', " +
			"'how much is left'. Returns riyals and dollars.",
		Access:  Read,
		Ambient: true,
		Timeout: 2 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			live := time.Duration(0)
			if running != nil {
				live = running()
			}
			liveUSD := live.Minutes() * ratePerMinuteUSD

			raw, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					return spentSoFar(liveUSD, live), nil
				}
				return "", fmt.Errorf("the spend counter could not be read: %w", err)
			}
			var f spendFile
			if err := json.Unmarshal(raw, &f); err != nil {
				return "", fmt.Errorf("the spend counter is not readable JSON: %w", err)
			}
			// A counter left over from another day is not today's spending. The day is UTC because
			// that is the boundary the guard actually enforces on, and saying something different
			// from what is enforced is how a person ends up surprised twice.
			if f.Day != time.Now().UTC().Format("2006-01-02") {
				return "Nothing recorded since the counter last reset. " +
					spentSoFar(liveUSD, live) + " The last record before that is from " + f.Day + ".", nil
			}

			usd := f.Seconds/60*ratePerMinuteUSD + liveUSD
			sar := usd * SARPerUSD
			minutes := f.Seconds/60 + live.Minutes()

			if limitUSD <= 0 {
				return fmt.Sprintf(
					"Today: %.2f riyals, about %.2f dollars, over %d calls and %.0f minutes of talking. No daily limit is set.",
					sar, usd, f.Sessions, minutes), nil
			}
			leftUSD := limitUSD - usd
			if leftUSD < 0 {
				leftUSD = 0
			}
			return fmt.Sprintf(
				"Today: %.2f riyals of a %.2f riyal limit, so %.2f riyals left. That is %.2f dollars spent over %d calls and %.0f minutes of talking%s.",
				sar, limitUSD*SARPerUSD, leftUSD*SARPerUSD, usd, f.Sessions, minutes, thisCall(live)), nil
		},
	}
}

// spentSoFar answers when there is no settled record yet but a call may still be running.
func spentSoFar(liveUSD float64, live time.Duration) string {
	if live <= 0 {
		return "Nothing has been spent today."
	}
	return fmt.Sprintf("This call so far is %.2f riyals, about %.2f dollars, over %.0f minutes.",
		liveUSD*SARPerUSD, liveUSD, live.Minutes())
}

func thisCall(live time.Duration) string {
	if live <= 0 {
		return ""
	}
	return fmt.Sprintf(", %.0f of them on this call, which is still running", live.Minutes())
}
