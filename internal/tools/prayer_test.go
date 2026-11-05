package tools

import "testing"

func TestTrimTimeKeepsHHMM(t *testing.T) {
	if got := trimTime("05:01"); got != "05:01" {
		t.Fatalf("trimTime = %q, want 05:01", got)
	}
	if got := trimTime("17:54 (EET)"); got != "17:54" {
		t.Fatalf("trimTime = %q, want 17:54", got)
	}
}

func TestPrayerSentenceNamesNext(t *testing.T) {
	timings := map[string]string{
		"Fajr": "05:01", "Sunrise": "05:59", "Dhuhr": "11:57",
		"Asr": "15:17", "Maghrib": "17:54", "Isha": "18:53",
	}
	s := prayerSentence(timings)
	if s == "" {
		t.Fatal("empty prayer sentence")
	}
}
