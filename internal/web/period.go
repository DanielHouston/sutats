package web

import (
	"fmt"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

// lastDays is the most recent n of days, or all of them when there are
// fewer: the window a phone shows, where 90 days would be slivers.
func lastDays(days []model.Day, n int) []model.Day {
	if len(days) <= n {
		return days
	}
	return days[len(days)-n:]
}

// periodDates titles a run of days the way the day view titles one day:
// "Jul 23 – Oct 20, 2026", with both years when they differ.
func periodDates(days []model.Day) string {
	if len(days) == 0 {
		return ""
	}
	from, err1 := time.Parse("2006-01-02", days[0].Date)
	to, err2 := time.Parse("2006-01-02", days[len(days)-1].Date)
	if err1 != nil || err2 != nil {
		return ""
	}
	if from.Year() != to.Year() {
		return from.Format("Jan 2, 2006") + " – " + to.Format("Jan 2, 2006")
	}
	return from.Format("Jan 2") + " – " + to.Format("Jan 2, 2006")
}

// periodFacts is the summary line over a run of days, in the day view's
// order: uptime, time degraded or down, and how much of it was observed.
type periodFacts struct {
	Uptime   string // "96.05%"
	Down     string // "14h 12m"; empty when none
	Observed string // "90 days", or hours and minutes under a day
}

// periodSummary adds up a run of days. It returns nil when nothing was
// observed, so the page shows no summary rather than zeros.
func periodSummary(days []model.Day, uptime float64) *periodFacts {
	var down, observed float64
	var daysSeen int
	for _, d := range days {
		down += d.Downtime
		observed += d.Observed
		if d.Observed > 0 {
			daysSeen++
		}
	}
	if observed <= 0 || uptime < 0 {
		return nil
	}
	f := &periodFacts{Uptime: pct(uptime)}
	if down > 0 {
		f.Down = hoursMinutes(down)
	}
	if observed >= 24*3600 {
		f.Observed = fmt.Sprintf("%d days", daysSeen)
		if daysSeen == 1 {
			f.Observed = "1 day"
		}
	} else {
		f.Observed = hoursMinutes(observed)
	}
	return f
}

// hoursMinutes writes seconds as the day view does (45s, 12m, 3h 5m), and
// in days and hours from two days up (3d 14h), where hours stop reading well.
func hoursMinutes(sec float64) string {
	s := int(sec + 0.5)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", (s+30)/60)
	case s >= 48*3600:
		h := (s + 1800) / 3600
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	default:
		m := (s + 30) / 60
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
}
