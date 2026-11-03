// Package history records component and signal status over time so the page
// can draw 90-day uptime bars. Data is bucketed per UTC day in memory; a
// Backend (a local file, or a ConfigMap via cmstore) persists it.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

const (
	keepDays   = 100
	dateLayout = "2006-01-02"
)

// Store is a thread-safe in-memory history.
type Store struct {
	mu       sync.RWMutex
	days     map[string]map[string]*model.Day // key -> date -> day
	lastSeen map[string]time.Time
	dirty    bool
}

// New creates an empty store.
func New() *Store {
	return &Store{days: map[string]map[string]*model.Day{}, lastSeen: map[string]time.Time{}}
}

// SignalKey is the history key for one signal of a component, so each probe
// (and the replica check) gets its own bar.
func SignalKey(component, signal string) string { return component + "::" + signal }

// Record adds one sample for each component, and for each of its signals, at
// time now. The time since the previous sample for that key is attributed to
// the current status, capped at maxGap so a stopped process does not get
// counted as an outage.
func (s *Store) Record(now time.Time, comps []model.Component, maxGap time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	seen := map[string]bool{}
	for _, c := range comps {
		seen[c.Key()] = true
		s.sample(now, c.Key(), c.Status, maxGap)
		for _, sig := range c.Signals {
			// Every row with a bar on the page, as the database records.
			if !sig.HasBar() {
				continue
			}
			k := SignalKey(c.Key(), sig.Name)
			seen[k] = true
			s.sample(now, k, sig.Status, maxGap)
		}
	}
	// Forget last-seen for keys that disappeared so a gap is not attributed
	// to them when they return.
	for name := range s.lastSeen {
		if !seen[name] {
			delete(s.lastSeen, name)
		}
	}
	s.prune(now)
	s.dirty = true
}

// Gap is the time a sample at now accounts for: the time since the key's
// last sample, capped at maxGap, or none for its first.
func Gap(now, last time.Time, seen bool, maxGap time.Duration) time.Duration {
	if !seen {
		return 0
	}
	return max(0, min(now.Sub(last), maxGap))
}

// SplitGap splits a sample's gap at the midnight (UTC) before now: the part
// that fell on the day before, and the rest, so each day is credited with
// its own time.
func SplitGap(now time.Time, gap time.Duration) (before, after time.Duration) {
	midnight := now.UTC().Truncate(24 * time.Hour)
	before = max(0, midnight.Sub(now.Add(-gap)))
	return before, gap - before
}

// sample attributes the time since the last sample for key to status.
func (s *Store) sample(now time.Time, key string, status model.Status, maxGap time.Duration) {
	last, seen := s.lastSeen[key]
	before, after := SplitGap(now, Gap(now, last, seen, maxGap))
	s.lastSeen[key] = now
	if before > 0 {
		s.add(now.Add(-after-time.Nanosecond).Format(dateLayout), key, status, before.Seconds())
	}
	s.add(now.Format(dateLayout), key, status, after.Seconds())
}

// add credits elapsed seconds at status to one day of key.
func (s *Store) add(date, key string, status model.Status, elapsed float64) {
	days := s.days[key]
	if days == nil {
		days = map[string]*model.Day{}
		s.days[key] = days
	}
	day := days[date]
	if day == nil {
		day = &model.Day{Date: date}
		days[date] = day
	}
	day.Observed += elapsed
	if status == model.StatusOutage || status == model.StatusDegraded {
		day.Downtime += elapsed
	}
	// A status only colours the day if it lasted some time. The first sample
	// of a key carries no elapsed time, so a workload caught mid-startup
	// does not paint the whole day red.
	if elapsed > 0 {
		day.Status = model.Worst(day.Status, status)
	}
}

func (s *Store) prune(now time.Time) {
	cutoff := now.AddDate(0, 0, -keepDays).Format(dateLayout)
	for key, days := range s.days {
		for date := range days {
			if date < cutoff {
				delete(days, date)
			}
		}
		if len(days) == 0 {
			delete(s.days, key)
		}
	}
}

// Days returns the last n days for a key, oldest first. Days with no data
// have StatusUnknown.
func (s *Store) Days(key string, n int, now time.Time) []model.Day {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now = now.UTC()
	out := make([]model.Day, 0, n)
	days := s.days[key]
	for i := n - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).Format(dateLayout)
		if d, ok := days[date]; ok {
			out = append(out, *d)
		} else {
			out = append(out, model.Day{Date: date})
		}
	}
	return out
}

// Uptime returns the fraction of observed time over the given days that the
// key was not degraded or down, or -1 if nothing has been observed.
func Uptime(days []model.Day) float64 {
	var observed, down float64
	for _, d := range days {
		observed += d.Observed
		down += d.Downtime
	}
	if observed <= 0 {
		return -1
	}
	return 1 - down/observed
}

// Seed loads precomputed days for a key (used by mock mode and tests).
func (s *Store) Seed(key string, days []model.Day) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]*model.Day{}
	for _, d := range days {
		d := d
		m[d.Date] = &d
	}
	s.days[key] = m
	s.dirty = true
}

// All returns a copy of every key's days, oldest first.
func (s *Store) All() map[string][]model.Day {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]model.Day, len(s.days))
	for key, days := range s.days {
		list := make([]model.Day, 0, len(days))
		for _, d := range days {
			list = append(list, *d)
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Date < list[b].Date })
		out[key] = list
	}
	return out
}

// ReplaceAll swaps in a complete set of days per key, as loaded from an
// external store. Keys absent from the map are dropped.
func (s *Store) ReplaceAll(all map[string][]model.Day) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.days = make(map[string]map[string]*model.Day, len(all))
	for key, days := range all {
		m := make(map[string]*model.Day, len(days))
		for _, d := range days {
			d := d
			m[d.Date] = &d
		}
		s.days[key] = m
	}
	s.dirty = false
}

// Dirty reports whether there are unsaved changes.
func (s *Store) Dirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// ---- compact serialisation ----

// Data is the persisted form. Each key is one contiguous series starting at
// Start, with one entry per day: status code, downtime and observed seconds.
// This is roughly ten times smaller than a list of Day objects, which
// matters when it lives in a ConfigMap.
type Data struct {
	Version int               `json:"v"`
	Series  map[string]Series `json:"series"`
}

// Series is one key's days.
type Series struct {
	Start    string    `json:"start"`
	Status   []int     `json:"s"`
	Downtime []float64 `json:"d"`
	Observed []float64 `json:"o"`
}

// Export snapshots the store into Data and clears the dirty flag.
func (s *Store) Export() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Data{Version: 2, Series: map[string]Series{}}
	for key, days := range s.days {
		if len(days) == 0 {
			continue
		}
		dates := make([]string, 0, len(days))
		for d := range days {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		first, _ := time.Parse(dateLayout, dates[0])
		last, _ := time.Parse(dateLayout, dates[len(dates)-1])
		n := int(last.Sub(first).Hours()/24) + 1
		ser := Series{Start: dates[0], Status: make([]int, n), Downtime: make([]float64, n), Observed: make([]float64, n)}
		for i := 0; i < n; i++ {
			if d, ok := days[first.AddDate(0, 0, i).Format(dateLayout)]; ok {
				ser.Status[i] = int(d.Status)
				ser.Downtime[i] = math.Round(d.Downtime)
				ser.Observed[i] = math.Round(d.Observed)
			}
		}
		out.Series[key] = ser
	}
	s.dirty = false
	return out
}

// Import replaces the store's contents with Data. lastSeen is kept so a
// leader that reloads does not lose its sampling cadence.
func (s *Store) Import(d Data) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.days = map[string]map[string]*model.Day{}
	for key, ser := range d.Series {
		start, err := time.Parse(dateLayout, ser.Start)
		if err != nil {
			continue
		}
		m := map[string]*model.Day{}
		for i := range ser.Status {
			if i >= len(ser.Downtime) || i >= len(ser.Observed) {
				break
			}
			if ser.Status[i] == 0 && ser.Observed[i] == 0 {
				continue
			}
			date := start.AddDate(0, 0, i).Format(dateLayout)
			m[date] = &model.Day{Date: date, Status: model.Status(ser.Status[i]), Downtime: ser.Downtime[i], Observed: ser.Observed[i]}
		}
		if len(m) > 0 {
			s.days[key] = m
		}
	}
	s.dirty = false
}

// Marshal encodes Data as JSON.
func (d Data) Marshal() ([]byte, error) { return json.Marshal(d) }

// Unmarshal decodes Data from JSON.
func Unmarshal(b []byte) (Data, error) {
	var d Data
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	if d.Version != 2 {
		return d, fmt.Errorf("unsupported history format version %d", d.Version)
	}
	return d, nil
}

// ---- backends ----

// Backend persists history blobs.
type Backend interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, blob []byte) error
}

// FileBackend keeps history in a JSON file.
type FileBackend struct{ Path string }

// NewFileBackend uses history.json inside dir, creating dir if needed.
func NewFileBackend(dir string) (FileBackend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return FileBackend{}, fmt.Errorf("creating data dir: %w", err)
	}
	return FileBackend{Path: filepath.Join(dir, "history.json")}, nil
}

// Load implements Backend.
func (f FileBackend) Load(context.Context) ([]byte, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Save implements Backend with an atomic rename.
func (f FileBackend) Save(_ context.Context, blob []byte) error {
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("writing history: %w", err)
	}
	return os.Rename(tmp, f.Path)
}

// Load reads the backend into the store. A missing or unreadable blob
// leaves the store empty and returns the error for logging.
func Load(ctx context.Context, b Backend, s *Store) error {
	blob, err := b.Load(ctx)
	if err != nil || blob == nil {
		return err
	}
	d, err := Unmarshal(blob)
	if err != nil {
		return err
	}
	s.Import(d)
	return nil
}

// Save writes the store to the backend if anything changed.
func Save(ctx context.Context, b Backend, s *Store) error {
	if !s.Dirty() {
		return nil
	}
	blob, err := s.Export().Marshal()
	if err != nil {
		return err
	}
	return b.Save(ctx, blob)
}
