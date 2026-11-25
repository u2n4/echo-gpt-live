package tools

// Prayer times and Arabic Hijri date without web search.
//
// Queries Aladhan's free, keyless timings API once per civil day at the house's coordinates and
// caches the day's timings + Hijri date. Supports configurable calculation methods (e.g. method=0
// Shia Ithna-Ashari / Ja'fari, method=4 Umm al-Qura) so Maghrib, Fajr and Midnight match the
// household's school accurately.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var prayerBase = "https://api.aladhan.com/v1/timings"

type prayerDay struct {
	timings map[string]string
	hijri   string
}

type prayerCache struct {
	mu   sync.Mutex
	day  string
	data prayerDay
}

// Prayer uses Umm al-Qura (method=4) for backwards compatibility.
func Prayer(lat, lon float64, tz string, client *http.Client) *Tool {
	return PrayerWithMethod(lat, lon, tz, 4, client)
}

// PrayerWithMethod builds the ambient prayer_times tool with a specific Aladhan method
// (0 = Shia Ithna-Ashari / Ja'fari, 4 = Umm al-Qura, etc.).
func PrayerWithMethod(lat, lon float64, tz string, method int, client *http.Client) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	loc := loadZone(tz)
	c := &prayerCache{}
	schoolNote := "Umm al-Qura"
	if method == 0 {
		schoolNote = "Shia Ithna-Ashari (Ja'fari)"
	}
	return &Tool{
		Name: "prayer_times",
		Description: fmt.Sprintf("Today's full prayer times (%s) and full Arabic Hijri date where the family lives: "+
			"Imsak, Fajr, Sunrise, Dhuhr, Asr, Maghrib, Isha, Midnight, and exact countdown until the next prayer. "+
			"Answers 'كم باقي للصلاة', 'متى المغرب', 'متى الفجر', 'متى الظهر', 'مواقيت الصلاة اليوم', 'كم التاريخ الهجري'. "+
			"Always use this instead of searching the web.", schoolNote),
		Access:  Read,
		Ambient: true,
		Timeout: 6 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			now := time.Now().In(loc)
			day := now.Format("02-01-2006")
			c.mu.Lock()
			cached := c.data
			cachedDay := c.day
			c.mu.Unlock()
			if cachedDay != day || len(cached.timings) == 0 {
				u := fmt.Sprintf("%s/%s?latitude=%.4f&longitude=%.4f&method=%d&timezonestring=%s",
					prayerBase, day, lat, lon, method, url.QueryEscape(tz))
				body, err := get(ctx, client, u, 64<<10, nil)
				if err != nil {
					return "", fmt.Errorf("the prayer service did not answer: %w", err)
				}
				var r struct {
					Code int `json:"code"`
					Data struct {
						Timings map[string]string `json:"timings"`
						Date    struct {
							Hijri struct {
								Day     string `json:"day"`
								Year    string `json:"year"`
								Weekday struct {
									Ar string `json:"ar"`
								} `json:"weekday"`
								Month struct {
									Ar string `json:"ar"`
								} `json:"month"`
							} `json:"hijri"`
						} `json:"date"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &r) != nil || r.Code != 200 || len(r.Data.Timings) == 0 {
					return "", fmt.Errorf("the prayer service sent something unreadable")
				}
				h := r.Data.Date.Hijri
				hijri := strings.TrimSpace(strings.Join(nonEmptyParts(
					h.Weekday.Ar, h.Day, h.Month.Ar, addSuffixIfSet(h.Year, " هـ"),
				), " "))
				pd := prayerDay{timings: r.Data.Timings, hijri: hijri}
				c.mu.Lock()
				c.day, c.data = day, pd
				cached = pd
				c.mu.Unlock()
			}
			return prayerSentenceAt(cached, method, now), nil
		},
	}
}

func prayerSentence(t map[string]string) string {
	return prayerSentenceAt(prayerDay{timings: t}, 4, time.Now())
}

func prayerSentenceAt(pd prayerDay, method int, now time.Time) string {
	t := pd.timings
	// Prayers considered for "next prayer" countdown (excluding Sunrise/Sunset/Midnight).
	prayers := []struct{ key, ar string }{
		{"Fajr", "الفجر"},
		{"Dhuhr", "الظهر"},
		{"Asr", "العصر"},
		{"Maghrib", "المغرب"},
		{"Isha", "العشاء"},
	}

	var parts []string
	if pd.hijri != "" {
		parts = append(parts, "التاريخ الهجري: "+pd.hijri+".")
	}

	// Build full schedule line so every prayer question is answered in one shot.
	var sched []string
	if method == 0 {
		if v := trimTime(t["Imsak"]); v != "" {
			sched = append(sched, "الإمساك "+formatArabic12h(v))
		}
	}
	fullOrder := []struct{ key, ar string }{
		{"Fajr", "الفجر"},
		{"Sunrise", "الشروق"},
		{"Dhuhr", "الظهر"},
		{"Asr", "العصر"},
		{"Maghrib", "المغرب"},
		{"Isha", "العشاء"},
	}
	for _, p := range fullOrder {
		if v := trimTime(t[p.key]); v != "" {
			sched = append(sched, p.ar+" "+formatArabic12h(v))
		}
	}
	if method == 0 {
		if v := trimTime(t["Midnight"]); v != "" {
			sched = append(sched, "منتصف الليل الشرعي "+formatArabic12h(v))
		}
	}
	if len(sched) > 0 {
		prefix := "مواقيت اليوم: "
		if method == 0 {
			prefix = "مواقيت اليوم (التوقيت الجعفري): "
		}
		parts = append(parts, prefix+strings.Join(sched, "، ")+".")
	}

	// Find next prayer and exact countdown.
	nowMins := now.Hour()*60 + now.Minute()
	nextAr, nextHHMM, diffMins := "", "", 0
	for _, p := range prayers {
		v := trimTime(t[p.key])
		if m, ok := parseHHMM(v); ok && m > nowMins {
			nextAr, nextHHMM, diffMins = p.ar, v, m-nowMins
			break
		}
	}
	if nextAr == "" {
		if fajr := trimTime(t["Fajr"]); fajr != "" {
			if m, ok := parseHHMM(fajr); ok {
				diffMins = (24*60 - nowMins) + m
				parts = append(parts, fmt.Sprintf("الصلاة الجاية الفجر بكره الساعة %s (باقي %s).",
					formatArabic12h(fajr), arabicCountdown(diffMins)))
			} else {
				parts = append(parts, "الصلاة الجاية الفجر بكره "+formatArabic12h(fajr)+".")
			}
		}
	} else {
		parts = append(parts, fmt.Sprintf("الصلاة الجاية %s الساعة %s (باقي %s).",
			nextAr, formatArabic12h(nextHHMM), arabicCountdown(diffMins)))
	}
	return strings.Join(parts, " ")
}

func trimTime(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}

func parseHHMM(s string) (int, bool) {
	if len(s) < 5 || s[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:5])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func formatArabic12h(hhmm string) string {
	mins, ok := parseHHMM(hhmm)
	if !ok {
		return hhmm
	}
	h, m := mins/60, mins%60
	suffix := "ص"
	if h >= 12 {
		suffix = "م"
	}
	h12 := h % 12
	if h12 == 0 {
		h12 = 12
	}
	return fmt.Sprintf("%d:%02d %s", h12, m, suffix)
}

func arabicCountdown(mins int) string {
	if mins <= 1 {
		return "أقل من دقيقة"
	}
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d دقيقة", m)
	case m == 0:
		if h == 1 {
			return "ساعة"
		}
		if h == 2 {
			return "ساعتين"
		}
		return fmt.Sprintf("%d ساعات", h)
	default:
		hPart := fmt.Sprintf("%d ساعات", h)
		if h == 1 {
			hPart = "ساعة"
		} else if h == 2 {
			hPart = "ساعتين"
		}
		return fmt.Sprintf("%s و%d دقيقة", hPart, m)
	}
}

func loadZone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("+03", 3*60*60)
}

func nonEmptyParts(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func addSuffixIfSet(s, suffix string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return strings.TrimSpace(s) + suffix
}
