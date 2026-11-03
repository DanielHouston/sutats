package history

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

func TestRecordAndUptime(t *testing.T) {
	s := New()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	gap := time.Minute
	ok := model.Component{Name: "A", Status: model.StatusOperational}
	down := model.Component{Name: "A", Status: model.StatusOutage}

	s.Record(base, []model.Component{ok}, gap)
	// First sample carries no elapsed time.
	if d := s.Days("A", 1, base); d[0].Observed != 0 {
		t.Fatalf("first sample observed = %v", d[0].Observed)
	}
	for i := 1; i <= 10; i++ {
		c := ok
		if i > 8 {
			c = down
		}
		s.Record(base.Add(time.Duration(i)*15*time.Second), []model.Component{c}, gap)
	}
	days := s.Days("A", 1, base)
	if days[0].Observed != 150 || days[0].Downtime != 30 {
		t.Fatalf("observed=%v downtime=%v", days[0].Observed, days[0].Downtime)
	}
	if days[0].Status != model.StatusOutage {
		t.Fatalf("day status = %v", days[0].Status)
	}
	// 30 s down out of 150 s observed is exactly 80%.
	if u := Uptime(days); math.Abs(u-0.8) > 1e-12 {
		t.Fatalf("uptime = %v, want 0.8", u)
	}
}

func TestMaintenanceIsNotDowntime(t *testing.T) {
	s := New()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for i, st := range []model.Status{model.StatusOperational, model.StatusMaintenance, model.StatusMaintenance, model.StatusOperational} {
		s.Record(base.Add(time.Duration(i)*time.Minute), []model.Component{{Name: "A", Status: st}}, time.Minute)
	}
	d := s.Days("A", 1, base)[0]
	if d.Observed != 180 || d.Downtime != 0 || d.Status != model.StatusMaintenance {
		t.Fatalf("day = %+v, want 180 s observed, no downtime, maintenance", d)
	}
	if u := Uptime([]model.Day{d}); u != 1 {
		t.Fatalf("uptime = %v, want 1", u)
	}
}

func TestGapAcrossMidnightIsSplit(t *testing.T) {
	s := New()
	before := time.Date(2026, 10, 15, 23, 59, 40, 0, time.UTC)
	after := time.Date(2026, 10, 16, 0, 0, 10, 0, time.UTC)
	s.Record(before, []model.Component{{Name: "A", Status: model.StatusOperational}}, time.Minute)
	s.Record(after, []model.Component{{Name: "A", Status: model.StatusOutage}}, time.Minute)
	days := s.Days("A", 2, after)
	// 20 s to midnight on the 15th, 10 s after it on the 16th.
	if d := days[0]; d.Date != "2026-10-15" || d.Observed != 20 || d.Downtime != 20 || d.Status != model.StatusOutage {
		t.Fatalf("15th = %+v", d)
	}
	if d := days[1]; d.Date != "2026-10-16" || d.Observed != 10 || d.Downtime != 10 || d.Status != model.StatusOutage {
		t.Fatalf("16th = %+v", d)
	}
	// A sample exactly at midnight puts the whole gap in the day before.
	s.Record(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC).Add(-30*time.Second), []model.Component{{Name: "B", Status: model.StatusOperational}}, time.Minute)
	s.Record(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), []model.Component{{Name: "B", Status: model.StatusOperational}}, time.Minute)
	days = s.Days("B", 2, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if days[0].Observed != 30 || days[1].Observed != 0 {
		t.Fatalf("at midnight: %+v", days)
	}
}

func TestSignalsRecordedSeparately(t *testing.T) {
	s := New()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	c := model.Component{Name: "A", Status: model.StatusDegraded, Signals: []model.Signal{
		{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational},
		{Kind: model.SignalHTTP, Name: "Health", Status: model.StatusDegraded},
		{Kind: model.SignalStability, Name: "Stability", Status: model.StatusDegraded},
		{Kind: model.SignalConfig, Name: "Probe config", Status: model.StatusUnknown},
	}}
	s.Record(base, []model.Component{c}, time.Minute)
	s.Record(base.Add(10*time.Second), []model.Component{c}, time.Minute)
	if d := s.Days("A", 1, base); d[0].Downtime != 10 {
		t.Fatalf("component downtime = %v", d[0].Downtime)
	}
	if d := s.Days(SignalKey("A", "Pods"), 1, base); d[0].Downtime != 0 || d[0].Observed != 10 {
		t.Fatalf("pods day = %+v", d[0])
	}
	if d := s.Days(SignalKey("A", "Health"), 1, base); d[0].Downtime != 10 || d[0].Status != model.StatusDegraded {
		t.Fatalf("health day = %+v", d[0])
	}
	// The Kubernetes checks have bars too, so they keep history as well.
	if d := s.Days(SignalKey("A", "Stability"), 1, base); d[0].Downtime != 10 || d[0].Status != model.StatusDegraded {
		t.Fatalf("stability day = %+v", d[0])
	}
	if d := s.Days(SignalKey("A", "Probe config"), 1, base); d[0].Observed != 0 {
		t.Fatal("config signals should not be recorded")
	}
}

func TestZeroLengthObservationDoesNotColourDay(t *testing.T) {
	s := New()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	// First sample: caught mid-startup, no elapsed time yet.
	s.Record(base, []model.Component{{Name: "A", Status: model.StatusOutage}}, time.Minute)
	s.Record(base.Add(15*time.Second), []model.Component{{Name: "A", Status: model.StatusOperational}}, time.Minute)
	d := s.Days("A", 1, base)[0]
	if d.Status != model.StatusOperational || d.Downtime != 0 || d.Observed != 15 {
		t.Fatalf("day = %+v, want operational with no downtime", d)
	}
}

func TestGapIsCapped(t *testing.T) {
	s := New()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	c := model.Component{Name: "A", Status: model.StatusOutage}
	s.Record(base, []model.Component{c}, time.Minute)
	s.Record(base.Add(time.Hour), []model.Component{c}, time.Minute)
	if d := s.Days("A", 1, base); d[0].Downtime != 60 {
		t.Fatalf("downtime = %v, want 60 (capped)", d[0].Downtime)
	}
}

func TestDaysFillsMissing(t *testing.T) {
	s := New()
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	days := s.Days("nothing", 90, now)
	if len(days) != 90 || days[0].Date != "2026-07-18" || days[89].Date != "2026-10-15" {
		t.Fatalf("days = %d, first %s last %s", len(days), days[0].Date, days[89].Date)
	}
	if Uptime(days) != -1 {
		t.Fatal("expected -1 uptime with no data")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := New()
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	// Two keys, one with a gap in the middle.
	s.Seed("A", []model.Day{
		{Date: "2026-10-10", Status: model.StatusOperational, Observed: 86400},
		{Date: "2026-10-12", Status: model.StatusOutage, Observed: 86400, Downtime: 3600.4},
		{Date: "2026-10-15", Status: model.StatusDegraded, Observed: 36000, Downtime: 10},
	})
	s.Seed("A::Pods", []model.Day{{Date: "2026-10-15", Status: model.StatusOperational, Observed: 36000}})

	d := s.Export()
	if s.Dirty() {
		t.Fatal("export should clear dirty")
	}
	ser := d.Series["A"]
	if ser.Start != "2026-10-10" || len(ser.Status) != 6 || ser.Status[2] != int(model.StatusOutage) || ser.Downtime[2] != 3600 {
		t.Fatalf("series A = %+v", ser)
	}
	if ser.Status[1] != 0 || ser.Observed[1] != 0 {
		t.Fatal("gap day should be empty")
	}

	blob, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New()
	s2.Import(back)
	got := s2.Days("A", 6, now)
	if got[0].Status != model.StatusOperational || got[1].Status != model.StatusUnknown || got[2].Downtime != 3600 || got[5].Status != model.StatusDegraded {
		t.Fatalf("reimported days = %+v", got)
	}
	if p := s2.Days("A::Pods", 1, now); p[0].Observed != 36000 {
		t.Fatalf("pods = %+v", p[0])
	}
	if _, err := Unmarshal([]byte(`{"v":1}`)); err == nil {
		t.Fatal("old format should be rejected")
	}
}

func TestFileBackend(t *testing.T) {
	dir := t.TempDir()
	b, err := NewFileBackend(dir + "/nested")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := New()
	if err := Load(ctx, b, s); err != nil {
		t.Fatalf("load of missing file should be fine: %v", err)
	}
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	c := model.Component{Name: "A", Status: model.StatusDegraded}
	s.Record(now, []model.Component{c}, time.Minute)
	s.Record(now.Add(10*time.Second), []model.Component{c}, time.Minute)
	if err := Save(ctx, b, s); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, b, s); err != nil {
		t.Fatal("second save with nothing dirty should be a no-op")
	}
	s2 := New()
	if err := Load(ctx, b, s2); err != nil {
		t.Fatal(err)
	}
	d := s2.Days("A", 1, now)
	if d[0].Downtime != 10 || d[0].Status != model.StatusDegraded {
		t.Fatalf("reloaded day = %+v", d[0])
	}
}
