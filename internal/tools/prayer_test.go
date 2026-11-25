package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrimTimeKeepsHHMM(t *testing.T) {
	if got := trimTime("05:01"); got != "05:01" {
		t.Fatalf("trimTime = %q, want 05:01", got)
	}
	if got := trimTime("17:54 (EET)"); got != "17:54" {
		t.Fatalf("trimTime = %q, want 17:54", got)
	}
}

func TestPrayerSentenceNamesNextAndHijriJafari(t *testing.T) {
	pd := prayerDay{
		hijri: "الأربعاء 15 جمادى الآخرة 1448 هـ",
		timings: map[string]string{
			"Imsak": "04:45", "Fajr": "04:55", "Sunrise": "06:07",
			"Dhuhr": "11:27", "Asr": "14:28", "Maghrib": "17:03",
			"Isha": "17:50", "Midnight": "23:27",
		},
	}
	now := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	s := prayerSentenceAt(pd, 0, now)
	for _, want := range []string{"15 جمادى الآخرة 1448", "التوقيت الجعفري", "الإمساك", "المغرب", "منتصف الليل الشرعي", "الصلاة الجاية المغرب", "باقي ساعتين و3 دقيقة"} {
		if !strings.Contains(s, want) {
			t.Errorf("prayer sentence missing %q: %s", want, s)
		}
	}
}

func TestPrayerWithMethodUsesRequestedMethod(t *testing.T) {
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":200,"data":{"timings":{"Imsak":"04:45","Fajr":"04:55","Sunrise":"06:07","Dhuhr":"11:27","Asr":"14:28","Maghrib":"17:03","Isha":"17:50","Midnight":"23:27"},"date":{"hijri":{"day":"15","year":"1448","weekday":{"ar":"الاربعاء"},"month":{"ar":"جمادى الآخرة"}}}}}`))
	}))
	defer srv.Close()

	old := prayerBase
	prayerBase = srv.URL
	defer func() { prayerBase = old }()

	tool := PrayerWithMethod(24.7136, 46.6753, "Asia/Riyadh", 0, srv.Client())
	out, err := tool.Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seenQuery, "method=0") {
		t.Fatalf("expected method=0 in query, got %q", seenQuery)
	}
	if !strings.Contains(out, "جمادى الآخرة") || !strings.Contains(out, "المغرب") {
		t.Fatalf("unexpected output: %s", out)
	}
}
