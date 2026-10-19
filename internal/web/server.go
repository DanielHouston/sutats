// Package web serves the status page, a JSON API and health endpoints.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Resolver returns the snapshot for the page served on host (without port,
// lowercase). Unknown hosts get the default page.
type Resolver func(host string) model.Snapshot

// SeriesFunc returns fine-grained history for one key (a component ID, or
// "<id>::<signal>") between two instants, bucketed. Only available with
// database history; the result is encoded as JSON as-is.
type SeriesFunc func(ctx context.Context, key string, from, to time.Time, bucket time.Duration) (any, error)

// Server is the HTTP handler set.
type Server struct {
	resolve Resolver
	ready   func() bool
	tmpl    *template.Template
	mux     *http.ServeMux

	// Series, when set, enables GET /api/v1/series.
	Series SeriesFunc
}

// New builds a Server. ready reports whether the backing data source has
// completed its first sync, for /readyz.
func New(resolve Resolver, ready func() bool) (*Server, error) {
	funcs := template.FuncMap{
		"pct":      pct,
		"ago":      ago,
		"when":     when,
		"rfc3339":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"dayTitle": dayTitle,
		"plural":   plural,
		"title":    strings.Title, //nolint:staticcheck // ASCII status words only
		"upper":    strings.ToUpper,
		"bar": func(days []model.Day, label, key string, clickable bool) barData {
			return barData{Days: days, Label: label, Key: key, Clickable: clickable && key != ""}
		},
		"subWithDetail": func(id string, s model.Signal, detail bool) subData {
			return subData{ID: id, Signal: s, Detail: detail}
		},
		"signalKey": func(id, name string) string { return id + "::" + name },
		"bad":       func(s model.Status) bool { return s >= model.StatusDegraded },
		"newestFirst": func(u []model.IncidentUpdate) []model.IncidentUpdate {
			out := make([]model.IncidentUpdate, len(u))
			for i, x := range u {
				out[len(u)-1-i] = x
			}
			return out
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	s := &Server{resolve: resolve, ready: ready, tmpl: tmpl, mux: http.NewServeMux()}

	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /partials/board", s.board)
	s.mux.HandleFunc("GET /favicon.svg", s.favicon)
	s.mux.HandleFunc("GET /api/v1/status", s.apiStatus)
	s.mux.HandleFunc("GET /api/v1/components", s.apiComponents)
	s.mux.HandleFunc("GET /api/v1/series", s.apiSeries)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready != nil && !s.ready() {
			http.Error(w, "not synced", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.mux.ServeHTTP(w, r)
	if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
	}
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("rendering template", "name", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

// Host returns the request's host name, lowercased and without a port.
// Behind a proxy that rewrites Host, X-Forwarded-Host is honoured.
func Host(r *http.Request) string {
	h := r.Header.Get("X-Forwarded-Host")
	if h == "" {
		h = r.Host
	}
	if i := strings.IndexByte(h, ','); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSpace(h))
}

func (s *Server) snapshot(r *http.Request) model.Snapshot {
	snap := s.resolve(Host(r))
	snap.HasDetail = s.Series != nil
	return snap
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, "index.html", s.snapshot(r))
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot(r)
	w.Header().Set("X-Status", snap.Status.String())
	w.Header().Set("X-Headline", snap.Headline)
	w.Header().Set("X-Generated-At", snap.GeneratedAt.UTC().Format(time.RFC3339))
	s.render(w, "board", snap)
}

var statusColours = map[model.Status]string{
	model.StatusOperational: "#1E9C57",
	model.StatusMaintenance: "#2F6FD6",
	model.StatusDegraded:    "#D98A0B",
	model.StatusOutage:      "#C9332B",
	model.StatusUnknown:     "#9AA19C",
}

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	colour := statusColours[s.snapshot(r).Status]
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	// A three-cell bar, matching the page's one visual device.
	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="%s"><rect x="2" y="6" width="8" height="20"/><rect x="12" y="6" width="8" height="20"/><rect x="22" y="6" width="8" height="20"/></svg>`, colour)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		slog.Error("encoding json", "err", err)
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot(r))
}

func (s *Server) apiComponents(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot(r)
	var comps []model.Component
	for _, g := range snap.Groups {
		comps = append(comps, g.Components...)
	}
	if comps == nil {
		comps = []model.Component{}
	}
	writeJSON(w, comps)
}

// apiSeries serves fine-grained history for one key:
//
//	?key=<id or id::signal>&hours=24&bucket=1m          the last N hours
//	?key=...&from=2026-10-16&to=2026-10-17&bucket=15m    an explicit window
//
// from/to accept a date (UTC midnight) or an RFC 3339 time; to is exclusive.
// The key must be one of the bars on the requesting host's page, as shown
// in its HTML; it is translated to the history key there.
func (s *Server) apiSeries(w http.ResponseWriter, r *http.Request) {
	if s.Series == nil {
		http.Error(w, "fine-grained history needs database history (--history=database)", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	now := time.Now()
	from, to := now.Add(-24*time.Hour), now
	if h := q.Get("hours"); h != "" {
		v, err := strconv.ParseFloat(h, 64)
		if err != nil || v <= 0 || v > 24*100 {
			http.Error(w, "hours must be a number between 0 and 2400", http.StatusBadRequest)
			return
		}
		from = now.Add(-time.Duration(v * float64(time.Hour)))
	}
	if f := q.Get("from"); f != "" {
		t, err := parseWhen(f)
		if err != nil {
			http.Error(w, "from: "+err.Error(), http.StatusBadRequest)
			return
		}
		from = t
		to = from.Add(24 * time.Hour)
	}
	if tt := q.Get("to"); tt != "" {
		t, err := parseWhen(tt)
		if err != nil {
			http.Error(w, "to: "+err.Error(), http.StatusBadRequest)
			return
		}
		to = t
	}
	if !to.After(from) || to.Sub(from) > 100*24*time.Hour {
		http.Error(w, "window must be positive and at most 100 days", http.StatusBadRequest)
		return
	}
	bucket := time.Minute
	if b := r.URL.Query().Get("bucket"); b != "" {
		if v, err := time.ParseDuration(b); err == nil && v >= 15*time.Second && v <= 24*time.Hour {
			bucket = v
		} else {
			http.Error(w, "bucket must be a duration between 15s and 24h", http.StatusBadRequest)
			return
		}
	}
	historyKey, ok := s.snapshot(r).Keys[key]
	if !ok {
		http.Error(w, "no bar with that key on this page", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	series, err := s.Series(ctx, historyKey, from, to, bucket)
	if err != nil {
		slog.Error("series query", "key", historyKey, "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"key": key, "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339),
		"bucket": bucket.String(), "points": series,
	})
}

func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("use a date (2026-10-16) or an RFC 3339 time")
}

// --- template helpers ---

// barData feeds the shared "bar" template. Key identifies the series behind
// the bar; when Clickable, cells become buttons that open a day breakdown.
type barData struct {
	Days      []model.Day
	Label     string
	Key       string
	Clickable bool
}

// subData feeds the "sub" template: a signal, its component's ID and
// whether day breakdowns are available.
type subData struct {
	ID     string
	Signal model.Signal
	Detail bool
}

func pct(f float64) string {
	if f < 0 {
		return "No data"
	}
	v := f * 100
	switch {
	case v >= 99.995:
		return "100%"
	case v >= 99.95:
		return fmt.Sprintf("%.2f%%", v)
	default:
		return fmt.Sprintf("%.2f%%", v)
	}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func when(t time.Time) string {
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}

func dayTitle(d model.Day) string {
	t, err := time.Parse("2006-01-02", d.Date)
	label := d.Date
	if err == nil {
		label = t.Format("2 Jan")
	}
	switch {
	case d.Observed == 0:
		return label + ": no data"
	case d.Downtime == 0:
		return label + ": " + d.Status.Label()
	default:
		return fmt.Sprintf("%s: %s, %s of downtime", label, d.Status.Label(), humanDur(time.Duration(d.Downtime)*time.Second))
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
