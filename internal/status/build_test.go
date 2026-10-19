package status

import (
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
)

func comp(name, group string, observed model.Status) model.Component {
	return model.Component{Name: name, Group: group, Observed: observed, Status: observed, Desired: 2, Ready: 2}
}

func TestBuildOverallIsWorstComponent(t *testing.T) {
	now := time.Now()
	snap := Build(Input{
		Title: "t", Now: now, Connected: true,
		Components: []model.Component{
			comp("A", "Edge", model.StatusOperational),
			comp("B", "Edge", model.StatusDegraded),
			comp("C", "Data", model.StatusOperational),
		},
	})
	if snap.Status != model.StatusDegraded {
		t.Fatalf("status = %v, want degraded", snap.Status)
	}
	if snap.Headline != "Some systems are degraded." {
		t.Fatalf("headline = %q", snap.Headline)
	}
	if len(snap.Groups) != 2 || snap.Groups[0].Name != "Edge" || snap.Groups[0].Status != model.StatusDegraded {
		t.Fatalf("groups = %+v", snap.Groups)
	}
	if snap.Groups[1].Status != model.StatusOperational {
		t.Fatalf("Data group status = %v", snap.Groups[1].Status)
	}
}

func TestBuildHeadlines(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		comps []model.Component
		want  string
	}{
		{"all ok", []model.Component{comp("A", "", model.StatusOperational)}, "All systems operational."},
		{"one down", []model.Component{comp("A", "", model.StatusOutage), comp("B", "", model.StatusOperational)}, "One system is down."},
		{"two down", []model.Component{comp("A", "", model.StatusOutage), comp("B", "", model.StatusOutage), comp("C", "", model.StatusOperational)}, "2 systems are down."},
		{"all down", []model.Component{comp("A", "", model.StatusOutage)}, "All systems are down."},
		{"none", nil, "No components yet."},
	}
	for _, c := range cases {
		got := Build(Input{Now: now, Connected: true, Components: c.comps}).Headline
		if got != c.want {
			t.Errorf("%s: headline = %q, want %q", c.name, got, c.want)
		}
	}
	if got := Build(Input{Now: now, Connected: false}).Headline; got != "Waiting for the cluster." {
		t.Errorf("disconnected headline = %q", got)
	}
}

func TestIncidentRaisesComponentAndOverall(t *testing.T) {
	now := time.Now()
	inc := model.Incident{Title: "x", Impact: model.StatusOutage, Components: []string{"A"}, StartedAt: now.Add(-time.Hour)}
	snap := Build(Input{
		Now: now, Connected: true,
		Components: []model.Component{comp("A", "", model.StatusOperational), comp("B", "", model.StatusOperational)},
		Incidents:  []model.Incident{inc},
	})
	if snap.Status != model.StatusOutage {
		t.Fatalf("overall = %v, want outage", snap.Status)
	}
	a := snap.Groups[0].Components[0]
	if a.Status != model.StatusOutage || a.Observed != model.StatusOperational {
		t.Fatalf("A = status %v observed %v", a.Status, a.Observed)
	}
	if b := snap.Groups[0].Components[1]; b.Status != model.StatusOperational {
		t.Fatalf("B should be untouched, got %v", b.Status)
	}
	if len(snap.ActiveIncidents) != 1 {
		t.Fatalf("active incidents = %d", len(snap.ActiveIncidents))
	}
}

func TestMaintenanceWindowOverridesOutage(t *testing.T) {
	now := time.Now()
	from, until := now.Add(-time.Hour), now.Add(time.Hour)
	inc := model.Incident{Title: "m", Impact: model.StatusMaintenance, Components: []string{"A"}, ScheduledFor: &from, ScheduledUntil: &until, StartedAt: from}
	snap := Build(Input{
		Now: now, Connected: true,
		Components: []model.Component{comp("A", "", model.StatusOutage)},
		Incidents:  []model.Incident{inc},
	})
	if got := snap.Groups[0].Components[0].Status; got != model.StatusMaintenance {
		t.Fatalf("status = %v, want maintenance", got)
	}
	if snap.Status != model.StatusMaintenance || snap.Headline != "Maintenance in progress." {
		t.Fatalf("overall = %v %q", snap.Status, snap.Headline)
	}
}

func TestPagesFilterComponentsAndIncidents(t *testing.T) {
	now := time.Now()
	pages := []model.Page{
		{Slug: "a", Title: "A", Namespaces: []string{"ns-a"}},
		{Slug: "b", Title: "B", Namespaces: []string{"ns-b"}},
	}
	ca := comp("API", "", model.StatusOperational)
	ca.Namespace, ca.Pages = "ns-a", []string{"a"}
	cb := comp("API", "", model.StatusOperational)
	cb.Namespace, cb.Pages = "ns-b", []string{"b"}
	cb.ID = "deployment/ns-b/api"
	ca.ID = "deployment/ns-a/api"
	// Team A's incident on "API" must not touch team B's "API".
	inc := model.Incident{Title: "a only", Impact: model.StatusOutage, Components: []string{"API"}, Namespace: "ns-a", StartedAt: now.Add(-time.Hour)}
	global := model.Incident{Title: "everywhere", Impact: model.StatusDegraded, Namespace: "ops", Pages: []string{"*"}, StartedAt: now.Add(-time.Hour)}

	in := Input{Now: now, Connected: true, Components: []model.Component{ca, cb}, Incidents: []model.Incident{inc, global}, AllPages: pages}

	in.Page = pages[0]
	a := Build(in)
	if a.ComponentCount != 1 || a.Groups[0].Components[0].Status != model.StatusOutage || len(a.ActiveIncidents) != 2 {
		t.Fatalf("page a = %d comps, status %v, %d incidents", a.ComponentCount, a.Groups[0].Components[0].Status, len(a.ActiveIncidents))
	}
	in.Page = pages[1]
	b := Build(in)
	if b.ComponentCount != 1 || b.Groups[0].Components[0].Status != model.StatusDegraded || len(b.ActiveIncidents) != 1 || b.ActiveIncidents[0].Title != "everywhere" {
		t.Fatalf("page b = %d comps, status %v, incidents %+v", b.ComponentCount, b.Groups[0].Components[0].Status, b.ActiveIncidents)
	}
	in.Page = model.Page{}
	d := Build(in)
	if d.ComponentCount != 2 || len(d.ActiveIncidents) != 2 {
		t.Fatalf("default page = %d comps, %d incidents", d.ComponentCount, len(d.ActiveIncidents))
	}
	// On the default page both "API"s are present; only ns-a's is in outage.
	var outages int
	for _, c := range d.Groups[0].Components {
		if c.Status == model.StatusOutage {
			outages++
		}
	}
	if outages != 1 {
		t.Fatalf("default page outages = %d, want 1", outages)
	}
}

func TestProblemsAndAttention(t *testing.T) {
	now := time.Now()
	ok := comp("Fine", "A", model.StatusOperational)
	bad := comp("Broken", "A", model.StatusDegraded)
	bad.Reason = "1 of 2 replicas ready"
	podsOnly := comp("Down", "B", model.StatusOutage)
	podsOnly.Signals = []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOutage, Detail: "0 of 3 replicas ready"}}
	snap := Build(Input{Now: now, Connected: true, Components: []model.Component{ok, bad, podsOnly}})
	if len(snap.Problems) != 2 || snap.Problems[0].Name != "Broken" || snap.Problems[0].Reason != "1 of 2 replicas ready" || snap.Problems[0].Anchor != "c-broken" {
		t.Fatalf("problems = %+v", snap.Problems)
	}
	if snap.Problems[1].Reason != "0 of 3 replicas ready" {
		t.Fatalf("pods-only problem should fall back to the pods detail: %+v", snap.Problems[1])
	}
	var fine, broken model.Component
	for _, g := range snap.Groups {
		for _, c := range g.Components {
			switch c.Name {
			case "Fine":
				fine = c
			case "Broken":
				broken = c
			}
		}
	}
	if fine.Attention || !broken.Attention {
		t.Fatalf("attention: fine=%v broken=%v", fine.Attention, broken.Attention)
	}

	// Recent downtime on a healthy component's endpoint keeps it expanded.
	store := historyWith("Fine::Health", now.AddDate(0, 0, -3), 600)
	snap = Build(Input{Now: now, Connected: true, Components: []model.Component{withSignal(ok, "Health")}, History: store})
	if c := snap.Groups[0].Components[0]; !c.Attention {
		t.Fatal("downtime three days ago should flag attention")
	}
	store = historyWith("Fine::Health", now.AddDate(0, 0, -20), 600)
	snap = Build(Input{Now: now, Connected: true, Components: []model.Component{withSignal(ok, "Health")}, History: store})
	if c := snap.Groups[0].Components[0]; c.Attention {
		t.Fatal("downtime twenty days ago should not flag attention")
	}
	if len(snap.Problems) != 0 {
		t.Fatalf("no problems expected, got %+v", snap.Problems)
	}

	// The same recent downtime on a check set to info does not.
	info := withSignal(ok, "Health")
	info.Checks = model.Checks{Modes: map[string]model.Mode{model.SignalHTTP: model.ModeInfo}}
	store = historyWith("Fine::Health", now.AddDate(0, 0, -3), 600)
	snap = Build(Input{Now: now, Connected: true, Components: []model.Component{info}, History: store})
	if c := snap.Groups[0].Components[0]; c.Attention {
		t.Fatal("downtime on an info check should not flag attention")
	}
}

func withSignal(c model.Component, name string) model.Component {
	c.Signals = []model.Signal{
		{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational, Detail: "2 of 2 ready"},
		{Kind: model.SignalHTTP, Name: name, Status: model.StatusOperational, Detail: "200 · 80 ms"},
	}
	return c
}

func historyWith(key string, when time.Time, downtime float64) *history.Store {
	s := history.New()
	s.Seed(key, []model.Day{{Date: when.UTC().Format("2006-01-02"), Status: model.StatusDegraded, Observed: 86400, Downtime: downtime}})
	return s
}

func TestGroupCollapse(t *testing.T) {
	now := time.Now()
	var small []model.Component
	for _, n := range []string{"a", "b", "c"} {
		small = append(small, comp(n, "Healthy", model.StatusOperational))
	}
	snap := Build(Input{Now: now, Connected: true, Components: small})
	if snap.Groups[0].Collapsed {
		t.Fatal("a small page must not collapse groups")
	}

	var big []model.Component
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		big = append(big, comp(n, "Healthy", model.StatusOperational))
	}
	big = append(big, comp("f", "Troubled", model.StatusOperational), comp("g", "Troubled", model.StatusDegraded))
	store := historyWith("f", now.AddDate(0, 0, -1), 300)
	snap = Build(Input{Now: now, Connected: true, Components: big, History: store})
	byName := map[string]model.Group{}
	for _, g := range snap.Groups {
		byName[g.Name] = g
	}
	if !byName["Healthy"].Collapsed || byName["Healthy"].Attention {
		t.Fatalf("healthy group on a big page should collapse: %+v", byName["Healthy"])
	}
	if byName["Troubled"].Collapsed || !byName["Troubled"].Attention {
		t.Fatalf("group with a degraded member must stay open: %+v", byName["Troubled"])
	}
	if h := byName["Troubled"].History; len(h) != HistoryDays || h[len(h)-2].Downtime != 300 {
		t.Fatalf("group history should combine its components: %+v", h[len(h)-2])
	}
	if byName["Healthy"].Anchor() != "g-healthy" {
		t.Fatalf("anchor = %q", byName["Healthy"].Anchor())
	}
}

func TestIncidentBuckets(t *testing.T) {
	now := time.Now()
	future := now.Add(48 * time.Hour)
	resolvedAt := now.Add(-2 * 24 * time.Hour)
	oldResolved := now.Add(-60 * 24 * time.Hour)
	incs := []model.Incident{
		{Title: "future", Impact: model.StatusMaintenance, ScheduledFor: &future, StartedAt: future},
		{Title: "recent", Impact: model.StatusDegraded, StartedAt: resolvedAt.Add(-time.Hour), ResolvedAt: &resolvedAt},
		{Title: "old", Impact: model.StatusDegraded, StartedAt: oldResolved.Add(-time.Hour), ResolvedAt: &oldResolved},
		{Title: "open", Impact: model.StatusDegraded, StartedAt: now.Add(-time.Minute)},
	}
	snap := Build(Input{Now: now, Connected: true, Components: []model.Component{comp("A", "", model.StatusOperational)}, Incidents: incs})
	if len(snap.Scheduled) != 1 || snap.Scheduled[0].Title != "future" {
		t.Errorf("scheduled = %+v", snap.Scheduled)
	}
	if len(snap.ActiveIncidents) != 1 || snap.ActiveIncidents[0].Title != "open" {
		t.Errorf("active = %+v", snap.ActiveIncidents)
	}
	if len(snap.PastIncidents) != 1 || snap.PastIncidents[0].Title != "recent" {
		t.Errorf("past = %+v", snap.PastIncidents)
	}
	// A future maintenance window must not change today's status.
	if snap.Status != model.StatusDegraded {
		t.Errorf("overall = %v", snap.Status)
	}
}
