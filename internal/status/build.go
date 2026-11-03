// Package status combines live component state, operator incidents and
// recorded history into the Snapshot the page renders.
package status

import (
	"fmt"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
)

// HistoryDays is how many days the uptime bar covers.
const HistoryDays = 90

// PastIncidentWindow is how far back the "Past incidents" list goes.
const PastIncidentWindow = 30 * 24 * time.Hour

// AttentionDays is how far back downtime keeps a component expanded.
const AttentionDays = 7

// CollapseGroupsAbove is the component count beyond which healthy groups
// are shown collapsed to a single row. Small pages gain nothing from hiding
// names, so they never collapse.
const CollapseGroupsAbove = 6

// recentDowntime reports whether any of the last n days has downtime.
func recentDowntime(days []model.Day, n int) bool {
	for i := len(days) - 1; i >= 0 && i >= len(days)-n; i-- {
		if days[i].Downtime > 0 {
			return true
		}
	}
	return false
}

// Input is everything Build needs.
type Input struct {
	Title      string
	Now        time.Time
	Components []model.Component
	Incidents  []model.Incident
	History    *history.Store
	Connected  bool

	// Page selects which components and incidents to include. The zero Page
	// is the default page with everything on it. AllPages is the full page
	// list, used to decide which incidents reach which components and to
	// build the navigation links. Links are the navigation entries.
	Page     model.Page
	AllPages []model.Page
	Links    []model.PageLink

	// ShowInternals keeps namespaces, workload names, Service, Ingress and
	// Route names, configuration mistakes and check settings in the
	// snapshot. Off by default, since status pages are usually public; see
	// public.go.
	ShowInternals bool
}

// reaches reports whether an incident may affect a component. An incident
// reaches a component in its own namespace, or on any page it is shown on.
// This stops team A's "API" incident from marking team B's "API" too.
func reaches(inc model.Incident, c model.Component, pages []model.Page) bool {
	if inc.Namespace == "" || inc.Namespace == c.Namespace {
		return true
	}
	for _, p := range pages {
		if inc.OnPage(p) && c.OnPage(p) {
			return true
		}
	}
	return false
}

// ApplyIncidents layers active incidents on top of observed status. It is
// separate from Build so the recorder can store the effective status too.
func ApplyIncidents(comps []model.Component, incs []model.Incident, pages []model.Page, now time.Time) {
	for i := range comps {
		c := &comps[i]
		c.Status = c.Observed
		inMaintenance := false
		for _, inc := range incs {
			if !inc.ActiveAt(now) || !inc.Affects(c.Name) || !reaches(inc, *c, pages) {
				continue
			}
			if inc.IsMaintenance() || inc.Impact == model.StatusMaintenance {
				inMaintenance = true
				continue
			}
			c.Status = model.Worst(c.Status, inc.Impact)
		}
		// Planned maintenance is not an outage: it overrides whatever the
		// cluster reports while the window is open.
		if inMaintenance {
			c.Status = model.StatusMaintenance
			if c.Observed != model.StatusOperational && c.Reason != "" {
				c.Reason = "Scheduled maintenance · " + c.Reason
			} else {
				c.Reason = "Scheduled maintenance"
			}
		}
	}
}

// Build computes a Snapshot.
func Build(in Input) model.Snapshot {
	now := in.Now
	snap := model.Snapshot{
		Title:            in.Title,
		GeneratedAt:      now,
		ClusterConnected: in.Connected,
		Page:             in.Page,
		Pages:            in.Links,
		// Empty lists, not null, so API clients can iterate without checks.
		Groups:          []model.Group{},
		ActiveIncidents: []model.Incident{},
		Scheduled:       []model.Incident{},
		PastIncidents:   []model.Incident{},
	}

	var comps []model.Component
	for _, c := range in.Components {
		if c.OnPage(in.Page) {
			comps = append(comps, c)
		}
	}
	snap.ComponentCount = len(comps)
	var incidents []model.Incident
	for _, inc := range in.Incidents {
		if inc.OnPage(in.Page) {
			incidents = append(incidents, inc)
		}
	}
	// History is read under the real keys before anything is renamed.
	var ids map[string]string
	if !in.ShowInternals {
		ids = publicIDs(in.Components)
	}
	snap.Keys = map[string]string{}
	for i := range comps {
		c := &comps[i]
		// Each page gets its own copy of the signals: Build fills in their
		// history and may rename them.
		c.Signals = append([]model.Signal(nil), c.Signals...)
		attachHistory(c, in.History, now)
		if in.ShowInternals {
			showKeys(*c, snap.Keys)
			continue
		}
		realKey := c.Key()
		c.ID = ids[realKey]
		snap.Keys[c.ID] = realKey
		if hideSignals(c, realKey, c.ID, snap.Keys) {
			c.Recompute() // the reason may quote a renamed signal
		}
	}

	// Incidents still need the namespace, to decide what they reach.
	ApplyIncidents(comps, incidents, in.AllPages, now)

	overall := model.StatusUnknown
	if len(comps) > 0 {
		overall = model.StatusOperational
	}
	var outages int
	for i := range comps {
		c := &comps[i]
		overall = model.Worst(overall, c.Status)
		if c.Status == model.StatusOutage {
			outages++
		}

		c.Attention = c.Status != model.StatusOperational || recentDowntime(c.History, AttentionDays)
		for _, sig := range c.Signals {
			// An info check is shown but does not matter by definition, so
			// its downtime does not hold the rows open.
			if c.Checks.Mode(sig.Kind) != model.ModeInfo && recentDowntime(sig.History, AttentionDays) {
				c.Attention = true
			}
		}
		if c.Status != model.StatusOperational && c.Status != model.StatusUnknown {
			reason := c.Reason
			if reason == "" {
				reason = c.PodsDetail()
			}
			snap.Problems = append(snap.Problems, model.Problem{
				Anchor: c.Anchor(), Name: c.Name, Group: c.Group, Status: c.Status, Reason: reason,
			})
		}
	}
	if snap.Problems == nil {
		snap.Problems = []model.Problem{}
	}
	// Attention needed the check modes; now the internals can go.
	if !in.ShowInternals {
		for i := range comps {
			c := &comps[i]
			c.Kind, c.Namespace, c.Workload = "", "", ""
			c.Checks = model.Checks{}
		}
	}

	// Group, preserving the component order.
	index := map[string]int{}
	for _, c := range comps {
		gi, ok := index[c.Group]
		if !ok {
			gi = len(snap.Groups)
			index[c.Group] = gi
			snap.Groups = append(snap.Groups, model.Group{Name: c.Group, Status: model.StatusOperational})
		}
		g := &snap.Groups[gi]
		g.Components = append(g.Components, c)
		g.Status = model.Worst(g.Status, c.Status)
		g.Attention = g.Attention || c.Attention
	}
	for gi := range snap.Groups {
		g := &snap.Groups[gi]
		g.History, g.Uptime = combinedHistory(g.Components, in.History, GroupKey(in.Page, g.Name), now)
		g.Collapsed = g.Name != "" && !g.Attention && g.Status == model.StatusOperational && len(comps) > CollapseGroupsAbove
	}

	for _, inc := range incidents {
		switch {
		case inc.IsMaintenance() && !inc.Resolved() && now.Before(*inc.ScheduledFor):
			snap.Scheduled = append(snap.Scheduled, inc)
		case inc.ActiveAt(now):
			snap.ActiveIncidents = append(snap.ActiveIncidents, inc)
			if !inc.IsMaintenance() {
				overall = model.Worst(overall, inc.Impact)
			} else if overall < model.StatusMaintenance {
				overall = model.StatusMaintenance
			}
		default:
			ended := inc.ResolvedAt
			if ended == nil && inc.ScheduledUntil != nil {
				ended = inc.ScheduledUntil
			}
			if ended != nil && now.Sub(*ended) <= PastIncidentWindow {
				snap.PastIncidents = append(snap.PastIncidents, inc)
			}
		}
	}
	// Scheduled list reads better soonest-first.
	for i, j := 0, len(snap.Scheduled)-1; i < j; i, j = i+1, j-1 {
		snap.Scheduled[i], snap.Scheduled[j] = snap.Scheduled[j], snap.Scheduled[i]
	}

	if !in.ShowInternals {
		hideIncidents(snap.ActiveIncidents, snap.Scheduled, snap.PastIncidents)
		snap.Page.Namespaces = nil
	}

	snap.Status = overall
	snap.Headline = headline(overall, outages, len(comps), in.Connected)
	snap.History, snap.Uptime = combinedHistory(comps, in.History, SystemKey(in.Page), now)
	return snap
}

// attachHistory fills in the 90-day days and uptime of a component and each
// of its signals. Uptime -1 means no history is kept.
func attachHistory(c *model.Component, store *history.Store, now time.Time) {
	if store == nil {
		c.Uptime = -1
		for j := range c.Signals {
			c.Signals[j].Uptime = -1
		}
		return
	}
	c.History = store.Days(c.Key(), HistoryDays, now)
	c.Uptime = history.Uptime(c.History)
	for j := range c.Signals {
		sig := &c.Signals[j]
		if !sig.HasBar() {
			sig.Uptime = -1
			continue
		}
		sig.History = store.Days(history.SignalKey(c.Key(), sig.Name), HistoryDays, now)
		sig.Uptime = history.Uptime(sig.History)
	}
}

// combinedHistory folds a set of components' days into one row. Where the
// sampler recorded the row itself under key (see Rollups), its days are the
// real time anything in it was wrong. Days from before that have only the
// members' totals, and the longest of their downtimes is the closest
// estimate, since theirs may or may not have overlapped. Each day takes the
// larger of the two: both fall short of the truth if anything, and the
// recorded one is exact once it covers the whole day.
func combinedHistory(comps []model.Component, store *history.Store, key string, now time.Time) ([]model.Day, float64) {
	days := make([]model.Day, HistoryDays)
	for i := range days {
		days[i].Date = now.UTC().AddDate(0, 0, i-HistoryDays+1).Format("2006-01-02")
	}
	for _, c := range comps {
		for i, d := range c.History {
			if i >= len(days) {
				break
			}
			days[i].Status = model.Worst(days[i].Status, d.Status)
			if d.Observed > days[i].Observed {
				days[i].Observed = d.Observed
			}
			if d.Downtime > days[i].Downtime {
				days[i].Downtime = d.Downtime
			}
		}
	}
	if store != nil {
		for i, d := range store.Days(key, HistoryDays, now) {
			days[i].Status = model.Worst(days[i].Status, d.Status)
			days[i].Observed = max(days[i].Observed, d.Observed)
			days[i].Downtime = max(days[i].Downtime, d.Downtime)
		}
	}
	return days, history.Uptime(days)
}

func headline(s model.Status, outages, total int, connected bool) string {
	switch s {
	case model.StatusOperational:
		return "All systems operational."
	case model.StatusMaintenance:
		return "Maintenance in progress."
	case model.StatusDegraded:
		return "Some systems are degraded."
	case model.StatusOutage:
		if total > 0 && outages == total {
			return "All systems are down."
		}
		if outages == 1 {
			return "One system is down."
		}
		return fmt.Sprintf("%d systems are down.", outages)
	default:
		if !connected {
			return "Waiting for the cluster."
		}
		return "No components yet."
	}
}
