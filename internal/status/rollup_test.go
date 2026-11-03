package status

import (
	"math"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
)

func TestRollups(t *testing.T) {
	shop := model.Page{Slug: "shop"}
	c := comp("C", "Data", model.StatusOperational)
	c.Pages = []string{"shop"}
	got := map[string]model.Status{}
	for _, r := range Rollups([]model.Component{
		comp("A", "Edge", model.StatusOutage),
		comp("B", "Edge", model.StatusMaintenance),
		c,
	}, []model.Page{shop, {Slug: "empty"}}) {
		got[r.Key()] = r.Status
	}
	want := map[string]model.Status{
		SystemKey(model.Page{}):        model.StatusOutage,
		GroupKey(model.Page{}, "Edge"): model.StatusOutage, // outage outranks maintenance
		GroupKey(model.Page{}, "Data"): model.StatusOperational,
		SystemKey(shop):                model.StatusOperational,
		GroupKey(shop, "Data"):         model.StatusOperational,
	}
	if len(got) != len(want) {
		t.Fatalf("rollups = %v, want %v (no rows for a page with nothing on it)", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
}

// Two components down an hour each, at different times: the combined rows
// were down two hours, not one.
func TestCombinedRowsCountSeparateOutages(t *testing.T) {
	store := history.New()
	now := time.Now().UTC()
	base := now.AddDate(0, 0, -1).Truncate(24 * time.Hour).Add(9 * time.Hour)
	for i := 0; i <= 180; i++ {
		a, b := comp("A", "Edge", model.StatusOperational), comp("B", "Edge", model.StatusOperational)
		if i >= 1 && i <= 60 {
			a.Status = model.StatusOutage
		}
		if i >= 61 && i <= 120 {
			b.Status = model.StatusDegraded
		}
		comps := []model.Component{a, b}
		store.Record(base.Add(time.Duration(i)*time.Minute), append(Rollups(comps, nil), comps...), 2*time.Minute)
	}
	snap := Build(Input{Title: "t", Now: now, Connected: true, History: store, Components: []model.Component{
		comp("A", "Edge", model.StatusOperational), comp("B", "Edge", model.StatusOperational),
	}})
	yesterday := len(snap.History) - 2
	if d := snap.History[yesterday]; d.Observed != 10800 || d.Downtime != 7200 || d.Status != model.StatusOutage {
		t.Fatalf("system yesterday = %+v, want 10800 s observed, 7200 s down", d)
	}
	if d := snap.Groups[0].History[yesterday]; d.Downtime != 7200 {
		t.Fatalf("group yesterday = %+v, want 7200 s down", d)
	}
	if u := snap.Uptime; math.Abs(u-1.0/3) > 1e-12 {
		t.Fatalf("system uptime = %v, want 1/3", u)
	}
	for _, c := range snap.Groups[0].Components {
		if d := c.History[yesterday]; d.Downtime != 3600 {
			t.Fatalf("%s yesterday = %+v, want 3600 s down", c.Name, d)
		}
	}
}

// Days recorded before the combined rows existed fall back to the longest
// member downtime; a day only partly recorded takes the larger figure.
func TestCombinedRowsFallBackToEstimate(t *testing.T) {
	store := history.New()
	now := time.Now().UTC()
	day := func(n int) string { return now.AddDate(0, 0, -n).Format("2006-01-02") }
	store.Seed("A", []model.Day{
		{Date: day(2), Status: model.StatusOutage, Observed: 86400, Downtime: 3600},
		{Date: day(1), Status: model.StatusOutage, Observed: 86400, Downtime: 3600},
	})
	store.Seed("B", []model.Day{
		{Date: day(2), Status: model.StatusDegraded, Observed: 86400, Downtime: 1800},
		{Date: day(1), Status: model.StatusOperational, Observed: 86400},
	})
	// The combined row was first recorded late on day 1, after A's outage.
	store.Seed(SystemKey(model.Page{}), []model.Day{{Date: day(1), Status: model.StatusOutage, Observed: 7200, Downtime: 600}})
	snap := Build(Input{Title: "t", Now: now, Connected: true, History: store, Components: []model.Component{
		comp("A", "", model.StatusOperational), comp("B", "", model.StatusOperational),
	}})
	h := snap.History
	if d := h[len(h)-3]; d.Downtime != 3600 || d.Observed != 86400 || d.Status != model.StatusOutage {
		t.Fatalf("day 2 = %+v, want the longest member downtime", d)
	}
	if d := h[len(h)-2]; d.Downtime != 3600 || d.Observed != 86400 {
		t.Fatalf("day 1 = %+v, want the larger of estimate and recorded", d)
	}
}
