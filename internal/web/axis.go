package web

import (
	"strings"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

// monthStart reports whether the i-th day of a row opens a month, and so
// gets a mark on the axis. The first slot never does: the row starts there.
func monthStart(i int, date string) bool { return i > 0 && strings.HasSuffix(date, "-01") }

// axisSlot is one day's slot in the axis over the top bar.
type axisSlot struct {
	Date  string // the day, for the date shown when it is hovered
	Mark  bool   // a month starts here; its label gets a hairline
	Label string // month name, or Today on the last slot
	End   bool   // the last slot
}

// Labels need room: a month name takes about three days' width on a phone.
// Names that would collide are left out.
const (
	roomAfterStart = 8 // a name on the first slot needs this many slots before the next month starts
	roomBeforeEnd  = 6 // a month name this close to the end would run into Today
)

// axis lays out the labels over a row of days: each month's name over its
// first day, and the name of the month the bar starts in when there is room
// before the first month start. January also gets the year. The last slot
// says Today, right-aligned so it ends with the bar.
func axis(days []model.Day) []axisSlot {
	n := len(days)
	out := make([]axisSlot, n)
	if n == 0 {
		return out
	}
	first := n
	for i, d := range days {
		out[i].Date = d.Date
		if !monthStart(i, d.Date) {
			continue
		}
		out[i].Mark = true
		if first == n {
			first = i
		}
		if i <= n-roomBeforeEnd {
			out[i].Label = monthName(d.Date)
		}
	}
	if first >= roomAfterStart && n > roomBeforeEnd {
		out[0].Label = monthName(days[0].Date)
	}
	out[n-1].End = true
	out[n-1].Label = "Today"
	return out
}

func monthName(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return ""
	}
	if t.Month() == time.January {
		return t.Format("Jan 2006")
	}
	return t.Format("Jan")
}
