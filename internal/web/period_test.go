package web

import (
	"testing"

	"github.com/DanielHouston/sutats/internal/model"
)

func TestPeriodTitle(t *testing.T) {
	days := daysEnding("2026-10-20", 90)
	if got := periodDates(days); got != "Jul 23 – Oct 20, 2026" {
		t.Errorf("dates = %q", got)
	}
	if got := periodDates(daysEnding("2027-01-10", 90)); got != "Oct 13, 2026 – Jan 10, 2027" {
		t.Errorf("dates across a new year = %q", got)
	}

	// Nothing observed: no summary at all, rather than zeros.
	if f := periodSummary(days, -1); f != nil {
		t.Errorf("empty history = %+v", f)
	}
	for i := range days {
		days[i].Observed = 86400
	}
	days[10].Downtime = 3*3600 + 300
	days[11].Downtime = 2 * 86400
	f := periodSummary(days, 0.9993)
	if f == nil || f.Uptime != "99.93%" || f.Down != "2d 3h" || f.Observed != "90 days" {
		t.Errorf("summary = %+v", f)
	}
	// A new install: hours observed, no downtime.
	fresh := []model.Day{{Date: "2026-10-20", Observed: 7*3600 + 56*60}}
	if f := periodSummary(fresh, 1); f == nil || f.Observed != "7h 56m" || f.Down != "" {
		t.Errorf("fresh install = %+v", f)
	}
	for sec, want := range map[float64]string{45: "45s", 720: "12m", 3*3600 + 300: "3h 5m", 86*3600 + 900: "3d 14h"} {
		if got := hoursMinutes(sec); got != want {
			t.Errorf("hoursMinutes(%v) = %q, want %q", sec, got, want)
		}
	}
}
