package incidents

import (
	"testing"

	"github.com/DanielHouston/sutats/internal/model"
)

const sample = `
incidents:
  - title: Elevated errors
    impact: degraded
    components: [API]
    updates:
      - at: 2026-10-15T08:31:00Z
        status: identified
        body: Found it.
      - at: 2026-10-15T08:12:00Z
        status: investigating
        body: Looking.
      - at: 2026-10-15T09:00:00Z
        status: Resolved
        body: Done.
  - title: Storage upgrade
    scheduled_for: 2026-10-20T01:00:00Z
    scheduled_until: 2026-10-20T03:00:00Z
  - title: Broken one
    impact: catastrophic
    updates:
      - at: 2026-10-15T08:12:00Z
        status: investigating
        body: x
  - title: No times at all
    impact: outage
`

func TestParse(t *testing.T) {
	incs, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 2 {
		t.Fatalf("got %d incidents, want 2 (invalid ones skipped): %+v", len(incs), incs)
	}
	// Sorted newest first by start time: the maintenance window (20 Oct) first.
	m, e := incs[0], incs[1]
	if m.Title != "Storage upgrade" || !m.IsMaintenance() || m.Impact != model.StatusMaintenance {
		t.Errorf("maintenance = %+v", m)
	}
	if m.ID != "2026-10-20-storage-upgrade" {
		t.Errorf("id = %q", m.ID)
	}
	if e.Impact != model.StatusDegraded || !e.Resolved() {
		t.Errorf("errors incident = %+v", e)
	}
	if e.Updates[0].Status != "investigating" || e.Updates[2].Status != "resolved" {
		t.Errorf("updates not sorted/normalised: %+v", e.Updates)
	}
	if !e.StartedAt.Equal(e.Updates[0].At) {
		t.Errorf("started = %v", e.StartedAt)
	}
}

func TestParseBadYAML(t *testing.T) {
	if _, err := Parse([]byte("incidents: [")); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseTimeFormats(t *testing.T) {
	for _, s := range []string{"2026-10-15T09:30:00Z", "2026-10-15T09:30+01:00", "2026-10-15 09:30", "2026-10-15"} {
		if _, err := parseTime(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	if _, err := parseTime("yesterday"); err == nil {
		t.Error("expected error for 'yesterday'")
	}
}
