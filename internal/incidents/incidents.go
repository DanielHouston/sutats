// Package incidents loads operator-written incidents and maintenance windows
// from YAML. The YAML normally lives in a ConfigMap so it can be edited with
// kubectl or managed by GitOps, but it can also be read from a local file.
package incidents

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"github.com/DanielHouston/sutats/internal/model"
)

// Key is the ConfigMap data key that holds the YAML document.
const Key = "incidents.yaml"

// Document is the on-disk / in-ConfigMap shape.
type Document struct {
	Incidents []Entry `json:"incidents"`
}

// Entry is one incident as written by an operator.
type Entry struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Impact         string   `json:"impact"`
	Components     []string `json:"components"`
	ScheduledFor   string   `json:"scheduled_for"`
	ScheduledUntil string   `json:"scheduled_until"`
	Updates        []Update `json:"updates"`
	// Pages restricts (or widens) which pages show the incident. Empty means
	// the pages the ConfigMap's namespace belongs to; "*" means every page.
	Pages []string `json:"pages"`
}

// Update is one timeline entry.
type Update struct {
	At     string `json:"at"`
	Status string `json:"status"`
	Body   string `json:"body"`
}

// Statuses that close an incident.
var closingStatuses = map[string]bool{"resolved": true, "completed": true}

// Parse turns YAML into incidents. It is forgiving: entries with problems are
// skipped and logged rather than failing the whole document, so one typo in a
// ConfigMap cannot blank the page.
func Parse(data []byte) ([]model.Incident, error) {
	var doc Document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing incidents yaml: %w", err)
	}
	return FromDocument(doc), nil
}

// FromDocument converts an already-decoded Document into incidents, skipping
// and logging any entries that are invalid.
func FromDocument(doc Document) []model.Incident {
	var out []model.Incident
	for i, e := range doc.Incidents {
		inc, err := convert(e)
		if err != nil {
			slog.Warn("skipping incident", "index", i, "id", e.ID, "err", err)
			continue
		}
		out = append(out, inc)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

func convert(e Entry) (model.Incident, error) {
	inc := model.Incident{
		ID:         strings.TrimSpace(e.ID),
		Title:      strings.TrimSpace(e.Title),
		Components: e.Components,
	}
	for _, p := range e.Pages {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			inc.Pages = append(inc.Pages, p)
		}
	}
	if inc.Title == "" {
		return inc, fmt.Errorf("missing title")
	}
	inc.Impact = model.ParseStatus(strings.ToLower(strings.TrimSpace(e.Impact)))
	if inc.Impact == model.StatusUnknown {
		return inc, fmt.Errorf("unknown impact %q (use none, maintenance, degraded or outage)", e.Impact)
	}
	if e.ScheduledFor != "" {
		t, err := parseTime(e.ScheduledFor)
		if err != nil {
			return inc, fmt.Errorf("scheduled_for: %w", err)
		}
		inc.ScheduledFor = &t
		if inc.Impact == model.StatusOperational {
			inc.Impact = model.StatusMaintenance
		}
	}
	if e.ScheduledUntil != "" {
		t, err := parseTime(e.ScheduledUntil)
		if err != nil {
			return inc, fmt.Errorf("scheduled_until: %w", err)
		}
		inc.ScheduledUntil = &t
	}
	for j, u := range e.Updates {
		t, err := parseTime(u.At)
		if err != nil {
			return inc, fmt.Errorf("update %d: at: %w", j, err)
		}
		inc.Updates = append(inc.Updates, model.IncidentUpdate{
			At:     t,
			Status: strings.ToLower(strings.TrimSpace(u.Status)),
			Body:   strings.TrimSpace(u.Body),
		})
	}
	sort.SliceStable(inc.Updates, func(a, b int) bool { return inc.Updates[a].At.Before(inc.Updates[b].At) })

	switch {
	case len(inc.Updates) > 0:
		inc.StartedAt = inc.Updates[0].At
	case inc.ScheduledFor != nil:
		inc.StartedAt = *inc.ScheduledFor
	default:
		return inc, fmt.Errorf("needs at least one update or a scheduled_for time")
	}
	if n := len(inc.Updates); n > 0 && closingStatuses[inc.Updates[n-1].Status] {
		t := inc.Updates[n-1].At
		inc.ResolvedAt = &t
	}
	if inc.ID == "" {
		inc.ID = slug(inc.StartedAt.Format("2006-01-02") + "-" + inc.Title)
	}
	return inc, nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use RFC 3339, e.g. 2026-10-15T09:30:00Z)", s)
}

func slug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// Source provides the current incident list.
type Source interface {
	Incidents() []model.Incident
}

// Static is a Source with a fixed list.
type Static []model.Incident

// Incidents implements Source.
func (s Static) Incidents() []model.Incident { return s }

// FileSource re-reads a YAML file on each call (cheap, and handy in dev).
type FileSource struct{ Path string }

// Incidents implements Source.
func (f FileSource) Incidents() []model.Incident {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		slog.Warn("reading incidents file", "path", f.Path, "err", err)
		return nil
	}
	incs, err := Parse(data)
	if err != nil {
		slog.Warn("parsing incidents file", "path", f.Path, "err", err)
	}
	return incs
}

// ConfigMapSource watches every ConfigMap with a given name, in all watched
// namespaces, and keeps the parsed incidents per namespace. Teams post
// incidents by creating that ConfigMap in their own namespace.
type ConfigMapSource struct {
	name      string
	factories []informers.SharedInformerFactory

	mu    sync.RWMutex
	byNS  map[string][]model.Incident
	ready bool
}

// NewConfigMapSource builds a source for ConfigMaps called name. namespaces
// limits where to look; empty means everywhere. Call Run to start it.
func NewConfigMapSource(client kubernetes.Interface, name string, namespaces []string) *ConfigMapSource {
	s := &ConfigMapSource{name: name, byNS: map[string][]model.Incident{}}
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, ns := range namespaces {
		f := informers.NewSharedInformerFactoryWithOptions(client, 10*time.Minute,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) {
				o.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
			}),
		)
		inf := f.Core().V1().ConfigMaps().Informer()
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { s.load(obj) },
			UpdateFunc: func(_, obj interface{}) { s.load(obj) },
			DeleteFunc: func(obj interface{}) {
				if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = d.Obj
				}
				if cm, ok := obj.(*corev1.ConfigMap); ok {
					s.mu.Lock()
					delete(s.byNS, cm.Namespace)
					s.mu.Unlock()
					slog.Info("incidents configmap deleted", "namespace", cm.Namespace)
				}
			},
		})
		s.factories = append(s.factories, f)
	}
	return s
}

func (s *ConfigMapSource) load(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	data, ok := cm.Data[Key]
	if !ok {
		slog.Warn("incidents configmap has no key", "key", Key, "namespace", cm.Namespace, "name", cm.Name)
	}
	list, err := Parse([]byte(data))
	if err != nil {
		slog.Warn("incidents configmap is invalid, keeping previous list", "namespace", cm.Namespace, "err", err)
		return
	}
	for i := range list {
		list[i].Namespace = cm.Namespace
		// Make IDs unique across namespaces so anchors do not collide.
		list[i].ID = cm.Namespace + "-" + list[i].ID
	}
	s.mu.Lock()
	s.byNS[cm.Namespace] = list
	s.mu.Unlock()
	slog.Info("incidents loaded", "namespace", cm.Namespace, "count", len(list))
}

// Run starts the informers and blocks until ctx is done.
func (s *ConfigMapSource) Run(ctx context.Context) {
	for _, f := range s.factories {
		f.Start(ctx.Done())
	}
	for _, f := range s.factories {
		f.WaitForCacheSync(ctx.Done())
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	<-ctx.Done()
}

// Incidents implements Source: every namespace's incidents, newest first.
func (s *ConfigMapSource) Incidents() []model.Incident {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Incident
	for _, list := range s.byNS {
		out = append(out, list...)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}
