package web

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

func daysEnding(last string, n int) []model.Day {
	end, _ := time.Parse("2006-01-02", last)
	out := make([]model.Day, n)
	for i := range out {
		out[i].Date = end.AddDate(0, 0, i-n+1).Format("2006-01-02")
	}
	return out
}

// labels renders an axis as "index:label" for the slots that have one, and
// the marker indexes.
func labels(slots []axisSlot) (string, string) {
	var ls, ms []string
	for i, s := range slots {
		if s.Label != "" {
			ls = append(ls, strconv.Itoa(i)+":"+s.Label)
		}
		if s.Mark {
			ms = append(ms, strconv.Itoa(i))
		}
	}
	return strings.Join(ls, " "), strings.Join(ms, " ")
}

func TestAxis(t *testing.T) {
	cases := []struct {
		last, labels, marks string
	}{
		// 23 Jul to 20 Oct: July is named on the first slot, each month at
		// its first day, and the last slot is Today.
		{"2026-10-20", "0:Jul 9:Aug 40:Sep 70:Oct 89:Today", "9 40 70"},
		// A month that starts three days before today keeps its mark but
		// not its name, which would run into Today.
		{"2026-10-03", "0:Jul 26:Aug 57:Sep 89:Today", "26 57 87"},
		// A month start two days in leaves no room to name the month before it.
		{"2026-10-28", "1:Aug 32:Sep 62:Oct 89:Today", "1 32 62"},
		// January carries the year.
		{"2027-02-15", "0:Nov 13:Dec 44:Jan 2027 75:Feb 89:Today", "13 44 75"},
	}
	for _, c := range cases {
		gotL, gotM := labels(axis(daysEnding(c.last, 90)))
		if gotL != c.labels || gotM != c.marks {
			t.Errorf("ending %s:\n labels %q\n want   %q\n marks  %q\n want   %q", c.last, gotL, c.labels, gotM, c.marks)
		}
	}
	if len(axis(nil)) != 0 {
		t.Error("no days, no slots")
	}
}

// Month starts are shown on the axis only; the bars stay evenly spaced.
func TestMonthsOnTheAxisOnly(t *testing.T) {
	body := get(t, testServer(t), "/").Body.String()
	if !strings.Contains(body, `class="axis"`) || !strings.Contains(body, `<span class="axis__label">Today</span>`) {
		t.Error("the axis is missing, or its Today label")
	}
	if n := strings.Count(body, "axis__slot--mark"); n < 2 {
		t.Errorf("the axis marks %d month starts, want at least 2 in 90 days", n)
	}
	if strings.Contains(body, "bar__day--mark") {
		t.Error("bar cells must not carry markers")
	}
	// Each slot carries its day. There are two axes: 90 days, and the last
	// 30 for phones.
	if n := strings.Count(body, `class="axis__slot`); n != 90+30 || strings.Count(body, `" data-date="20`) < 90+30 {
		t.Errorf("axes have %d slots, or slots without dates; want 90 and 30", n)
	}
}

// A phone shows the last 30 days, so every number beside a bar comes in
// both windows, and the 30-day one matches its cells.
func TestThirtyDayWindow(t *testing.T) {
	body := get(t, testServer(t), "/").Body.String()
	n90 := strings.Count(body, `<span class="days-90">`)
	n30 := strings.Count(body, `<span class="days-30">`)
	if n90 < 20 || n90 != n30 {
		t.Errorf("%d 90-day and %d 30-day uptimes; want a pair beside every bar", n90, n30)
	}
	for _, want := range []string{
		`<span class="period__dates days-30">`,
		`<span class="period__summary days-30"><b>`,
		`<div class="days-30"><div class="axis"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestLastDays(t *testing.T) {
	days := daysEnding("2026-10-20", 90)
	got := lastDays(days, 30)
	if len(got) != 30 || got[0].Date != "2026-09-21" || got[29].Date != "2026-10-20" {
		t.Errorf("last 30 = %d days, %s to %s", len(got), got[0].Date, got[len(got)-1].Date)
	}
	if short := lastDays(days[:10], 30); len(short) != 10 {
		t.Errorf("fewer than 30 days should all be kept, got %d", len(short))
	}
}

// History and the current status are kept apart: the cells are history,
// today's included ("so far", filled as far as the day has gone), and each
// row with a bar has its current status in the Now column after it.
func TestNowColumnSeparateFromHistory(t *testing.T) {
	body := get(t, testServer(t), "/").Body.String()
	bars := strings.Count(body, `<div class="bar`)
	nows := strings.Count(body, `<div class="row__now`)
	// Every bar has a Now cell; the header and the axis rows add two more.
	if bars < 20 || nows != bars+2 {
		t.Errorf("%d bars, %d Now cells; want one each and two for the header", bars, nows)
	}
	// The mock's Auth is degraded now, Website is fine.
	if !strings.Contains(body, `<i class="now__mark now__mark--operational"></i>OK</span>`) ||
		!strings.Contains(body, `<div class="row__now row__now--bad"><span class="now__state"><i class="now__mark now__mark--degraded"></i>Degraded</span>`) {
		t.Error("Now cells do not show current statuses")
	}
	if !regexp.MustCompile(`[0-9]+/[0-9]+ OK</span>`).MatchString(body) {
		t.Error("the system row should count the components that are OK")
	}
	if !strings.Contains(body, `data-tip="Today · `) || !strings.Contains(body, ` so far"`) {
		t.Error("today's cell should read as history so far")
	}
	if !regexp.MustCompile(`bar__day--today" style="--fill: [0-9]+%"`).MatchString(body) {
		t.Error("today's cell should be filled as far as the day has gone")
	}
	if strings.Contains(body, `class="now now--`) || strings.Contains(body, "track") {
		t.Error("the old Now marker is still on the page")
	}
}

func TestDayGone(t *testing.T) {
	for _, c := range []struct {
		at   string
		want int
	}{
		{"2026-10-20T00:00:00Z", 0},
		{"2026-10-20T12:00:00Z", 50},
		{"2026-10-20T14:32:00Z", 60},
		{"2026-10-20T23:59:59Z", 99},
		{"2026-10-20T13:30:00+01:00", 52}, // 12:30 UTC
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		if got := dayGone(at); got != c.want {
			t.Errorf("dayGone(%s) = %d, want %d", c.at, got, c.want)
		}
	}
}
