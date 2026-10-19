// Package mock provides fake components, incidents and history so the page
// can be developed and demoed without a cluster (run with --mock).
package mock

import (
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/DanielHouston/sutats/internal/checks"
	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/incidents"
	"github.com/DanielHouston/sutats/internal/model"
)

// Source produces a slowly changing set of mock components.
type Source struct {
	mu    sync.Mutex
	rng   *rand.Rand
	comps []model.Component
}

// New builds a mock source with a fixed scenario.
func New() *Source {
	s := &Source{rng: rand.New(rand.NewSource(42))}
	s.comps = []model.Component{
		comp("Website", "Edge", "Marketing site and docs", "Deployment", "web", "website", 3, 3, 10),
		comp("API", "Edge", "Public REST and GraphQL API", "Deployment", "api", "api", 6, 6, 20),
		comp("Ingress", "Edge", "Cluster ingress controllers", "DaemonSet", "ingress-nginx", "ingress-nginx-controller", 5, 5, 30),
		comp("Auth", "Platform", "Login, sessions and tokens", "Deployment", "auth", "auth", 2, 1, 40),
		comp("Background jobs", "Platform", "Queue workers and schedulers", "Deployment", "jobs", "worker", 24, 24, 50),
		comp("Search", "Platform", "", "StatefulSet", "search", "opensearch", 3, 3, 60),
		comp("PostgreSQL", "Data", "Primary database cluster", "StatefulSet", "db", "postgres", 3, 3, 70),
		comp("Object storage", "Data", "S3-compatible blob store", "StatefulSet", "storage", "minio", 4, 4, 80),
	}
	for i := range s.comps {
		s.comps[i].Checks = checks.Resolve(mockChecks[s.comps[i].Name])
		s.derive(&s.comps[i])
	}
	return s
}

// mockChecks is per-component check configuration, written the way the
// sutats.io/check.<name> annotations are. Queue workers restart between
// jobs, so their stability row is shown but not counted.
var mockChecks = map[string]checks.Layer{
	"Background jobs": mustLayer(map[string]string{"sutats.io/check.stability": "info window=1h"}),
}

// UseChecks applies cluster-wide check defaults (the --check.<name> flags)
// under each mock component's own configuration, as the watcher does.
func (s *Source) UseChecks(defaults checks.Layer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.comps {
		s.comps[i].Checks = checks.Resolve(defaults, mockChecks[s.comps[i].Name])
		s.derive(&s.comps[i])
	}
}

func mustLayer(ann map[string]string) checks.Layer {
	l, err := checks.FromAnnotations(ann)
	if err != nil {
		panic(err)
	}
	return l
}

func comp(name, group, desc, kind, ns, wl string, desired, ready int32, order int) model.Component {
	// Mock pages: "edge" is the Edge group, "platform" is everything else,
	// and PostgreSQL is on both.
	var pages []string
	switch {
	case group == "Edge":
		pages = []string{"edge"}
	case name == "PostgreSQL":
		pages = []string{"platform", "edge"}
	default:
		pages = []string{"platform"}
	}
	return model.Component{
		ID: strings.ToLower(kind) + "/" + ns + "/" + wl, Name: name, Group: group, Description: desc,
		Kind: kind, Namespace: ns, Workload: wl, Desired: desired, Ready: ready, Order: order, Pages: pages,
	}
}

// Pages returns the mock page list. Hosts are left empty for the host
// template to fill in.
func (s *Source) Pages() []model.Page {
	return []model.Page{
		{Slug: "edge", Title: "Edge services", Namespaces: []string{"web", "api", "ingress-nginx", "db"}},
		{Slug: "platform", Title: "Platform", Namespaces: []string{"auth", "jobs", "search", "db", "storage"}},
	}
}

// fakeProbes are the HTTP signals shown in mock mode, keyed by component.
var fakeProbes = map[string][]model.Signal{
	"Website": {{Kind: model.SignalHTTP, Name: "Homepage", Status: model.StatusOperational, Detail: "200 · 212 ms", LatencyMS: 212}},
	"API": {
		{Kind: model.SignalHTTP, Name: "Public API", Status: model.StatusOperational, Detail: "200 · 84 ms", LatencyMS: 84},
		{Kind: model.SignalHTTP, Name: "GraphQL", Status: model.StatusOperational, Detail: "200 · 131 ms", LatencyMS: 131},
	},
	"Auth":   {{Kind: model.SignalHTTP, Name: "Login", Status: model.StatusOperational, Detail: "200 · 340 ms", LatencyMS: 340}},
	"Search": {{Kind: model.SignalHTTP, Name: "Query API", Status: model.StatusDegraded, Detail: "200 · 2.6 s slow", LatencyMS: 2600}},
}

func (s *Source) derive(c *model.Component) {
	var st model.Status
	var reason string
	switch {
	case c.Desired == 0:
		st, reason = model.StatusMaintenance, "Scaled to zero"
	case c.Ready == 0:
		st, reason = model.StatusOutage, "0 of "+itoa(c.Desired)+" replicas ready"
	case c.Ready < c.Desired:
		st, reason = model.StatusDegraded, itoa(c.Ready)+" of "+itoa(c.Desired)+" replicas ready"
	default:
		st, reason = model.StatusOperational, itoa(c.Ready)+" of "+itoa(c.Desired)+" ready"
	}
	stability := model.Signal{Kind: model.SignalStability, Name: "Stability", Status: model.StatusOperational, Detail: "no restarts in 15 min"}
	if c.Name == "Background jobs" {
		stability.Status, stability.Detail = model.StatusDegraded, "3 restarts in 1 h"
	}
	all := append([]model.Signal{
		{Kind: model.SignalPods, Name: "Pods", Status: st, Detail: reason},
		stability,
		{Kind: model.SignalScheduling, Name: "Scheduling", Status: model.StatusOperational, Detail: "all pods scheduled"},
	}, fakeProbes[c.Name]...)
	c.Signals = c.Signals[:0:0]
	for _, sig := range all {
		if c.Checks.Enabled(sig.Kind) {
			c.Signals = append(c.Signals, sig)
		}
	}
	c.Recompute()
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Components returns the current mock state, nudging one replica count now
// and then so the page visibly changes.
func (s *Source) Components() ([]model.Component, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rng.Intn(6) == 0 {
		c := &s.comps[s.rng.Intn(len(s.comps))]
		if c.Ready < c.Desired {
			c.Ready++
		} else if s.rng.Intn(3) == 0 && c.Desired > 1 {
			c.Ready--
		}
		s.derive(c)
	}
	out := make([]model.Component, len(s.comps))
	copy(out, s.comps)
	return out, nil
}

// Synced always reports true.
func (s *Source) Synced() bool { return true }

// Incidents returns a sample incident set relative to now.
func Incidents(now time.Time) incidents.Source {
	day := 24 * time.Hour
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	doc := incidents.Document{Incidents: []incidents.Entry{
		{
			Title: "Elevated login failures", Impact: "degraded", Components: []string{"Auth"},
			Updates: []incidents.Update{
				{At: at(-52 * time.Minute), Status: "investigating", Body: "Some sign-in attempts are returning errors. We are investigating."},
				{At: at(-31 * time.Minute), Status: "identified", Body: "One auth replica is failing its health checks after a node reboot. A replacement is starting."},
			},
		},
		{
			Title: "Database storage upgrade", Impact: "maintenance", Components: []string{"PostgreSQL", "API"},
			ScheduledFor: at(3 * day), ScheduledUntil: at(3*day + 2*time.Hour),
			Updates: []incidents.Update{
				{At: at(-2 * day), Status: "scheduled", Body: "We will move the primary database to faster storage. Expect up to 10 minutes of read-only mode."},
			},
		},
		{
			Title: "API timeouts in eu-west", Impact: "outage", Components: []string{"API"},
			Updates: []incidents.Update{
				{At: at(-6*day - 3*time.Hour), Status: "investigating", Body: "API requests are timing out for a subset of customers."},
				{At: at(-6*day - 2*time.Hour), Status: "identified", Body: "A bad rollout exhausted the connection pool. Rolling back."},
				{At: at(-6*day - 80*time.Minute), Status: "monitoring", Body: "Rollback complete, error rates are back to normal."},
				{At: at(-6 * day), Status: "resolved", Body: "Resolved. A post-incident review will follow."},
			},
		},
		{
			Title: "Search index rebuild", Impact: "maintenance", Components: []string{"Search"},
			ScheduledFor: at(-12 * day), ScheduledUntil: at(-12*day + 90*time.Minute),
			Updates: []incidents.Update{
				{At: at(-14 * day), Status: "scheduled", Body: "Search results may be stale during the rebuild."},
				{At: at(-12*day + 85*time.Minute), Status: "completed", Body: "Rebuild finished ahead of schedule."},
			},
		},
	}}
	list := incidents.FromDocument(doc)
	// Real incidents come from a ConfigMap in a namespace, which decides the
	// pages they show on. Give the mock ones a home too.
	homes := map[string]string{
		"Elevated login failures":  "auth",
		"Database storage upgrade": "db",
		"API timeouts in eu-west":  "api",
		"Search index rebuild":     "search",
	}
	for i := range list {
		list[i].Namespace = homes[list[i].Title]
	}
	return incidents.Static(list)
}

// HistoryDays is how far back the mock scenario reaches.
const HistoryDays = 90

// Series describes one signal's series for seeding: its history key, the
// component it belongs to, its kind and a typical latency for HTTP probes.
type Series struct {
	Key       string
	Component string
	Kind      string
	BaseMS    int
}

// Window is a stretch of time during which a signal was in a non-operational
// state. Outside every window a signal is operational.
type Window struct {
	Key        string
	Start, End time.Time
	Status     model.Status
}

// Scenario is the scripted 90-day story behind the mock: a few memorable
// incidents, some routine blips, and the state the live mock shows today.
// Both the file-mode seed and the database seed are derived from it so the
// two modes tell the same story.
func Scenario(comps []model.Component, now time.Time) ([]Series, []Window) {
	now = now.UTC()
	day := func(daysAgo int, hour, min int) time.Time {
		d := now.AddDate(0, 0, -daysAgo).Truncate(24 * time.Hour)
		return d.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
	}
	base := map[string]int{"Homepage": 210, "Public API": 85, "GraphQL": 130, "Login": 340, "Query API": 450}

	var series []Series
	keyOf := map[string]string{} // "Component/Signal" -> history key
	for _, c := range comps {
		for _, s := range c.Signals {
			if !s.HasBar() {
				continue
			}
			k := history.SignalKey(c.Key(), s.Name)
			keyOf[c.Name+"/"+s.Name] = k
			series = append(series, Series{Key: k, Component: c.Key(), Kind: s.Kind, BaseMS: base[s.Name]})
		}
	}

	var wins []Window
	add := func(comp, sig string, start time.Time, d time.Duration, st model.Status) {
		if k, ok := keyOf[comp+"/"+sig]; ok {
			wins = append(wins, Window{Key: k, Start: start, End: start.Add(d), Status: st})
		}
	}
	h, m := time.Hour, time.Minute

	// The headline incidents.
	add("API", "GraphQL", day(6, 3, 10), 3*h+5*m, model.StatusOutage)   // the eu-west timeouts
	add("API", "Pods", day(6, 3, 10), 20*m, model.StatusDegraded)       // bad rollout at the same time
	add("Search", "Pods", day(12, 2, 0), 90*m, model.StatusMaintenance) // index rebuild window
	add("Search", "Query API", day(12, 2, 0), 90*m, model.StatusMaintenance)
	add("Object storage", "Pods", day(27, 14, 40), 22*m, model.StatusOutage)
	add("Ingress", "Pods", day(81, 9, 5), 8*m, model.StatusOutage)

	// Slow periods and partial degradations.
	add("Search", "Query API", day(1, 13, 0), 3*h, model.StatusDegraded)
	add("Search", "Query API", day(3, 8, 30), 90*m, model.StatusDegraded)
	add("Search", "Query API", day(9, 16, 0), 2*h, model.StatusDegraded)
	add("Auth", "Login", day(9, 11, 20), 2*h, model.StatusDegraded)
	add("API", "Public API", day(14, 7, 45), 25*m, model.StatusDegraded)
	add("API", "GraphQL", day(30, 22, 0), 40*m, model.StatusDegraded)
	add("API", "GraphQL", day(61, 4, 15), 1*h, model.StatusDegraded)
	add("API", "Pods", day(45, 10, 0), 15*m, model.StatusDegraded)
	add("Website", "Homepage", day(22, 18, 10), 50*m, model.StatusDegraded)
	add("Website", "Homepage", day(70, 1, 0), 30*m, model.StatusDegraded)
	add("Website", "Pods", day(40, 12, 30), 12*m, model.StatusDegraded)
	add("Ingress", "Pods", day(33, 15, 0), 20*m, model.StatusDegraded)
	add("PostgreSQL", "Pods", day(58, 23, 10), 35*m, model.StatusDegraded)
	for _, d := range []int{5, 17, 29, 52, 77} {
		add("Background jobs", "Pods", day(d, 6+d%5, 0), time.Duration(10+d%20)*m, model.StatusDegraded)
	}

	// What the live mock shows right now, so today's cells agree with it.
	add("Auth", "Pods", now.Add(-52*m), 52*m+h, model.StatusDegraded)
	add("Search", "Query API", now.Add(-2*h), 3*h, model.StatusDegraded)

	// Routine blips: a few minutes of slowness every couple of weeks.
	rng := rand.New(rand.NewSource(11))
	for _, s := range series {
		if s.Kind != model.SignalHTTP {
			continue
		}
		for i := 0; i < 3; i++ {
			start := day(rng.Intn(HistoryDays-1)+1, rng.Intn(24), rng.Intn(60))
			wins = append(wins, Window{Key: s.Key, Start: start, End: start.Add(time.Duration(4+rng.Intn(8)) * m), Status: model.StatusDegraded})
		}
	}
	return series, wins
}

// RandomScenario invents a plausible history for any set of components, for
// backfilling a demo or trial install with the bars already filled in.
// Endpoints get a few slow spells and maybe an outage; pod checks get a
// couple of short degradations. Deterministic for a given component set.
func RandomScenario(comps []model.Component, now time.Time, days int) ([]Series, []Window) {
	now = now.UTC()
	rng := rand.New(rand.NewSource(97))
	var series []Series
	var wins []Window
	at := func(daysAgo, hour, min int) time.Time {
		return now.AddDate(0, 0, -daysAgo).Truncate(24 * time.Hour).Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
	}
	for _, c := range comps {
		for _, s := range c.Signals {
			if !s.HasBar() {
				continue
			}
			key := history.SignalKey(c.Key(), s.Name)
			base := 0
			if s.Kind == model.SignalHTTP {
				base = 40 + rng.Intn(360)
			}
			series = append(series, Series{Key: key, Component: c.Key(), Kind: s.Kind, BaseMS: base})

			slow, outages := 2+rng.Intn(3), 0
			if s.Kind == model.SignalHTTP && rng.Intn(2) == 0 {
				outages = 1
			}
			if s.Kind == model.SignalPods {
				slow, outages = 1+rng.Intn(2), rng.Intn(4)/3 // one in four gets a short outage
			}
			for i := 0; i < slow; i++ {
				start := at(2+rng.Intn(days-3), rng.Intn(24), rng.Intn(60))
				wins = append(wins, Window{Key: key, Start: start, End: start.Add(time.Duration(5+rng.Intn(85)) * time.Minute), Status: model.StatusDegraded})
			}
			for i := 0; i < outages; i++ {
				start := at(5+rng.Intn(days-8), rng.Intn(24), rng.Intn(60))
				wins = append(wins, Window{Key: key, Start: start, End: start.Add(time.Duration(10+rng.Intn(50)) * time.Minute), Status: model.StatusOutage})
			}
		}
	}
	return series, wins
}

// SeedHistory fills the in-memory store from the scenario for file mode:
// per-day buckets for every signal, and the worst-of-signals for each
// component. Database mode seeds samples instead; see db.SeedSamples.
func SeedHistory(store *history.Store, comps []model.Component, now time.Time) {
	series, wins := Scenario(comps, now)
	now = now.UTC()
	byKey := map[string][]Window{}
	for _, w := range wins {
		byKey[w.Key] = append(byKey[w.Key], w)
	}
	dayOf := func(i int) (time.Time, time.Time, string) {
		start := now.AddDate(0, 0, i-HistoryDays+1).Truncate(24 * time.Hour)
		return start, start.Add(24 * time.Hour), start.Format("2006-01-02")
	}

	perComponent := map[string][]model.Day{}
	for _, s := range series {
		days := make([]model.Day, HistoryDays)
		for i := range days {
			start, end, date := dayOf(i)
			if end.After(now) {
				end = now
			}
			d := model.Day{Date: date, Status: model.StatusOperational, Observed: end.Sub(start).Seconds()}
			for _, w := range byKey[s.Key] {
				a, b := w.Start, w.End
				if a.Before(start) {
					a = start
				}
				if b.After(end) {
					b = end
				}
				if !b.After(a) {
					continue
				}
				d.Status = model.Worst(d.Status, w.Status)
				if w.Status >= model.StatusDegraded {
					d.Downtime += b.Sub(a).Seconds()
				}
			}
			days[i] = d
		}
		store.Seed(s.Key, days)
		comp := perComponent[s.Component]
		if comp == nil {
			comp = make([]model.Day, HistoryDays)
			copy(comp, days)
		} else {
			for i := range comp {
				comp[i].Status = model.Worst(comp[i].Status, days[i].Status)
				if days[i].Downtime > comp[i].Downtime {
					comp[i].Downtime = days[i].Downtime
				}
			}
		}
		perComponent[s.Component] = comp
	}
	for key, days := range perComponent {
		store.Seed(key, days)
	}
}
