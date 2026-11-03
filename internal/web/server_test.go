package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/mock"
	"github.com/DanielHouston/sutats/internal/model"
	"github.com/DanielHouston/sutats/internal/status"
)

// testServer serves the mock with internal names hidden, the default.
func testServer(t *testing.T) *Server { t.Helper(); return testServerWith(t, false) }

func testServerWith(t *testing.T, showInternals bool) *Server {
	t.Helper()
	now := time.Now()
	m := mock.New()
	comps, _ := m.Components()
	store := history.New()
	mock.SeedHistory(store, comps, now)
	snap := status.Build(status.Input{
		Title: "Test", Now: now, Components: comps,
		Incidents: mock.Incidents(now).Incidents(), History: store, Connected: true,
		ShowInternals: showInternals,
	})
	edge := status.Build(status.Input{
		Title: "Edge", Now: now, Components: comps,
		Incidents: mock.Incidents(now).Incidents(), History: store, Connected: true,
		Page:          model.Page{Slug: "edge", Title: "Edge", Host: "edge.example.test"},
		ShowInternals: showInternals,
	})
	s, err := New(func(host string) model.Snapshot {
		if host == "edge.example.test" {
			return edge
		}
		return snap
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPagesByHost(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Host = "EDGE.example.test:8443"
	s.ServeHTTP(rec, req)
	var snap model.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Page.Slug != "edge" || snap.Title != "Edge" || len(snap.Groups) != 2 {
		t.Fatalf("edge page = slug %q title %q groups %d", snap.Page.Slug, snap.Title, len(snap.Groups))
	}
	// X-Forwarded-Host from a proxy wins over Host.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Host = "127.0.0.1:8085"
	req.Header.Set("X-Forwarded-Host", "edge.example.test")
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"slug": "edge"`) {
		t.Fatal("X-Forwarded-Host was not honoured")
	}
	// Unknown host falls back to the default page.
	if rec := get(t, s, "/api/v1/status"); !strings.Contains(rec.Body.String(), `"slug": ""`) {
		t.Fatal("unknown host should get the default page")
	}
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndexRenders(t *testing.T) {
	s := testServer(t)
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Some systems are degraded. · Test</title>",
		`data-status="degraded"`,
		"Elevated login failures",
		"Scheduled maintenance",
		// The Now column says why a component is in its state.
		`<span class="now__text" title="5 of 5 ready">5 of 5 ready</span>`,
		`<span class="sub__name sub__name--bad">Query API</span> <span class="sub__detail sub__detail--bad">200 · 2.6 s slow</span>`,
		`<span class="sub__name">GraphQL</span> <span class="sub__detail">200 · 131 ms</span>`,
		`aria-label="GraphQL, 90 days,`,
		`<div class="row__label">Kubernetes</div>`,
		`<div class="row__label">HTTP probes</div>`,
		`<span class="status status--degraded">Degraded</span>`,
		`class="bar__day bar__day--outage"`,
		// Problems list names what is wrong and links to the row.
		`<a href="#c-auth">Auth</a>`,
		`<span class="problem__reason">1 of 2 replicas ready</span>`,
		// Auth is degraded so its signal rows start open.
		`data-target="subs-c-auth" aria-controls="subs-c-auth" aria-expanded="true"`,
		`<span class="row__name">All components</span>`,
		// Background jobs' stability check is set to info: failing, shown,
		// and marked as not counting towards the component.
		`<span class="sub__name sub__name--bad">Stability</span> <span class="sub__detail sub__detail--bad">3 restarts in 1 h</span><span class="sub__note">not counted</span>`,
		"Past incidents",
		"API timeouts in eu-west",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Updates are newest first: "identified" must come before "investigating".
	if strings.Index(body, "Identified") > strings.Index(body, "Investigating") {
		t.Error("incident updates are not newest first")
	}
	// Incidents are one line with the latest update, timeline behind a disclosure.
	for _, want := range []string{
		`<span class="incident__latest"><b>Identified</b> One auth replica is failing its health checks after a node reboot. A replacement is starting.</span>`,
		`-elevated-login-failures" aria-expanded="false"`,
		`-elevated-login-failures" hidden>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident markup is missing %q", want)
		}
	}
	// Eight mock components exceed the collapse threshold: healthy Data
	// collapses to its header bar, Platform (degraded) stays open.
	for _, want := range []string{
		`<div class="members" id="members-g-data" hidden>`,
		`<div class="members" id="members-g-platform">`,
		`data-target="members-g-data" aria-controls="members-g-data" aria-expanded="false"`,
		`class="row row--group row--group-collapsed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("group markup is missing %q", want)
		}
	}
	// Signal rows start hidden exactly when the component needs no attention.
	var comps []model.Component
	if err := json.Unmarshal(get(t, s, "/api/v1/components").Body.Bytes(), &comps); err != nil {
		t.Fatal(err)
	}
	var sawHidden, sawOpen bool
	for _, c := range comps {
		if !c.HasDetail() {
			continue
		}
		open := `<div class="subs" id="subs-` + c.Anchor() + `">`
		hidden := `<div class="subs" id="subs-` + c.Anchor() + `" hidden>`
		if c.Attention && !strings.Contains(body, open) {
			t.Errorf("%s needs attention but its rows are not open", c.Name)
		}
		if !c.Attention && !strings.Contains(body, hidden) {
			t.Errorf("%s is fine but its rows are not hidden", c.Name)
		}
		sawHidden = sawHidden || !c.Attention
		sawOpen = sawOpen || c.Attention
	}
	if !sawHidden || !sawOpen {
		t.Logf("mock has open=%v hidden=%v components; both states are not exercised", sawOpen, sawHidden)
	}
	if strings.Contains(body, "all operational") {
		t.Error("the count line should give way to the problems list when something is wrong")
	}
}

func TestDayCellsClickableOnlyWithSeries(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "button") && strings.Contains(body, `class="bar bar--clickable"`) {
		t.Fatal("cells should not be clickable without a series function")
	}
	s.Series = func(_ context.Context, key string, from, to time.Time, bucket time.Duration) (any, error) {
		return []map[string]any{{"time": from.Format(time.RFC3339), "status": "degraded", "observed_seconds": 900, "downtime_seconds": 120, "key": key, "bucket": bucket.String(), "to": to.Format(time.RFC3339)}}, nil
	}
	body = get(t, s, "/").Body.String()
	for _, want := range []string{
		`class="bar bar--clickable" data-key="api"`,
		`data-key="api::GraphQL"`,
		`data-key="api::Pods"`,
		`<button type="button" class="bar__day bar__day--`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// The whole-system bar is derived and stays plain.
	if strings.Contains(body, `data-key=""`) {
		t.Error("hero bar must not be clickable")
	}

	// The page's key is translated to the history key behind it.
	rec := get(t, s, "/api/v1/series?key=api::GraphQL&from=2026-10-16&bucket=15m")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"from": "2026-10-16T00:00:00Z"`) || !strings.Contains(rec.Body.String(), `"to": "2026-10-17T00:00:00Z"`) ||
		!strings.Contains(rec.Body.String(), `"key": "deployment/api/api::GraphQL"`) {
		t.Fatalf("series by date = %d %s", rec.Code, rec.Body.String())
	}
	// Internal keys, and keys of bars on other pages, are refused.
	for _, k := range []string{"deployment/api/api", "nope"} {
		if rec := get(t, s, "/api/v1/series?key="+k+"&from=2026-10-16"); rec.Code != http.StatusNotFound {
			t.Errorf("series for %q = %d, want 404", k, rec.Code)
		}
	}
	for _, bad := range []string{"?key=x&from=yesterday", "?key=x&from=2026-10-16&to=2026-10-15", "?key=x&bucket=1s", "?from=2026-10-16"} {
		if rec := get(t, s, "/api/v1/series"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestBoardPartialHeaders(t *testing.T) {
	rec := get(t, testServer(t), "/partials/board")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Status") != "degraded" || rec.Header().Get("X-Headline") == "" {
		t.Fatalf("code=%d headers=%v", rec.Code, rec.Header())
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("partial should not be a full document")
	}
}

func TestAPI(t *testing.T) {
	s := testServer(t)
	rec := get(t, s, "/api/v1/status")
	var snap model.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Status != model.StatusDegraded || len(snap.Groups) != 3 {
		t.Fatalf("snapshot = status %v, %d groups", snap.Status, len(snap.Groups))
	}
	rec = get(t, s, "/api/v1/components")
	var comps []model.Component
	if err := json.Unmarshal(rec.Body.Bytes(), &comps); err != nil || len(comps) != 8 {
		t.Fatalf("components: err=%v n=%d", err, len(comps))
	}
	// Internal names are hidden by default.
	for _, leak := range []string{`"namespace"`, `"workload"`, `"kind": "Deployment"`, `"checks"`, "deployment/", `"restart_window"`} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("components JSON contains %s", leak)
		}
	}
	for _, c := range comps {
		if c.Name == "Background jobs" && (c.ID != "background-jobs" || c.Status != model.StatusOperational) {
			t.Errorf("background jobs = %q %v", c.ID, c.Status)
		}
	}
}

func TestShowInternals(t *testing.T) {
	s := testServerWith(t, true)
	rec := get(t, s, "/api/v1/components")
	var comps []model.Component
	if err := json.Unmarshal(rec.Body.Bytes(), &comps); err != nil {
		t.Fatal(err)
	}
	// The configuration in force is in the API, defaults written out.
	for _, c := range comps {
		if c.Name == "Background jobs" && (c.ID != "deployment/jobs/worker" || c.Namespace != "jobs" || c.Checks.Mode(model.SignalStability) != model.ModeInfo || c.Checks.Window() != time.Hour) {
			t.Errorf("background jobs = %q %q %+v", c.ID, c.Namespace, c.Checks)
		}
	}
	if !strings.Contains(rec.Body.String(), `"restart_window": "15m"`) || !strings.Contains(rec.Body.String(), `"tls_warn": "7d"`) {
		t.Error("components JSON should show the check settings in force")
	}
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, `<a href="#c-deployment-auth-auth">Auth</a>`) {
		t.Error("anchors should use the real IDs")
	}
}

func TestHealthAndStatic(t *testing.T) {
	s := testServer(t)
	for _, p := range []string{"/healthz", "/readyz", "/static/style.css", "/static/app.js", "/favicon.svg", "/static/fonts/ibm-plex-sans-latin-400-normal.woff2", "/static/fonts/ibm-plex-mono-latin-400-normal.woff2"} {
		if rec := get(t, s, p); rec.Code != http.StatusOK {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	if rec := get(t, s, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d", rec.Code)
	}
}

func TestPct(t *testing.T) {
	if got := pct(-1); got != "No data" {
		t.Errorf("pct(-1) = %q", got)
	}
	if got := pct(0.99995); got != "100%" {
		t.Errorf("pct = %q", got)
	}
}

func TestFooterShowsVersion(t *testing.T) {
	s := testServer(t)
	if body := get(t, s, "/").Body.String(); strings.Contains(body, "foot__version") {
		t.Fatal("no version set, but the footer shows one")
	}
	s.Version = "v1.0.2-32-g5e8b259"
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, `<p class="foot__version">sutats v1.0.2-32-g5e8b259</p>`) {
		t.Fatal("footer is missing the version")
	}
}
