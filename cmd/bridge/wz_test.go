package main

import (
	"strings"
	"testing"
	"time"
)

func TestWhereAndWhen(t *testing.T) {
	a := &app{place: "حي الورد، مدينة النور (Noor)، بلد ما", tz: mustZone("Asia/Riyadh")}
	before := time.Now().In(a.tz)
	got := a.whereAndWhen()
	if !strings.Contains(got, "حي الورد") || !strings.Contains(got, "النور") {
		t.Errorf("the place is missing: %s", got)
	}
	now := time.Now().In(a.tz)
	if !strings.Contains(got, before.Format("2006-01-02")) && !strings.Contains(got, now.Format("2006-01-02")) {
		t.Errorf("today is missing: %s", got)
	}
	if !strings.Contains(got, before.Format("15:04")) && !strings.Contains(got, now.Format("15:04")) {
		t.Errorf("the local time is missing: %s", got)
	}
	t.Logf("%s", got)
}

// Saudi has never observed daylight saving, so a host with no tzdata must still be right.
func TestZoneFallbackIsPlusThree(t *testing.T) {
	loc := mustZone("Nowhere/Invented")
	_, off := time.Now().In(loc).Zone()
	if off != 3*60*60 {
		t.Errorf("fallback offset %d, want +03:00", off)
	}
}
