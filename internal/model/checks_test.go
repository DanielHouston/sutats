package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestModeCounts(t *testing.T) {
	cases := []struct {
		mode Mode
		in   Status
		want Status
	}{
		{ModeOn, StatusOutage, StatusOutage},
		{"", StatusDegraded, StatusDegraded},
		{ModeInfo, StatusOutage, StatusUnknown},
		{ModeDegraded, StatusOutage, StatusDegraded},
		{ModeDegraded, StatusOperational, StatusOperational},
		{ModeOutage, StatusDegraded, StatusOutage},
		{ModeOutage, StatusMaintenance, StatusMaintenance},
		{ModeOutage, StatusUnknown, StatusUnknown},
	}
	for _, c := range cases {
		if got := c.mode.Counts(c.in); got != c.want {
			t.Errorf("%q.Counts(%v) = %v, want %v", c.mode, c.in, got, c.want)
		}
	}
}

func TestRecomputeAppliesModes(t *testing.T) {
	c := Component{
		Checks: Checks{Modes: map[string]Mode{SignalStability: ModeInfo, SignalEndpoints: ModeDegraded}},
		Signals: []Signal{
			{Kind: SignalPods, Name: "Pods", Status: StatusOperational, Detail: "2 of 2 ready"},
			{Kind: SignalStability, Name: "Stability", Status: StatusOutage, Detail: "2 pods in CrashLoopBackOff"},
			{Kind: SignalEndpoints, Name: "Endpoints", Status: StatusOutage, Detail: "api has no ready endpoints"},
		},
	}
	c.Recompute()
	if c.Status != StatusDegraded || c.Reason != "Endpoints: api has no ready endpoints" {
		t.Fatalf("status = %v, reason %q", c.Status, c.Reason)
	}
	if n := c.Signals[1].CountNote(); n != "not counted" {
		t.Errorf("info note = %q", n)
	}
	if n := c.Signals[2].CountNote(); n != "counts as degraded" {
		t.Errorf("capped note = %q", n)
	}
	if n := c.Signals[0].CountNote(); n != "" {
		t.Errorf("a healthy signal needs no note, got %q", n)
	}

	// Only info signals failing: the component is fine.
	c.Signals = c.Signals[:2]
	c.Recompute()
	if c.Status != StatusOperational || c.Reason != "" {
		t.Fatalf("info-only failure = %v %q", c.Status, c.Reason)
	}

	// Escalation.
	c.Checks.Modes[SignalPods] = ModeOutage
	c.Signals[0].Status, c.Signals[0].Detail = StatusDegraded, "1 of 2 replicas ready"
	c.Recompute()
	if c.Status != StatusOutage || c.Signals[0].CountNote() != "counts as outage" {
		t.Fatalf("escalated = %v %q", c.Status, c.Signals[0].CountNote())
	}
}

func TestZeroChecksAreDefaults(t *testing.T) {
	var c Checks
	if c.Mode(SignalTLS) != ModeOn || !c.Enabled(SignalTLS) || c.Window() != DefaultRestartWindow || c.Warn() != DefaultTLSWarn || c.MinReady() != 1 {
		t.Fatalf("zero Checks = %v %v %v %v", c.Mode(SignalTLS), c.Window(), c.Warn(), c.MinReady())
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		14 * 24 * time.Hour: "14d", 2 * time.Hour: "2h", 90 * time.Minute: "90m", 45 * time.Second: "45s", 1500 * time.Millisecond: "1.5s",
	} {
		b, _ := json.Marshal(Duration(d))
		var back Duration
		if string(b) != `"`+want+`"` || json.Unmarshal(b, &back) != nil || time.Duration(back) != d {
			t.Errorf("%v: json %s, back %v", d, b, time.Duration(back))
		}
	}
	if _, err := ParseDuration("2w"); err == nil {
		t.Error("2w should not parse")
	}
}
