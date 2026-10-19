// Command sutats serves status pages for workloads running on Kubernetes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/DanielHouston/sutats/internal/checks"
	"github.com/DanielHouston/sutats/internal/cmstore"
	"github.com/DanielHouston/sutats/internal/db"
	"github.com/DanielHouston/sutats/internal/history"
	"github.com/DanielHouston/sutats/internal/incidents"
	"github.com/DanielHouston/sutats/internal/k8s"
	"github.com/DanielHouston/sutats/internal/mock"
	"github.com/DanielHouston/sutats/internal/model"
	"github.com/DanielHouston/sutats/internal/probe"
	"github.com/DanielHouston/sutats/internal/status"
	"github.com/DanielHouston/sutats/internal/web"
)

// version is set at build time with -ldflags "-X main.version=v1.0.0".
var version = "dev"

// componentSource is what the k8s watcher and the mock both provide.
type componentSource interface {
	Components() ([]model.Component, error)
	Pages() []model.Page
	Synced() bool
}

// snapshots is one build of every page, looked up by host.
type snapshots struct {
	def    model.Snapshot
	byHost map[string]model.Snapshot
}

func main() {
	var (
		listen         = flag.String("listen", envOr("SUTATS_LISTEN", ":8080"), "address to serve on")
		title          = flag.String("title", envOr("SUTATS_TITLE", "Status"), "title of the default (all components) page")
		kubeconfig     = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (default: in-cluster, then ~/.kube/config)")
		namespaces     = flag.String("namespaces", os.Getenv("SUTATS_NAMESPACES"), "comma-separated namespaces to watch (default: all)")
		allWorkloads   = flag.Bool("all-workloads", envBool("SUTATS_ALL_WORKLOADS"), "show every workload, not only annotated ones")
		incidentsName  = flag.String("incidents-configmap", envOr("SUTATS_INCIDENTS_CONFIGMAP", "sutats-incidents"), "name of the incidents ConfigMap looked for in every watched namespace")
		incidentsFile  = flag.String("incidents-file", os.Getenv("SUTATS_INCIDENTS_FILE"), "read incidents from a local YAML file instead of ConfigMaps")
		pageHost       = flag.String("page-host", os.Getenv("SUTATS_PAGE_HOST"), "host template for pages, e.g. {slug}.status.example.com")
		defaultHost    = flag.String("host", os.Getenv("SUTATS_HOST"), "host of the default page, used for links back from other pages")
		historyMode    = flag.String("history", envOr("SUTATS_HISTORY", "auto"), "where history lives: auto, database, configmap or file (auto = database if DATABASE_URL is set, else configmap in-cluster, else file)")
		databaseURL    = flag.String("database-url", os.Getenv("DATABASE_URL"), "TimescaleDB connection string for database history")
		dataDir        = flag.String("data-dir", envOr("SUTATS_DATA_DIR", "/data"), "directory for the history file in file mode; empty keeps it in memory")
		stateNamespace = flag.String("state-namespace", os.Getenv("POD_NAMESPACE"), "namespace for the state ConfigMaps and the leader Lease (configmap mode)")
		leaderElection = flag.Bool("leader-election", true, "elect one replica to sample and write; others serve (database and configmap modes)")
		interval       = flag.Duration("interval", 15*time.Second, "how often to sample component status for history")
		probeParallel  = flag.Int("probe-parallel", 16, "how many HTTP probes may run at the same time")
		useMock        = flag.Bool("mock", envBool("SUTATS_MOCK"), "serve fake data without a cluster")
		seedDays       = flag.Int("seed-history", 0, "one-shot: write N days of invented history for the current components into the database, ending where real samples begin, then exit (demo and trial installs only)")
		debug          = flag.Bool("debug", envBool("SUTATS_DEBUG"), "verbose logging")
		showVersion    = flag.Bool("version", false, "print the version and exit")
		showInternals  = flag.Bool("show-internals", envBool("SUTATS_SHOW_INTERNALS"), "show namespaces, workload, Service, Ingress and Route names, configuration mistakes and check settings on the page and in the API (for pages only visible inside your network)")
		// --check.<name> for every check: the cluster-wide defaults that
		// Namespace and workload sutats.io/check.<name> annotations override.
		checkFlags = checks.Flags(flag.CommandLine)
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("sutats", version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	checkDefaults, err := checkFlags()
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			slog.Error("invalid check flag", "err", line)
		}
		os.Exit(2)
	}

	if *pageHost != "" && !strings.Contains(*pageHost, "{slug}") {
		slog.Error("--page-host must contain {slug}, e.g. {slug}.status.example.com")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- data sources ----
	var (
		source   componentSource
		incs     incidents.Source
		client   kubernetes.Interface
		runnable []func(context.Context)
	)
	now := time.Now()
	store := history.New()

	switch {
	case *useMock:
		m := mock.New()
		m.UseChecks(checkDefaults)
		source = m
		incs = mock.Incidents(now)
		comps, _ := m.Components()
		mock.SeedHistory(store, comps, now)
		slog.Info("running in mock mode")
	default:
		var err error
		var dyn dynamic.Interface
		client, dyn, err = k8s.Clients(*kubeconfig)
		if err != nil {
			slog.Error("cannot connect to kubernetes", "err", err)
			os.Exit(1)
		}
		w := k8s.NewWithClient(client, k8s.Options{
			Namespaces: splitList(*namespaces), AllWorkloads: *allWorkloads,
			Dynamic: dyn, Routes: k8s.HasRouteAPI(client), Checks: checkDefaults,
		})
		source = w
		runnable = append(runnable, w.Run)

		if *incidentsFile != "" {
			incs = incidents.FileSource{Path: *incidentsFile}
		} else {
			cmSource := incidents.NewConfigMapSource(client, *incidentsName, splitList(*namespaces))
			incs = cmSource
			runnable = append(runnable, cmSource.Run)
		}
	}

	prober := probe.New(*probeParallel)

	// ---- history persistence ----
	mode := *historyMode
	if mode == "auto" {
		switch {
		case *databaseURL != "":
			mode = "database"
		case client != nil && *stateNamespace != "":
			mode = "configmap"
		default:
			mode = "file"
		}
	}
	var (
		backend   history.Backend
		cmHistory *cmstore.Store
		cmLive    *cmstore.Store
		tsdb      *db.DB
	)
	switch mode {
	case "file":
		if *dataDir != "" {
			fb, err := history.NewFileBackend(*dataDir)
			if err != nil {
				slog.Warn("history will not be persisted", "dir", *dataDir, "err", err)
			} else {
				backend = fb
			}
		}
	case "configmap":
		if client == nil || *stateNamespace == "" {
			slog.Error("configmap history needs a cluster connection and --state-namespace (or POD_NAMESPACE)")
			os.Exit(2)
		}
		cmHistory = cmstore.New(client, *stateNamespace, "sutats-history", "history.json", true)
		cmLive = cmstore.New(client, *stateNamespace, "sutats-live", "live.json", false)
		backend = cmHistory
	case "database":
		if *databaseURL == "" {
			slog.Error("database history needs --database-url or DATABASE_URL")
			os.Exit(2)
		}
		var err error
		tsdb, err = db.Open(ctx, *databaseURL)
		if err != nil {
			slog.Error("database", "err", err)
			os.Exit(1)
		}
		defer tsdb.Close()
		if err := tsdb.Migrate(ctx); err != nil {
			slog.Error("database migration", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown --history mode", "mode", mode)
		os.Exit(2)
	}
	withLeader := *leaderElection && (mode == "configmap" || mode == "database")
	slog.Info("history", "mode", mode, "leader_election", withLeader)

	if *seedDays > 0 {
		if tsdb == nil {
			slog.Error("--seed-history needs database history")
			os.Exit(2)
		}
		os.Exit(seedHistory(ctx, tsdb, source, runnable, *seedDays))
	}

	switch {
	case tsdb != nil:
		if *useMock {
			if empty, err := tsdb.Empty(ctx); err == nil && empty {
				comps, _ := source.Components()
				if err := seedMockSamples(ctx, tsdb, comps, now); err != nil {
					slog.Warn("seeding mock history", "err", err)
				} else {
					slog.Info("seeded mock samples into the database", "days", mock.HistoryDays)
				}
			}
		}
		if err := tsdb.LoadDays(ctx, store, status.HistoryDays, time.Now()); err != nil {
			slog.Warn("could not load history from the database", "err", err)
		}
		// Start from the last saved probe results so a restarted leader keeps
		// its failure counts and a failing endpoint does not drop back to
		// Unknown until it fails three more times.
		if states, err := tsdb.LoadLive(ctx, 10**interval); err != nil {
			slog.Warn("could not load probe state from the database", "err", err)
		} else if len(states) > 0 {
			prober.Import(states)
		}
	case backend != nil && !*useMock:
		if err := history.Load(ctx, backend, store); err != nil {
			slog.Warn("could not load history, starting fresh", "err", err)
		}
	}

	// ---- snapshot cache ----
	var (
		mu        sync.RWMutex
		cache     snapshots
		lastBuild time.Time
	)
	build := func() snapshots {
		comps, err := source.Components()
		if err != nil {
			slog.Error("listing components", "err", err)
		}
		prober.Attach(comps)
		pages := resolveHosts(source.Pages(), *pageHost)
		incList := incs.Incidents()
		n := time.Now()
		connected := source.Synced()

		out := snapshots{byHost: map[string]model.Snapshot{}}
		out.def = status.Build(status.Input{
			Title: *title, Now: n, Components: comps, Incidents: incList, History: store, Connected: connected,
			Page: model.Page{Title: *title}, AllPages: pages, Links: links(pages, *title, *defaultHost, model.Page{}),
			ShowInternals: *showInternals,
		})
		for _, p := range pages {
			if p.Host == "" {
				continue
			}
			out.byHost[p.Host] = status.Build(status.Input{
				Title: p.Title, Now: n, Components: comps, Incidents: incList, History: store, Connected: connected,
				Page: p, AllPages: pages, Links: links(pages, *title, *defaultHost, p),
				ShowInternals: *showInternals,
			})
		}
		return out
	}
	refresh := func() {
		s := build()
		mu.Lock()
		cache = s
		lastBuild = time.Now()
		mu.Unlock()
	}
	cache = build()

	// ---- sampler (runs on the single instance, or on the leader) ----
	var leading atomic.Bool
	record := func(c context.Context, n time.Time, comps []model.Component) {
		if tsdb != nil {
			if err := tsdb.Record(c, n, comps, 3**interval); err != nil {
				slog.Error("recording samples", "err", err)
			}
			return
		}
		store.Record(n, comps, 3**interval)
	}
	saveHistory := func(c context.Context) {
		if backend == nil {
			return
		}
		if err := history.Save(c, backend, store); err != nil {
			slog.Error("saving history", "err", err)
		}
	}
	saveLive := func(c context.Context) {
		states := prober.Export()
		switch {
		case tsdb != nil:
			if err := tsdb.SaveLive(c, states); err != nil {
				slog.Error("saving live state", "err", err)
			}
		case cmLive != nil:
			blob, err := json.Marshal(states)
			if err != nil {
				return
			}
			if err := cmLive.Save(c, blob); err != nil {
				slog.Error("saving live state", "err", err)
			}
		}
	}
	sampler := func(sctx context.Context) {
		t := time.NewTicker(*interval)
		defer t.Stop()
		saveEvery := time.NewTicker(time.Minute)
		defer saveEvery.Stop()
		for {
			select {
			case <-sctx.Done():
				// Save on process shutdown, but not when we merely lost
				// leadership: another replica owns the state now.
				if ctx.Err() != nil {
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					saveHistory(c)
					cancel()
				}
				return
			case <-saveEvery.C:
				saveHistory(sctx)
			case <-t.C:
				if !source.Synced() {
					continue
				}
				comps, err := source.Components()
				if err != nil {
					continue
				}
				// Probes get most of the interval; the hard cap stops a
				// hanging endpoint from delaying the next sample.
				probeCtx, cancel := context.WithTimeout(sctx, *interval-time.Second)
				prober.Run(probeCtx, comps)
				cancel()
				prober.Attach(comps)
				n := time.Now()
				status.ApplyIncidents(comps, incs.Incidents(), source.Pages(), n)
				record(sctx, n, comps)
				saveLive(sctx)
				if tsdb == nil {
					refresh()
				}
			}
		}
	}
	lead := func(lctx context.Context) {
		leading.Store(true)
		defer leading.Store(false)
		sampler(lctx)
	}

	switch {
	case tsdb != nil:
		// Everyone reloads day buckets and live results from the database on
		// a timer, so leader and followers serve the same thing.
		runnable = append(runnable, func(ctx context.Context) {
			t := time.NewTicker(*interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					c, cancel := context.WithTimeout(ctx, *interval)
					if err := tsdb.LoadDays(c, store, status.HistoryDays, time.Now()); err != nil {
						slog.Warn("reloading history", "err", err)
					}
					if !leading.Load() {
						if states, err := tsdb.LoadLive(c, 10**interval); err == nil {
							prober.Import(states)
						} else {
							slog.Warn("reloading live state", "err", err)
						}
					}
					cancel()
					refresh()
				}
			}
		})
		if withLeader {
			runnable = append(runnable, func(ctx context.Context) { tsdb.Lead(ctx, lead) })
		} else {
			runnable = append(runnable, lead)
		}
	case withLeader:
		identity := envOr("POD_NAME", hostnameOr("sutats"))
		runnable = append(runnable, func(ctx context.Context) {
			for ctx.Err() == nil {
				lock := &resourcelock.LeaseLock{
					LeaseMeta:  metav1.ObjectMeta{Name: "sutats-leader", Namespace: *stateNamespace},
					Client:     client.CoordinationV1(),
					LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
				}
				leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
					Lock:            lock,
					ReleaseOnCancel: true,
					LeaseDuration:   15 * time.Second,
					RenewDeadline:   10 * time.Second,
					RetryPeriod:     2 * time.Second,
					Callbacks: leaderelection.LeaderCallbacks{
						OnStartedLeading: func(lctx context.Context) {
							slog.Info("became leader; sampling and writing history", "identity", identity)
							lead(lctx)
						},
						OnStoppedLeading: func() {
							slog.Info("no longer leader; serving from shared state", "identity", identity)
						},
						OnNewLeader: func(id string) {
							if id != identity {
								slog.Info("current leader", "identity", id)
							}
						},
					},
				})
				time.Sleep(time.Second)
			}
		})
		runnable = append(runnable, func(ctx context.Context) {
			cmHistory.Watch(ctx, func(blob []byte) {
				if leading.Load() {
					return // our own write echoing back
				}
				d, err := history.Unmarshal(blob)
				if err != nil {
					slog.Warn("ignoring history update", "err", err)
					return
				}
				store.Import(d)
				refresh()
			})
		})
		runnable = append(runnable, func(ctx context.Context) {
			cmLive.Watch(ctx, func(blob []byte) {
				if leading.Load() {
					return
				}
				var states map[string]probe.State
				if err := json.Unmarshal(blob, &states); err != nil {
					slog.Warn("ignoring live state update", "err", err)
					return
				}
				prober.Import(states)
				refresh()
			})
		})
	default:
		runnable = append(runnable, lead)
	}

	// Rebuild on demand too, so the page reflects the cluster within a
	// second of a change even between sampling ticks. The rebuild is cheap
	// (informer caches, no API calls) but we rate-limit it anyway.
	resolve := func(host string) model.Snapshot {
		mu.RLock()
		c := cache
		fresh := time.Since(lastBuild) < time.Second
		mu.RUnlock()
		if !fresh {
			refresh()
			mu.RLock()
			c = cache
			mu.RUnlock()
		}
		if s, ok := c.byHost[host]; ok {
			return s
		}
		return c.def
	}

	srv, err := web.New(resolve, source.Synced)
	if err != nil {
		slog.Error("starting web server", "err", err)
		os.Exit(1)
	}
	if tsdb != nil {
		srv.Series = func(c context.Context, key string, from, to time.Time, bucket time.Duration) (any, error) {
			return tsdb.Series(c, key, from, to, bucket)
		}
	}
	httpSrv := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	var wg sync.WaitGroup
	for _, r := range runnable {
		wg.Add(1)
		go func(r func(context.Context)) { defer wg.Done(); r(ctx) }(r)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("sutats listening", "version", version, "addr", *listen, "title", *title, "page_host", *pageHost)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
	wg.Wait()
}

// seedHistory is the one-shot --seed-history mode: start the component
// source long enough to learn the current components, invent a history for
// them that ends where the real samples begin, write it, and exit.
func seedHistory(ctx context.Context, tsdb *db.DB, source componentSource, runnable []func(context.Context), days int) int {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, r := range runnable {
		go r(sctx)
	}
	deadline := time.Now().Add(90 * time.Second)
	for !source.Synced() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if !source.Synced() {
		slog.Error("component source did not sync in time")
		return 1
	}
	comps, err := source.Components()
	if err != nil || len(comps) == 0 {
		slog.Error("no components to seed", "err", err)
		return 1
	}
	// Probes only become signals once the prober has run; for seeding, the
	// declared probes are enough to know which endpoint series exist.
	for i := range comps {
		for _, p := range comps[i].Probes {
			comps[i].Signals = append(comps[i].Signals, model.Signal{Kind: model.SignalHTTP, Name: p.Name})
		}
		for _, h := range comps[i].TLSHosts {
			comps[i].Signals = append(comps[i].Signals, model.Signal{Kind: model.SignalTLS, Name: "TLS " + h})
		}
	}
	now := time.Now().UTC()
	to := now
	if earliest, ok, err := tsdb.Earliest(ctx); err == nil && ok && earliest.Before(to) {
		to = earliest
	}
	from := now.AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	if !to.After(from) {
		slog.Info("nothing to seed: real samples already cover the window", "from", from, "earliest", to)
		return 0
	}
	series, wins := mock.RandomScenario(comps, now, days)
	if err := writeSeed(ctx, tsdb, series, wins, from, to); err != nil {
		slog.Error("seeding history", "err", err)
		return 1
	}
	slog.Info("seeded history", "components", len(comps), "series", len(series), "windows", len(wins), "from", from.Format("2006-01-02"), "to", to.Format(time.RFC3339))
	return 0
}

// seedMockSamples fills an empty database with one sample per minute for the
// whole mock scenario, then materialises day buckets and compresses old
// chunks so the demo starts the way a long-running install would look.
func seedMockSamples(ctx context.Context, tsdb *db.DB, comps []model.Component, now time.Time) error {
	series, wins := mock.Scenario(comps, now)
	from := now.UTC().AddDate(0, 0, -mock.HistoryDays+1).Truncate(24 * time.Hour)
	return writeSeed(ctx, tsdb, series, wins, from, now)
}

// writeSeed converts a scenario into database rows at one-minute resolution
// and then refreshes the day aggregate and compresses old chunks.
func writeSeed(ctx context.Context, tsdb *db.DB, series []mock.Series, wins []mock.Window, from, to time.Time) error {
	ss := make([]db.SeedSeries, 0, len(series))
	for _, s := range series {
		ss = append(ss, db.SeedSeries{Key: s.Key, Component: s.Component, HTTP: s.Kind == model.SignalHTTP, BaseMS: s.BaseMS})
	}
	ws := make([]db.SeedWindow, 0, len(wins))
	for _, w := range wins {
		ws = append(ws, db.SeedWindow{Key: w.Key, Start: w.Start, End: w.End, Status: w.Status})
	}
	seedCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := tsdb.SeedSamples(seedCtx, ss, ws, from, to, time.Minute); err != nil {
		return err
	}
	if err := tsdb.Refresh(seedCtx); err != nil {
		return err
	}
	return tsdb.Compress(seedCtx)
}

// resolveHosts fills in each page's host from the template unless the page
// set its own. Pages that end up with no host are not served.
func resolveHosts(pages []model.Page, tmpl string) []model.Page {
	out := make([]model.Page, len(pages))
	copy(out, pages)
	for i := range out {
		if out[i].Host == "" && tmpl != "" {
			out[i].Host = strings.ToLower(strings.ReplaceAll(tmpl, "{slug}", out[i].Slug))
		}
		if out[i].Host == "" {
			slog.Debug("page has no host; set --page-host or the sutats.io/page-host annotation", "page", out[i].Slug)
		}
	}
	return out
}

// links builds the navigation for one page: the default page first (unless
// we are on it, or its host is unknown), then every other page with a host.
func links(pages []model.Page, defaultTitle, defaultHost string, current model.Page) []model.PageLink {
	var out []model.PageLink
	if !current.IsDefault() && defaultHost != "" {
		out = append(out, model.PageLink{Title: defaultTitle, URL: "//" + strings.ToLower(defaultHost) + "/"})
	}
	for _, p := range pages {
		if p.Host == "" || p.Slug == current.Slug {
			continue
		}
		out = append(out, model.PageLink{Slug: p.Slug, Title: p.Title, URL: "//" + p.Host + "/"})
	}
	return out
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "1" || v == "true" || v == "yes"
}
