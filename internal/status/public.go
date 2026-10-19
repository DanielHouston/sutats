package status

import (
	"fmt"
	"sort"
	"strings"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
)

// A status page is usually public, and visitors need component names and
// states, not a map of the cluster. Unless Input.ShowInternals is set, Build
// hides everything that names Kubernetes objects: namespaces, workload kinds
// and names, Services, Ingresses, Routes, scheduler messages, configuration
// mistakes and the check settings in force. History and probe state keep
// using the real IDs; only the snapshot changes.

// publicIDs gives every component a public ID made from its display name:
// "Background jobs" becomes "background-jobs". Components that share a name
// get -2, -3 and so on, assigned in order of their real IDs so every page and
// every replica agrees. Computed over all components, not one page, so a
// component has the same ID on every page it appears on.
func publicIDs(all []model.Component) map[string]string {
	names := make(map[string]string, len(all))
	keys := make([]string, 0, len(all))
	for _, c := range all {
		if _, dup := names[c.Key()]; dup {
			continue
		}
		names[c.Key()] = c.Name
		keys = append(keys, c.Key())
	}
	sort.Strings(keys)
	used := make(map[string]bool, len(keys))
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = unique(publicSlug(names[k], "component"), "-", used)
	}
	return out
}

// publicSlug lowercases s and keeps letters and digits, joining runs of
// anything else with one hyphen. An empty result becomes fallback.
func publicSlug(s, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if out == "" {
		return fallback
	}
	return out
}

// unique returns base, or base plus sep and the lowest number from 2 up that
// is not yet used, and marks the result used.
func unique(base, sep string, used map[string]bool) string {
	id := base
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s%s%d", base, sep, n)
	}
	used[id] = true
	return id
}

// hideSignals switches a component's signals to their public names and
// details, drops configuration mistakes (they name annotations, Services and
// namespaces, and are for operators), and records each bar's public key
// against its history key. It reports whether anything shown changed, in
// which case the component's reason must be worked out again.
func hideSignals(c *model.Component, realKey, publicID string, keys map[string]string) bool {
	changed := false
	used := map[string]bool{}
	kept := c.Signals[:0]
	for _, s := range c.Signals {
		if s.Kind == model.SignalConfig {
			changed = true
			continue
		}
		realName := s.Name
		if s.PublicName != "" {
			s.Name = s.PublicName
		}
		if s.PublicDetail != "" {
			s.Detail = s.PublicDetail
		}
		// Two Routes both become "Route"; number them so rows and keys
		// stay distinct.
		s.Name = unique(s.Name, " ", used)
		if s.Name != realName || s.PublicDetail != "" {
			changed = true
		}
		s.PublicName, s.PublicDetail = "", ""
		if s.HasBar() {
			keys[history.SignalKey(publicID, s.Name)] = history.SignalKey(realKey, realName)
		}
		kept = append(kept, s)
	}
	c.Signals = kept
	return changed
}

// hideIncidents removes the namespace from incidents and from their IDs,
// which the ConfigMap source prefixes with it, keeping IDs unique on the
// page.
func hideIncidents(lists ...[]model.Incident) {
	used := map[string]bool{}
	for _, list := range lists {
		for i := range list {
			inc := &list[i]
			id := inc.ID
			if inc.Namespace != "" {
				id = strings.TrimPrefix(id, inc.Namespace+"-")
			}
			inc.ID = unique(id, "-", used)
			inc.Namespace = ""
		}
	}
}

// showKeys records a page's bar keys unchanged, for pages that show
// internal names.
func showKeys(c model.Component, keys map[string]string) {
	keys[c.Key()] = c.Key()
	for _, s := range c.Signals {
		if s.HasBar() {
			k := history.SignalKey(c.Key(), s.Name)
			keys[k] = k
		}
	}
}
