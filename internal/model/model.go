// Package model holds the data types shared by the watcher, the incident
// store, the history store and the web layer.
package model

import (
	"sort"
	"strings"
	"time"
)

// Status is the health of a component or of the whole page.
// The order matters: a higher value is worse, and the overall status is the
// worst status of any component.
type Status int

const (
	StatusUnknown Status = iota
	StatusOperational
	StatusMaintenance
	StatusDegraded
	StatusOutage
)

func (s Status) String() string {
	switch s {
	case StatusOperational:
		return "operational"
	case StatusMaintenance:
		return "maintenance"
	case StatusDegraded:
		return "degraded"
	case StatusOutage:
		return "outage"
	default:
		return "unknown"
	}
}

// Label is the human wording used in the UI.
func (s Status) Label() string {
	switch s {
	case StatusOperational:
		return "Operational"
	case StatusMaintenance:
		return "Under maintenance"
	case StatusDegraded:
		return "Degraded"
	case StatusOutage:
		return "Outage"
	default:
		return "Unknown"
	}
}

// MarshalText lets Status serialise as its lowercase name in JSON.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads the name written by MarshalText (used by the history
// file). Unrecognised names become StatusUnknown rather than an error.
func (s *Status) UnmarshalText(b []byte) error {
	*s = ParseStatus(string(b))
	if string(b) == "unknown" {
		*s = StatusUnknown
	}
	return nil
}

// ParseStatus maps a lowercase name (as used in incident YAML) to a Status.
func ParseStatus(name string) Status {
	switch name {
	case "operational", "none", "":
		return StatusOperational
	case "maintenance":
		return StatusMaintenance
	case "degraded", "partial", "partial_outage", "minor":
		return StatusDegraded
	case "outage", "major", "major_outage", "critical":
		return StatusOutage
	default:
		return StatusUnknown
	}
}

// Worst returns the more severe of two statuses.
func Worst(a, b Status) Status {
	if b > a {
		return b
	}
	return a
}

// Component is one row on the status page. It is usually backed by a
// Kubernetes workload, but the fields are generic so other sources could
// feed it too.
type Component struct {
	// ID is stable and unique across the cluster (kind/namespace/workload).
	// It keys history and probe state so two pages can both have an "API".
	// On pages that hide internal names it is replaced by a public ID made
	// from the display name, such as "api".
	ID string `json:"id"`
	// Name is the display name (from the sutats.io/component annotation).
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	Description string `json:"description,omitempty"`
	Order       int    `json:"order"`
	// Pages lists the page slugs this component appears on, derived from its
	// namespace's labels and its own annotations. Every component is also on
	// the default page.
	Pages []string `json:"pages,omitempty"`

	// Kind, Namespace and Workload identify the Kubernetes object behind it.
	// Empty on pages that hide internal names.
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Workload  string `json:"workload,omitempty"`

	// Observed is what the cluster reports right now, before incidents are
	// layered on. Status is Observed combined with any active incident.
	Observed Status `json:"observed"`
	Status   Status `json:"status"`
	// Reason is a short note explaining a non-operational status.
	Reason string `json:"reason,omitempty"`

	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
	// Updated is the number of replicas on the latest revision, where the
	// workload kind reports it. Used to spot a rollout in progress.
	Updated int32 `json:"updated"`

	// Attention is true when the component is not operational now, or any of
	// its bars had downtime in the last seven days. The page expands such
	// components by default and collapses the rest.
	Attention bool `json:"attention"`

	// Signals are the individual checks behind Observed: the replica count
	// and one entry per HTTP probe.
	Signals []Signal `json:"signals"`
	// Checks is the check configuration in force for this component, after
	// flags, namespace and workload annotations are merged. The zero value
	// means every check on with its default settings. Left out on pages that
	// hide internal names.
	Checks Checks `json:"checks,omitzero"`
	// Probes are the HTTP checks configured on the workload. Not exported
	// in JSON because URLs may be internal.
	Probes []ProbeSpec `json:"-"`
	// TLSHosts are hostnames from Ingresses and Routes in front of the
	// workload whose certificates the prober checks for expiry.
	TLSHosts []string `json:"-"`

	Uptime  float64 `json:"uptime_90d"`
	History []Day   `json:"history"`
}

// Signal kinds.
const (
	SignalPods       = "pods"       // replicas ready, from the workload
	SignalStability  = "stability"  // restarts, crash loops, OOM kills, from pods
	SignalScheduling = "scheduling" // unschedulable pods, image pull failures
	SignalEndpoints  = "endpoints"  // ready endpoints behind matching Services
	SignalIngress    = "ingress"    // Ingress or Route health
	SignalTLS        = "tls"        // certificate expiry on ingress hosts
	SignalHTTP       = "http"       // declared probes
	SignalConfig     = "config"     // a configuration mistake, never alarms
)

// Signal is one health check feeding a component.
type Signal struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	// PublicName and PublicDetail replace Name and Detail on pages that hide
	// internal names, when those name a Service, Ingress, Route or node.
	// Empty means Name and Detail are fine to show.
	PublicName   string `json:"-"`
	PublicDetail string `json:"-"`

	// CountsAs is what this signal contributes to its component's status
	// once its check's mode is applied. Unknown means it does not count.
	// Set by Recompute.
	CountsAs Status `json:"counts_as"`

	// History and Uptime are this signal's own 90-day record.
	Uptime  float64 `json:"uptime_90d"`
	History []Day   `json:"history,omitempty"`
}

// HasBar reports whether the signal has its own history bar.
func (s Signal) HasBar() bool { return s.Kind != SignalConfig }

// CountNote explains, for a failing signal, when its check's mode changes
// what it does to the component. It is empty otherwise.
func (s Signal) CountNote() string {
	if s.Status != StatusDegraded && s.Status != StatusOutage || s.CountsAs == s.Status {
		return ""
	}
	switch s.CountsAs {
	case StatusDegraded:
		return "counts as degraded"
	case StatusOutage:
		return "counts as outage"
	default:
		return "not counted"
	}
}

func (c Component) signalsOfKind(kinds ...string) []Signal {
	var out []Signal
	for _, s := range c.Signals {
		for _, k := range kinds {
			if s.Kind == k {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// Kubernetes returns the signals read from the cluster: replicas, pod
// stability, scheduling and Service endpoints.
func (c Component) Kubernetes() []Signal {
	return c.signalsOfKind(SignalPods, SignalStability, SignalScheduling, SignalEndpoints)
}

// Ingress returns the ingress-layer signals: Ingress or Route health and
// certificate expiry.
func (c Component) Ingress() []Signal { return c.signalsOfKind(SignalIngress, SignalTLS) }

// Endpoints returns the HTTP probes declared on the workload.
func (c Component) Endpoints() []Signal { return c.signalsOfKind(SignalHTTP) }

// Config returns configuration mistakes in the workload's or namespace's
// annotations. They are shown but never affect status.
func (c Component) Config() []Signal { return c.signalsOfKind(SignalConfig) }

// HasDetail reports whether the component has rows beyond the replica
// count worth expanding.
func (c Component) HasDetail() bool { return len(c.Signals) > 1 }

// ProbeSpec describes one HTTP check.
type ProbeSpec struct {
	Name          string
	URL           string
	Method        string
	Headers       map[string]string
	ExpectStatus  int    // 0 means any 2xx
	ExpectBody    string // substring that must appear in the body
	Timeout       time.Duration
	DegradedAfter time.Duration // 0 disables the latency threshold
	Insecure      bool          // skip TLS verification
}

// Recompute sets each signal's CountsAs from its check's mode, then
// Observed, Status and Reason from what the signals count as. Observed is
// the worst counted signal; Reason comes from the first signal at that level.
func (c *Component) Recompute() {
	c.Observed = StatusUnknown
	c.Reason = ""
	for i := range c.Signals {
		s := &c.Signals[i]
		s.CountsAs = c.Checks.Mode(s.Kind).Counts(s.Status)
		st := s.CountsAs
		if st > c.Observed {
			c.Observed = st
			c.Reason = ""
			if st != StatusOperational && s.Detail != "" {
				if s.Kind == SignalPods {
					c.Reason = s.Detail
				} else {
					c.Reason = s.Name + ": " + s.Detail
				}
			}
		} else if st == c.Observed && c.Reason == "" && st != StatusOperational && s.Detail != "" {
			c.Reason = s.Name + ": " + s.Detail
		}
	}
	if c.Observed == StatusUnknown && len(c.Signals) > 0 {
		// Only unknown signals (e.g. probes waiting for their first run)
		// and nothing else: treat as operational so the page is not grey.
		c.Observed = StatusOperational
	}
	c.Status = c.Observed
}

// Day is one cell of the 90-day bar.
type Day struct {
	Date     string  `json:"date"`
	Status   Status  `json:"status"`
	Downtime float64 `json:"downtime_seconds"`
	Observed float64 `json:"observed_seconds"`
}

// Group is a set of components displayed under a shared heading.
type Group struct {
	Name       string      `json:"name"`
	Status     Status      `json:"status"`
	Components []Component `json:"components"`

	// History and Uptime combine the group's components (worst per day),
	// shown as one bar when the group is collapsed.
	History []Day   `json:"history,omitempty"`
	Uptime  float64 `json:"uptime_90d"`
	// Attention is true when any component in the group needs attention.
	Attention bool `json:"attention"`
	// Collapsed is true when the page shows the group as a single row by
	// default: it is healthy and the page is big enough for that to help.
	Collapsed bool `json:"collapsed"`
}

// Anchor is the group's HTML id on the page.
func (g Group) Anchor() string { return "g-" + slugify(g.Name) }

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// IncidentUpdate is one timestamped entry in an incident's timeline.
type IncidentUpdate struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Body   string    `json:"body"`
}

// Incident is an operator-written event: an outage, degradation or a
// scheduled maintenance window. Incidents come from a ConfigMap so they can be
// edited with kubectl or GitOps.
type Incident struct {
	ID         string           `json:"id"`
	Title      string           `json:"title"`
	Impact     Status           `json:"impact"`
	Components []string         `json:"components"`
	Updates    []IncidentUpdate `json:"updates"`

	// Namespace is where the incident's ConfigMap lives. Pages is the explicit
	// list of page slugs from the YAML ("*" means every page); when empty the
	// incident shows on the pages its namespace belongs to.
	Namespace string   `json:"namespace,omitempty"`
	Pages     []string `json:"pages,omitempty"`

	// ScheduledFor / ScheduledUntil mark a maintenance window.
	ScheduledFor   *time.Time `json:"scheduled_for,omitempty"`
	ScheduledUntil *time.Time `json:"scheduled_until,omitempty"`

	// Derived fields.
	StartedAt  time.Time  `json:"started_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Resolved is true once the last update is resolved or completed.
func (i Incident) Resolved() bool { return i.ResolvedAt != nil }

// Latest returns the newest update, or nil if there are none.
func (i Incident) Latest() *IncidentUpdate {
	if len(i.Updates) == 0 {
		return nil
	}
	return &i.Updates[len(i.Updates)-1]
}

// Anchor is the incident's HTML id on the page.
func (i Incident) Anchor() string { return "incident-" + slugify(i.ID) }

// IsMaintenance is true for incidents that describe a planned window.
func (i Incident) IsMaintenance() bool { return i.ScheduledFor != nil }

// ActiveAt reports whether the incident should affect component status at t.
// A maintenance window is active between its scheduled times (unless it was
// marked completed). A normal incident is active until it is resolved.
func (i Incident) ActiveAt(t time.Time) bool {
	if i.Resolved() {
		return false
	}
	if i.IsMaintenance() {
		if t.Before(*i.ScheduledFor) {
			return false
		}
		if i.ScheduledUntil != nil && t.After(*i.ScheduledUntil) {
			return false
		}
		return true
	}
	return true
}

// Affects reports whether the incident names the given component, or names
// no components at all (which means "everything").
func (i Incident) Affects(component string) bool {
	if len(i.Components) == 0 {
		return true
	}
	for _, c := range i.Components {
		if c == component {
			return true
		}
	}
	return false
}

// Page is one status page: a slug, a title, the host it answers on and the
// namespaces that feed it. The default page has an empty slug and shows
// every component.
type Page struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Host       string   `json:"host,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// IsDefault reports whether this is the all-components page.
func (p Page) IsDefault() bool { return p.Slug == "" }

// HasNamespace reports whether ns feeds this page.
func (p Page) HasNamespace(ns string) bool {
	for _, n := range p.Namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// PageLink is a sibling page shown in the navigation.
type PageLink struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Snapshot is everything the page needs, computed at one instant.
type Snapshot struct {
	Title       string    `json:"title"`
	GeneratedAt time.Time `json:"generated_at"`
	Status      Status    `json:"status"`
	Headline    string    `json:"headline"`

	// Page identifies which page this snapshot is for. Pages lists the other
	// pages, for navigation.
	Page  Page       `json:"page"`
	Pages []PageLink `json:"pages,omitempty"`

	Groups []Group `json:"groups"`

	// Problems lists every component that is not operational right now, in
	// page order, so the top of the page can say what is wrong.
	Problems []Problem `json:"problems"`

	// History is the whole system's day-by-day record: the worst status of
	// any component each day. Uptime is the share of time no component was
	// degraded or down.
	History []Day   `json:"history"`
	Uptime  float64 `json:"uptime_90d"`

	ActiveIncidents  []Incident `json:"active_incidents"`
	Scheduled        []Incident `json:"scheduled_maintenance"`
	PastIncidents    []Incident `json:"past_incidents"`
	ComponentCount   int        `json:"component_count"`
	ClusterConnected bool       `json:"cluster_connected"`

	// HasDetail is true when day-level breakdowns are available (database
	// history). The page makes bar cells clickable only then.
	HasDetail bool `json:"detail_available"`

	// Keys maps every bar key on the page (a component ID, or ID::signal) to
	// the history key it reads. The day breakdown accepts only these, so a
	// page that hides internal names cannot be asked about anything else.
	Keys map[string]string `json:"-"`
}

// Key is what history and probe state are stored under: the ID when set,
// else the display name (tests and mock data may not set an ID).
func (c Component) Key() string {
	if c.ID != "" {
		return c.ID
	}
	return c.Name
}

// Anchor is the component's HTML id on the page, safe for use in fragments
// and CSS selectors.
func (c Component) Anchor() string { return "c-" + slugify(c.Key()) }

// PodsDetail is the replica check's detail text, for components that show
// nothing else (no endpoints) on their collapsed row.
func (c Component) PodsDetail() string {
	for _, s := range c.Signals {
		if s.Kind == SignalPods {
			return s.Detail
		}
	}
	return ""
}

// Problem is one entry in the "what is wrong right now" list.
type Problem struct {
	Anchor string `json:"anchor"`
	Name   string `json:"name"`
	Group  string `json:"group,omitempty"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// OnPage reports whether the component belongs on the given page.
func (c Component) OnPage(p Page) bool {
	if p.IsDefault() {
		return true
	}
	for _, s := range c.Pages {
		if s == p.Slug {
			return true
		}
	}
	return false
}

// OnPage reports whether the incident belongs on the given page. Explicit
// pages win; otherwise the incident follows its namespace. Incidents with no
// namespace (a local file, or mock data) show everywhere.
func (i Incident) OnPage(p Page) bool {
	if len(i.Pages) > 0 {
		for _, s := range i.Pages {
			if s == "*" || s == p.Slug || (p.IsDefault() && s == "default") {
				return true
			}
		}
		return p.IsDefault() && i.Namespace == ""
	}
	if p.IsDefault() || i.Namespace == "" {
		return true
	}
	return p.HasNamespace(i.Namespace)
}

// SortComponents orders by Order, then Group, then Name.
func SortComponents(cs []Component) {
	sort.SliceStable(cs, func(a, b int) bool {
		if cs[a].Order != cs[b].Order {
			return cs[a].Order < cs[b].Order
		}
		if cs[a].Group != cs[b].Group {
			return cs[a].Group < cs[b].Group
		}
		return cs[a].Name < cs[b].Name
	})
}
