package status

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
)

// Two teams each run an "API". The shop one has a broken Route next to a
// healthy one, a Service with no endpoints and an annotation mistake.
func internalComponents() []model.Component {
	shop := model.Component{
		ID: "deployment/shop/api", Name: "API", Kind: "Deployment", Namespace: "shop", Workload: "api",
		Pages: []string{"shop"}, Desired: 2, Ready: 2,
		Checks: model.Checks{Services: []string{"api"}, TLSHosts: []string{"shop.example.com"}},
		Signals: []model.Signal{
			{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational, Detail: "2 of 2 ready"},
			{Kind: model.SignalEndpoints, Name: "Endpoints", Status: model.StatusOutage,
				Detail: "api has no ready endpoints although pods are ready", PublicDetail: "a Service has no ready endpoints although pods are ready"},
			{Kind: model.SignalIngress, Name: "Route api-public", PublicName: "Route", Status: model.StatusOutage,
				Detail: "host already claimed by route other/x", PublicDetail: "not admitted by the router"},
			{Kind: model.SignalIngress, Name: "Route api-admin", PublicName: "Route", Status: model.StatusOperational, Detail: "admin.example.com"},
			{Kind: model.SignalConfig, Name: "Check config", Status: model.StatusUnknown, Detail: "namespace shop: sutats.io/check.tls: nonsense"},
		},
	}
	shop.Recompute()
	billing := model.Component{
		ID: "deployment/billing/api", Name: "API", Kind: "Deployment", Namespace: "billing", Workload: "api",
		Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational, Detail: "1 of 1 ready"}},
	}
	billing.Recompute()
	return []model.Component{shop, billing}
}

func TestHiddenByDefault(t *testing.T) {
	now := time.Now()
	comps := internalComponents()
	store := history.New()
	store.Seed(history.SignalKey("deployment/shop/api", "Route api-admin"), []model.Day{{Date: now.UTC().Format("2006-01-02"), Status: model.StatusDegraded, Observed: 3600, Downtime: 60}})
	resolved := now.Add(-time.Hour)
	incs := []model.Incident{{ID: "shop-2026-10-19-api-down", Title: "API down", Namespace: "shop", Impact: model.StatusOutage, ResolvedAt: &resolved, StartedAt: now.Add(-2 * time.Hour)}}
	page := model.Page{Slug: "shop", Title: "Shop", Namespaces: []string{"shop"}}

	snap := Build(Input{Now: now, Components: comps, Incidents: incs, History: store, Connected: true, Page: page, AllPages: []model.Page{page}})
	c := snap.Groups[0].Components[0]

	// The shop API sorts after billing's, so it is api-2 on every page.
	if c.ID != "api-2" || c.Anchor() != "c-api-2" || c.Kind != "" || c.Namespace != "" || c.Workload != "" {
		t.Errorf("component = id %q anchor %q kind %q ns %q wl %q", c.ID, c.Anchor(), c.Kind, c.Namespace, c.Workload)
	}
	var names []string
	for _, s := range c.Signals {
		names = append(names, s.Name+": "+s.Detail)
	}
	want := "Pods: 2 of 2 ready|Endpoints: a Service has no ready endpoints although pods are ready|Route: not admitted by the router|Route 2: admin.example.com"
	if got := strings.Join(names, "|"); got != want {
		t.Errorf("signals =\n %s\nwant\n %s", got, want)
	}
	if c.Reason != "Endpoints: a Service has no ready endpoints although pods are ready" || snap.Problems[0].Reason != c.Reason || snap.Problems[0].Anchor != "c-api-2" {
		t.Errorf("reason %q, problem %+v", c.Reason, snap.Problems[0])
	}
	// History was read under the real name before the rename.
	if c.Signals[3].History[len(c.Signals[3].History)-1].Downtime != 60 {
		t.Error("the renamed Route lost its history")
	}
	// Every bar key on the page maps to its history key, and nothing else.
	wantKeys := map[string]string{
		"api-2":            "deployment/shop/api",
		"api-2::Pods":      "deployment/shop/api::Pods",
		"api-2::Endpoints": "deployment/shop/api::Endpoints",
		"api-2::Route":     "deployment/shop/api::Route api-public",
		"api-2::Route 2":   "deployment/shop/api::Route api-admin",
	}
	if len(snap.Keys) != len(wantKeys) {
		t.Errorf("keys = %v", snap.Keys)
	}
	for k, v := range wantKeys {
		if snap.Keys[k] != v {
			t.Errorf("keys[%q] = %q, want %q", k, snap.Keys[k], v)
		}
	}
	// Incidents and the page lose their namespace.
	if inc := snap.PastIncidents[0]; inc.ID != "2026-10-19-api-down" || inc.Namespace != "" || inc.Anchor() != "incident-2026-10-19-api-down" {
		t.Errorf("incident = %q %q", inc.ID, inc.Namespace)
	}
	if snap.Page.Namespaces != nil {
		t.Errorf("page namespaces = %v", snap.Page.Namespaces)
	}
	// Nothing internal anywhere in the JSON.
	b, _ := json.Marshal(snap)
	for _, leak := range []string{"shop/", "billing", "api-public", "api-admin", "already claimed", "Check config", "sutats.io/", `"namespace"`, `"checks"`, "Deployment"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("snapshot JSON contains %q", leak)
		}
	}

	// The caller's components are untouched: other pages, the recorder and
	// the prober still see the real thing.
	if comps[0].ID != "deployment/shop/api" || comps[0].Signals[2].Name != "Route api-public" || len(comps[0].Signals) != 5 {
		t.Errorf("Build changed its input: %q %q %d", comps[0].ID, comps[0].Signals[2].Name, len(comps[0].Signals))
	}
}

func TestShowInternalsKeepsEverything(t *testing.T) {
	now := time.Now()
	snap := Build(Input{Now: now, Components: internalComponents(), Connected: true, ShowInternals: true})
	var shop model.Component
	for _, c := range snap.Groups[0].Components {
		if c.Namespace == "shop" {
			shop = c
		}
	}
	if shop.ID != "deployment/shop/api" || len(shop.Signals) != 5 || shop.Signals[2].Name != "Route api-public" || shop.Checks.Services[0] != "api" {
		t.Errorf("shop = %+v", shop)
	}
	if snap.Keys["deployment/shop/api::Route api-public"] != "deployment/shop/api::Route api-public" {
		t.Errorf("keys = %v", snap.Keys)
	}
}

func TestPublicSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Background jobs": "background-jobs", "API": "api", "A & B  (beta)": "a-b-beta", "  --  ": "component", "Café": "caf",
	} {
		if got := publicSlug(in, "component"); got != want {
			t.Errorf("publicSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
