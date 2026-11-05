package tools

// Prayer times without searching for them.
//
// "كم باقي للمغرب" is asked daily, and a general web search is the wrong shape:
// seconds of silence for a page about prayer instead of today's times. His
// location does not move, so this asks one free, keyless service (Aladhan,
// Umm al-Qura method for Saudi) for today's timings at his own coordinates,
// caches the civil day, and answers with the next prayer and the countdown.
//
// There is no API key because there is no account. Only coordinates and a
// timezone string leave the house, same as the weather tool.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var prayerBase = "https://api.aladhan.com/v1/timings"

type prayerCache struct {
	mu   sync.Mutex
	day  string
	data map[string]string
}

func Prayer(lat, lon float64, tz string, client *http.Client) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	c := &prayerCache{}
	return &Tool{
		Name: "prayer_times",
		Description: "Today's prayer times where he lives: Fajr, Sunrise, Dhuhr, Asr, " +
			"Maghrib, Isha, and how long until the next one. Answers 'كم باقي للصلاة', " +
			"'متى المغرب', 'مواقيت الصلاة اليوم'. Use this instead of searching the web; " +
			"it is the timings themselves, and it is far faster.",
		Access:  Read,
		Ambient: true,
		Timeout: 6 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			day := time.Now().Format("02-01-2006")
			c.mu.Lock()
			cached := c.data
			cachedDay := c.day
			c.mu.Unlock()
			if cachedDay != day || len(cached) == 0 {
				u := fmt.Sprintf("%s/%s?latitude=%.4f&longitude=%.4f&method=4&timezonestring=%s",
					prayerBase, day, lat, lon, url.QueryEscape(tz))
				body, err := get(ctx, client, u, 64<<10, nil)
				if err != nil {
					return "", fmt.Errorf("the prayer service did not answer: %w", err)
				}
				var r struct {
					Code int `json:"code"`
					Data struct {
						Timings map[string]string `json:"timings"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &r) != nil || r.Code != 200 || len(r.Data.Timings) == 0 {
					return "", fmt.Errorf("the prayer service sent something unreadable")
				}
				c.mu.Lock()
				c.day, c.data = day, r.Data.Timings
				cached = c.data
				c.mu.Unlock()
			}
			return prayerSentence(cached), nil
		},
	}
}

func prayerSentence(t map[string]string) string {
	order := []string{"Fajr", "Sunrise", "Dhuhr", "Asr", "Maghrib", "Isha"}
	names := map[string]string{
		"Fajr": "الفجر", "Sunrise": "الشروق", "Dhuhr": "الظهر",
		"Asr": "العصر", "Maghrib": "المغرب", "Isha": "العشاء",
	}
	now := time.Now().Format("15:04")
	next := ""
	for _, k := range order {
		if v := trimTime(t[k]); v != "" && v > now {
			next = k
			break
		}
	}
	if next == "" {
		return fmt.Sprintf("العشاء %s. بكره الفجر %s.", trimTime(t["Isha"]), trimTime(t["Fajr"]))
	}
	return fmt.Sprintf("%s %s. الصلاة الجاية %s %s.",
		names[next], trimTime(t[next]), names[next], trimTime(t[next]))
}

func trimTime(s string) string {
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}
