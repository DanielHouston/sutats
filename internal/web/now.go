package web

import (
	"fmt"
	"html/template"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

// nowData feeds the "now" template: a row's current status, shown in the
// column after its history, and an optional line saying more about it.
type nowData struct {
	Status model.Status
	Text   string
}

// nowWord is the current status as the Now column says it: short enough to
// sit beside its mark in a narrow column.
func nowWord(s model.Status) string {
	switch s {
	case model.StatusOperational:
		return "OK"
	case model.StatusMaintenance:
		return "Maintenance"
	case model.StatusDegraded:
		return "Degraded"
	case model.StatusOutage:
		return "Down"
	default:
		return "No data"
	}
}

// members lists every component on the page, group by group.
func members(groups []model.Group) []model.Component {
	var out []model.Component
	for _, g := range groups {
		out = append(out, g.Components...)
	}
	return out
}

// okCount says how many of cs are operational right now: "4/5 OK".
func okCount(cs []model.Component) string {
	ok := 0
	for _, c := range cs {
		if c.Status == model.StatusOperational {
			ok++
		}
	}
	return fmt.Sprintf("%d/%d OK", ok, len(cs))
}

// isToday reports whether a bar cell's date (UTC, as the history keeps
// days) is today.
func isToday(date string) bool { return date == time.Now().UTC().Format("2006-01-02") }

// todayFill sets how far up today's cell is filled: the share of the UTC
// day gone so far, as the CSS variable the stylesheet reads.
func todayFill() template.CSS {
	return template.CSS(fmt.Sprintf("--fill: %d%%", dayGone(time.Now())))
}

// dayGone is the whole percentage of t's UTC day that has passed.
func dayGone(t time.Time) int {
	t = t.UTC()
	since := t.Sub(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
	return int(since * 100 / (24 * time.Hour))
}

// clock is the time of day (UTC) an update was made, for the Now header.
func clock(t time.Time) string { return t.UTC().Format("15:04") }
