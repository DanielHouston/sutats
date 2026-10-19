package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/model"
	"github.com/DanielHouston/sutats/internal/probe"
)

// These tests need a real TimescaleDB. Set SUTATS_TEST_DATABASE_URL, e.g.
//
//	postgres://postgres:sutats@127.0.0.1:5433/sutats?sslmode=disable
//
// They create their own schema and wipe the tables they use.
func open(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("SUTATS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SUTATS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `TRUNCATE samples, probe_state`); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRecordAndLoadDays(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	// Yesterday at noon, so the day bucket is unambiguous and not "today".
	base := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour).Add(12 * time.Hour)
	ok := model.Component{ID: "deployment/ns/a", Status: model.StatusOperational, Signals: []model.Signal{
		{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational},
		{Kind: model.SignalHTTP, Name: "Health", Status: model.StatusOperational, LatencyMS: 80},
	}}
	bad := ok
	bad.Status = model.StatusOutage
	bad.Signals = []model.Signal{
		{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational},
		{Kind: model.SignalHTTP, Name: "Health", Status: model.StatusOutage, LatencyMS: 0},
	}
	for i := 0; i <= 10; i++ {
		c := ok
		if i > 8 {
			c = bad
		}
		if err := d.Record(ctx, base.Add(time.Duration(i)*15*time.Second), []model.Component{c}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	store := history.New()
	if err := d.LoadDays(ctx, store, 90, time.Now()); err != nil {
		t.Fatal(err)
	}
	day := store.Days("deployment/ns/a", 2, time.Now())[0]
	if day.Observed != 150 || day.Downtime != 30 || day.Status != model.StatusOutage {
		t.Fatalf("component day = %+v", day)
	}
	pods := store.Days(history.SignalKey("deployment/ns/a", "Pods"), 2, time.Now())[0]
	if pods.Downtime != 0 || pods.Observed != 150 {
		t.Fatalf("pods day = %+v", pods)
	}

	series, err := d.Series(ctx, history.SignalKey("deployment/ns/a", "Health"), base.Add(-time.Minute), base.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) < 2 || series[len(series)-1].Status != model.StatusOutage {
		t.Fatalf("series = %+v", series)
	}
	if series[0].LatencyMS == nil || *series[0].LatencyMS != 80 {
		t.Fatalf("latency not recorded: %+v", series[0])
	}
	// The upper bound is exclusive.
	if none, _ := d.Series(ctx, history.SignalKey("deployment/ns/a", "Health"), base.Add(-time.Hour), base, time.Minute); len(none) != 0 {
		t.Fatalf("expected nothing before base, got %d", len(none))
	}
}

func TestZeroLengthObservationDoesNotColourDay(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	base := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour).Add(12 * time.Hour)
	down := model.Component{ID: "deployment/ns/x", Status: model.StatusOutage}
	up := model.Component{ID: "deployment/ns/x", Status: model.StatusOperational}
	// First sample mid-startup (no elapsed), then healthy.
	if err := d.Record(ctx, base, []model.Component{down}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(ctx, base.Add(15*time.Second), []model.Component{up}, time.Minute); err != nil {
		t.Fatal(err)
	}
	store := history.New()
	if err := d.LoadDays(ctx, store, 90, time.Now()); err != nil {
		t.Fatal(err)
	}
	day := store.Days("deployment/ns/x", 2, time.Now())[0]
	if day.Status != model.StatusOperational || day.Downtime != 0 {
		t.Fatalf("day = %+v, want operational with no downtime", day)
	}
	// Migrating again is a no-op and keeps the version.
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := d.pool.QueryRow(ctx, `SELECT value FROM sutats_meta WHERE key='schema_version'`).Scan(&v); err != nil || v != "2" {
		t.Fatalf("schema version = %q, %v", v, err)
	}
}

func TestLiveState(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	in := map[string]probe.State{"c::H": {Status: model.StatusDegraded, Detail: "200 · 2.5 s slow", LatencyMS: 2500, Failures: 0}}
	if err := d.SaveLive(ctx, in); err != nil {
		t.Fatal(err)
	}
	in["c::H"] = probe.State{Status: model.StatusOperational, Detail: "200 · 90 ms", LatencyMS: 90}
	if err := d.SaveLive(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := d.LoadLive(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if st := out["c::H"]; st.Detail != "200 · 90 ms" || st.Status != model.StatusOperational {
		t.Fatalf("live = %+v", out)
	}
}

func TestSeedSamplesAndEmpty(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if empty, err := d.Empty(ctx); err != nil || !empty {
		t.Fatalf("empty = %v, %v", empty, err)
	}
	now := time.Now().UTC()
	// Three days of minute samples, far enough back that only a full refresh
	// materialises them, with a 30-minute outage on the middle day.
	from := now.AddDate(0, 0, -62).Truncate(24 * time.Hour)
	to := from.Add(72 * time.Hour)
	outage := from.Add(24*time.Hour + 10*time.Hour)
	series := []SeedSeries{
		{Key: "c::Pods", Component: "c", HTTP: false},
		{Key: "c::Health", Component: "c", HTTP: true, BaseMS: 100},
	}
	wins := []SeedWindow{{Key: "c::Health", Start: outage, End: outage.Add(30 * time.Minute), Status: model.StatusOutage}}
	if err := d.SeedSamples(ctx, series, wins, from, to, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := d.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Compress(ctx); err != nil {
		t.Fatal(err)
	}
	store := history.New()
	if err := d.LoadDays(ctx, store, 90, now); err != nil {
		t.Fatal(err)
	}
	mid := from.Add(24 * time.Hour).Format("2006-01-02")
	find := func(key string) model.Day {
		for _, day := range store.Days(key, 90, now) {
			if day.Date == mid {
				return day
			}
		}
		t.Fatalf("no day %s for %s", mid, key)
		return model.Day{}
	}
	health := find("c::Health")
	if health.Status != model.StatusOutage || health.Downtime != 1800 || health.Observed != 86400 {
		t.Fatalf("health day = %+v", health)
	}
	if pods := find("c::Pods"); pods.Status != model.StatusOperational || pods.Downtime != 0 {
		t.Fatalf("pods day = %+v", pods)
	}
	// The component row is the worst of its signals.
	if comp := find("c"); comp.Status != model.StatusOutage || comp.Downtime != 1800 {
		t.Fatalf("component day = %+v", comp)
	}
	// Latency is present when healthy and absent during the outage.
	pts, err := d.Series(ctx, "c::Health", outage.Add(-5*time.Minute), outage.Add(5*time.Minute), time.Minute)
	if err != nil || len(pts) != 10 {
		t.Fatalf("series = %d points, %v", len(pts), err)
	}
	if pts[0].LatencyMS == nil || *pts[0].LatencyMS < 50 || pts[9].LatencyMS != nil {
		t.Fatalf("latency shape wrong: first %v last %v", pts[0].LatencyMS, pts[9].LatencyMS)
	}
}

func TestLeaderLockIsExclusive(t *testing.T) {
	a := open(t)
	b, err := Open(context.Background(), os.Getenv("SUTATS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aLeads := make(chan struct{})
	go a.Lead(ctx, func(lctx context.Context) { close(aLeads); <-lctx.Done() })
	<-aLeads

	bLed := make(chan struct{}, 1)
	bctx, bcancel := context.WithTimeout(ctx, 2*time.Second)
	defer bcancel()
	go b.Lead(bctx, func(context.Context) { bLed <- struct{}{} })
	select {
	case <-bLed:
		t.Fatal("second process became leader while the first held the lock")
	case <-bctx.Done():
	}

	// Drop a's leadership; b should now get it.
	cancel()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	got := make(chan struct{}, 1)
	go b.Lead(ctx2, func(context.Context) { got <- struct{}{} })
	select {
	case <-got:
	case <-ctx2.Done():
		t.Fatal("second process never took over the lock")
	}
}
