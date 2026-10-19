// Package k8s watches Deployments, StatefulSets and DaemonSets and turns the
// ones that opt in (via annotations) into status-page components. It also
// watches Namespaces to discover pages, and Pods, Services, EndpointSlices,
// Ingresses and Routes to derive the checks around each workload.
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/DanielHouston/sutats/internal/checks"
	"github.com/DanielHouston/sutats/internal/model"
	"github.com/DanielHouston/sutats/internal/probe"
)

// Annotation keys understood on workloads.
const (
	AnnComponent   = "sutats.io/component"   // display name; opts the workload in
	AnnGroup       = "sutats.io/group"       // heading to list it under
	AnnDescription = "sutats.io/description" // one-line description
	AnnOrder       = "sutats.io/order"       // integer sort key, lower first
	AnnMaintenance = "sutats.io/maintenance" // "true" forces Under maintenance
	AnnHide        = "sutats.io/hide"        // "true" hides it even in --all-workloads mode
	AnnPages       = "sutats.io/pages"       // extra page slugs, comma separated
)

// Labels and annotations understood on namespaces.
const (
	LabelPage    = "sutats.io/page"       // the page this namespace's components appear on
	AnnNSPages   = "sutats.io/pages"      // extra pages, comma separated
	AnnPageTitle = "sutats.io/page-title" // title for the page named in the label
	AnnPageHost  = "sutats.io/page-host"  // explicit host for that page
)

// routeGVR is the OpenShift Route resource, read through the dynamic client
// so the OpenShift API types are not a dependency.
var routeGVR = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Options configures the watcher.
type Options struct {
	// Kubeconfig path. Empty means in-cluster config, falling back to the
	// default kubeconfig locations.
	Kubeconfig string
	// Namespaces to watch. Empty means all namespaces.
	Namespaces []string
	// AllWorkloads lists every workload, not just annotated ones.
	AllWorkloads bool
	// Resync is the informer resync period.
	Resync time.Duration
	// Dynamic enables Route discovery when the cluster has the Route API.
	Dynamic dynamic.Interface
	// Routes says whether to watch OpenShift Routes (needs Dynamic).
	Routes bool
	// Checks is the default check configuration, from flags. Namespace and
	// workload annotations override it.
	Checks checks.Layer
}

// restartTrack remembers a pod's restart count so increments can be dated.
type restartTrack struct {
	count  int32
	events []time.Time
	seen   time.Time
}

// Watcher keeps informer caches warm and builds component lists on demand.
type Watcher struct {
	opts      Options
	client    kubernetes.Interface
	factories []informers.SharedInformerFactory
	nsFactory informers.SharedInformerFactory
	dynFacts  []dynamicinformer.DynamicSharedInformerFactory
	routes    []cache.GenericLister

	mu           sync.RWMutex
	synced       bool
	pagesEnabled bool
	restarts     map[types.UID]*restartTrack
	warned       map[string]bool
}

// Clients builds a Kubernetes clientset and a dynamic client from
// kubeconfig (or in-cluster config).
func Clients(kubeconfig string) (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := loadConfig(kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	cfg.UserAgent = "sutats"
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return client, dyn, nil
}

// Client builds a Kubernetes clientset from kubeconfig (or in-cluster config).
func Client(kubeconfig string) (kubernetes.Interface, error) {
	c, _, err := Clients(kubeconfig)
	return c, err
}

// HasRouteAPI reports whether the cluster serves OpenShift Routes.
func HasRouteAPI(client kubernetes.Interface) bool {
	res, err := client.Discovery().ServerResourcesForGroupVersion("route.openshift.io/v1")
	if err != nil || res == nil {
		return false
	}
	for _, r := range res.APIResources {
		if r.Name == "routes" {
			return true
		}
	}
	return false
}

// New builds a Watcher. It does not start anything until Run is called.
func New(opts Options) (*Watcher, error) {
	client, dyn, err := Clients(opts.Kubeconfig)
	if err != nil {
		return nil, err
	}
	opts.Dynamic = dyn
	opts.Routes = HasRouteAPI(client)
	return NewWithClient(client, opts), nil
}

// NewWithClient is New with an injected client; useful for tests.
func NewWithClient(client kubernetes.Interface, opts Options) *Watcher {
	if opts.Resync == 0 {
		opts.Resync = 10 * time.Minute
	}
	w := &Watcher{opts: opts, client: client, restarts: map[types.UID]*restartTrack{}, warned: map[string]bool{}}
	namespaces := opts.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, ns := range namespaces {
		f := informers.NewSharedInformerFactoryWithOptions(client, opts.Resync, informers.WithNamespace(ns))
		// Register the informers we want so Start creates them.
		f.Apps().V1().Deployments().Informer()
		f.Apps().V1().StatefulSets().Informer()
		f.Apps().V1().DaemonSets().Informer()
		f.Apps().V1().ReplicaSets().Informer()
		f.Core().V1().Pods().Informer()
		f.Core().V1().Services().Informer()
		f.Discovery().V1().EndpointSlices().Informer()
		f.Networking().V1().Ingresses().Informer()
		w.factories = append(w.factories, f)
		if opts.Routes && opts.Dynamic != nil {
			df := dynamicinformer.NewFilteredDynamicSharedInformerFactory(opts.Dynamic, opts.Resync, ns, nil)
			w.routes = append(w.routes, df.ForResource(routeGVR).Lister())
			w.dynFacts = append(w.dynFacts, df)
		}
	}
	w.nsFactory = informers.NewSharedInformerFactory(client, opts.Resync)
	w.nsFactory.Core().V1().Namespaces().Informer()
	return w
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return cfg, nil
}

// Run starts the informers and blocks until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	// Page discovery needs cluster-wide namespace access. Check once instead
	// of letting the informer retry forbidden lists forever.
	pages := true
	if _, err := w.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		pages = false
		slog.Warn("cannot list namespaces; pages are disabled and only the default page is served", "err", err)
	}
	for _, f := range w.factories {
		f.Start(ctx.Done())
	}
	for _, df := range w.dynFacts {
		df.Start(ctx.Done())
	}
	if pages {
		w.nsFactory.Start(ctx.Done())
	}
	for _, f := range w.factories {
		for typ, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				slog.Warn("informer cache failed to sync", "type", typ.String())
			}
		}
	}
	for _, df := range w.dynFacts {
		df.WaitForCacheSync(ctx.Done())
	}
	if pages {
		w.nsFactory.WaitForCacheSync(ctx.Done())
	}
	w.mu.Lock()
	w.synced = true
	w.pagesEnabled = pages
	w.mu.Unlock()
	slog.Info("kubernetes informers synced", "namespaces", w.opts.Namespaces, "all_workloads", w.opts.AllWorkloads, "pages", pages, "routes", w.opts.Routes)
	<-ctx.Done()
}

// Synced reports whether the caches have completed their first list.
func (w *Watcher) Synced() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.synced
}

func (w *Watcher) pagesOn() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.pagesEnabled
}

// ---- pages ----

// namespacePages reads a namespace's page membership. primary is the slug
// from the label (title and host annotations apply to it); all includes the
// extra slugs from the pages annotation.
func namespacePages(ns *corev1.Namespace) (primary string, all []string) {
	primary = cleanSlug(ns.Labels[LabelPage], "namespace "+ns.Name)
	if primary != "" {
		all = append(all, primary)
	}
	for _, s := range splitSlugs(ns.Annotations[AnnNSPages], "namespace "+ns.Name) {
		if s != primary {
			all = append(all, s)
		}
	}
	return primary, all
}

func cleanSlug(s, where string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !slugRe.MatchString(s) {
		slog.Warn("ignoring invalid page slug (use lowercase letters, digits and hyphens)", "slug", s, "on", where)
		return ""
	}
	return s
}

func splitSlugs(s, where string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if c := cleanSlug(p, where); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// nsMeta is what a namespace contributes to the components in it: the
// pages they appear on and its check configuration.
type nsMeta struct {
	pages     []string
	checks    checks.Layer
	checksErr error
}

// nsMetas reads every namespace from the cache. Without namespace access
// (pages disabled) it is empty, and only flags and workload annotations
// configure checks.
func (w *Watcher) nsMetas() map[string]nsMeta {
	out := map[string]nsMeta{}
	if !w.pagesOn() {
		return out
	}
	nss, err := w.nsFactory.Core().V1().Namespaces().Lister().List(labels.Everything())
	if err != nil {
		return out
	}
	for _, ns := range nss {
		_, all := namespacePages(ns)
		l, err := checks.FromAnnotations(ns.Annotations)
		if len(all) > 0 || l != nil || err != nil {
			out[ns.Name] = nsMeta{pages: all, checks: l, checksErr: err}
		}
	}
	return out
}

// warnOnce logs a configuration mistake the first time it is seen. The
// page shows it for as long as it lasts; the log only needs it once.
func (w *Watcher) warnOnce(msg string, args ...any) {
	key := fmt.Sprint(append([]any{msg}, args...)...)
	w.mu.Lock()
	seen := w.warned[key]
	if !seen {
		if len(w.warned) > 1000 {
			w.warned = map[string]bool{}
		}
		w.warned[key] = true
	}
	w.mu.Unlock()
	if !seen {
		slog.Warn(msg, args...)
	}
}

// Pages lists every page discovered from namespace labels and workload
// annotations, sorted by slug. Titles and hosts come from the namespace
// carrying the page label; a page named only by workloads gets a title
// derived from its slug and no host (the host template fills that in).
func (w *Watcher) Pages() []model.Page {
	if !w.pagesOn() {
		return nil
	}
	bySlug := map[string]*model.Page{}
	get := func(slug string) *model.Page {
		p := bySlug[slug]
		if p == nil {
			p = &model.Page{Slug: slug}
			bySlug[slug] = p
		}
		return p
	}

	nss, _ := w.nsFactory.Core().V1().Namespaces().Lister().List(labels.Everything())
	sort.Slice(nss, func(a, b int) bool { return nss[a].Name < nss[b].Name })
	for _, ns := range nss {
		primary, all := namespacePages(ns)
		for _, slug := range all {
			get(slug).Namespaces = append(get(slug).Namespaces, ns.Name)
		}
		if primary != "" {
			p := get(primary)
			if t := strings.TrimSpace(ns.Annotations[AnnPageTitle]); t != "" && p.Title == "" {
				p.Title = t
			}
			if h := strings.ToLower(strings.TrimSpace(ns.Annotations[AnnPageHost])); h != "" && p.Host == "" {
				p.Host = h
			}
		}
	}
	// Pages named only on workloads.
	comps, _ := w.Components()
	for _, c := range comps {
		for _, slug := range c.Pages {
			get(slug)
		}
	}

	out := make([]model.Page, 0, len(bySlug))
	for _, p := range bySlug {
		if p.Title == "" {
			p.Title = humanise(p.Slug)
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Slug < out[b].Slug })
	return out
}

func humanise(slug string) string {
	s := strings.ReplaceAll(slug, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- components ----

// surroundings is everything from one namespace factory's caches that a
// component's checks are derived from, indexed for quick lookup.
type surroundings struct {
	podsByOwner map[types.UID][]*corev1.Pod
	services    []*corev1.Service
	slices      map[string][]*discoveryv1.EndpointSlice // service name -> slices
	ingresses   []*networkingv1.Ingress
	routes      []*unstructured.Unstructured
}

func (w *Watcher) gather(f informers.SharedInformerFactory, routes cache.GenericLister) surroundings {
	s := surroundings{podsByOwner: map[types.UID][]*corev1.Pod{}, slices: map[string][]*discoveryv1.EndpointSlice{}}

	// ReplicaSet -> Deployment, so Deployment pods can be found by UID.
	rsOwner := map[types.UID]types.UID{}
	if rss, err := f.Apps().V1().ReplicaSets().Lister().List(labels.Everything()); err == nil {
		for _, rs := range rss {
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" {
					rsOwner[rs.UID] = o.UID
				}
			}
		}
	}
	if pods, err := f.Core().V1().Pods().Lister().List(labels.Everything()); err == nil {
		for _, p := range pods {
			for _, o := range p.OwnerReferences {
				owner := o.UID
				if o.Kind == "ReplicaSet" {
					if d, ok := rsOwner[o.UID]; ok {
						owner = d
					}
				}
				s.podsByOwner[owner] = append(s.podsByOwner[owner], p)
			}
		}
	}
	s.services, _ = f.Core().V1().Services().Lister().List(labels.Everything())
	if slices, err := f.Discovery().V1().EndpointSlices().Lister().List(labels.Everything()); err == nil {
		for _, es := range slices {
			if name := es.Labels[discoveryv1.LabelServiceName]; name != "" {
				s.slices[es.Namespace+"/"+name] = append(s.slices[es.Namespace+"/"+name], es)
			}
		}
	}
	s.ingresses, _ = f.Networking().V1().Ingresses().Lister().List(labels.Everything())
	if routes != nil {
		if objs, err := routes.List(labels.Everything()); err == nil {
			for _, o := range objs {
				if u, ok := o.(*unstructured.Unstructured); ok {
					s.routes = append(s.routes, u)
				}
			}
		}
	}
	return s
}

// Components returns the current component list built from informer caches.
// History and incident effects are not applied here.
func (w *Watcher) Components() ([]model.Component, error) {
	nss := w.nsMetas()
	now := time.Now()
	seenPods := map[types.UID]bool{}
	var out []model.Component
	for i, f := range w.factories {
		var rl cache.GenericLister
		if i < len(w.routes) {
			rl = w.routes[i]
		}
		env := w.gather(f, rl)

		deps, err := f.Apps().V1().Deployments().Lister().List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if c, ok := w.fromDeployment(d, nss); ok {
				w.surround(&c, d.UID, d.Spec.Template.Labels, env, now, seenPods)
				out = append(out, c)
			}
		}
		sts, err := f.Apps().V1().StatefulSets().Lister().List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, s := range sts {
			if c, ok := w.fromStatefulSet(s, nss); ok {
				w.surround(&c, s.UID, s.Spec.Template.Labels, env, now, seenPods)
				out = append(out, c)
			}
		}
		dss, err := f.Apps().V1().DaemonSets().Lister().List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, d := range dss {
			if c, ok := w.fromDaemonSet(d, nss); ok {
				w.surround(&c, d.UID, d.Spec.Template.Labels, env, now, seenPods)
				out = append(out, c)
			}
		}
	}
	w.pruneRestarts(seenPods)
	model.SortComponents(out)
	return out, nil
}

// base fills the annotation-driven fields. ok is false if the workload should
// not be shown.
func (w *Watcher) base(kind string, meta metav1.ObjectMeta, nss map[string]nsMeta) (model.Component, bool) {
	ann := meta.Annotations
	name := strings.TrimSpace(ann[AnnComponent])
	if name == "" {
		if !w.opts.AllWorkloads || ann[AnnHide] == "true" {
			return model.Component{}, false
		}
		name = meta.Namespace + "/" + meta.Name
	}
	c := model.Component{
		ID:          strings.ToLower(kind) + "/" + meta.Namespace + "/" + meta.Name,
		Name:        name,
		Group:       strings.TrimSpace(ann[AnnGroup]),
		Description: strings.TrimSpace(ann[AnnDescription]),
		Kind:        kind,
		Namespace:   meta.Namespace,
		Workload:    meta.Name,
	}
	if c.Group == "" && w.opts.AllWorkloads && ann[AnnComponent] == "" {
		c.Group = meta.Namespace
	}
	if o, err := strconv.Atoi(strings.TrimSpace(ann[AnnOrder])); err == nil {
		c.Order = o
	}
	where := kind + " " + meta.Namespace + "/" + meta.Name
	ns := nss[meta.Namespace]
	c.Pages = append(c.Pages, ns.pages...)
	for _, s := range splitSlugs(ann[AnnPages], where) {
		if !contains(c.Pages, s) {
			c.Pages = append(c.Pages, s)
		}
	}
	var probeErr error
	c.Probes, probeErr = probe.ParseAnnotations(ann)
	if probeErr != nil {
		// Surface the mistake on the page (as an unknown, non-alarming
		// signal) instead of silently ignoring the probe.
		configError(&c, "Probe config", probeErr.Error())
		w.warnOnce("invalid probe annotation", "workload", where, "err", probeErr)
	}
	w.configure(&c, ann, ns, where)
	return c, true
}

// configure resolves the component's checks from the flags, its namespace
// and its own annotations, in that order of precedence (lowest first), and
// reports mistakes in any of them as a config signal. A mistake never turns
// a check off: the words that were wrong are ignored and the rest apply.
func (w *Watcher) configure(c *model.Component, ann map[string]string, ns nsMeta, where string) {
	var msgs []string
	for _, m := range lines(ns.checksErr) {
		msgs = append(msgs, "namespace "+c.Namespace+": "+m)
	}
	own, err := checks.FromAnnotations(ann)
	msgs = append(msgs, lines(err)...)
	services, err := checks.Services(ann)
	msgs = append(msgs, lines(err)...)

	c.Checks = checks.Resolve(w.opts.Checks, ns.checks, own)
	c.Checks.Services = services
	if !c.Checks.Enabled(model.SignalHTTP) {
		c.Probes = nil
	}
	if len(msgs) > 0 {
		configError(c, "Check config", msgs...)
		w.warnOnce("invalid check configuration", "workload", where, "err", strings.Join(msgs, "; "))
	}
}

// lines splits a joined error into its messages.
func lines(err error) []string {
	if err == nil {
		return nil
	}
	return strings.Split(err.Error(), "\n")
}

// configError adds messages to the named config signal, creating it if
// needed. Config signals are Unknown: shown, never alarming.
func configError(c *model.Component, name string, msgs ...string) {
	detail := strings.Join(msgs, "; ")
	for i := range c.Signals {
		if c.Signals[i].Kind == model.SignalConfig && c.Signals[i].Name == name {
			c.Signals[i].Detail += "; " + detail
			return
		}
	}
	c.Signals = append(c.Signals, model.Signal{Kind: model.SignalConfig, Name: name, Status: model.StatusUnknown, Detail: detail})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// finish records the replica check as a signal. The pod-derived signals and
// Recompute are added by surround.
func finish(c *model.Component, status model.Status, reason string) {
	detail := reason
	if detail == "" {
		detail = fmt.Sprintf("%d of %d ready", c.Ready, c.Desired)
	}
	c.Signals = append([]model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: status, Detail: detail}}, c.Signals...)
	c.Recompute()
}

func (w *Watcher) fromDeployment(d *appsv1.Deployment, nss map[string]nsMeta) (model.Component, bool) {
	c, ok := w.base("Deployment", d.ObjectMeta, nss)
	if !ok {
		return c, false
	}
	c.Desired = 1
	if d.Spec.Replicas != nil {
		c.Desired = *d.Spec.Replicas
	}
	c.Ready = d.Status.ReadyReplicas
	c.Updated = d.Status.UpdatedReplicas
	rolloutStuck := false
	for _, cond := range d.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing && cond.Status == "False" && cond.Reason == "ProgressDeadlineExceeded" {
			rolloutStuck = true
		}
	}
	st, reason := derive(c, d.Annotations, rolloutStuck, d.Generation, d.Status.ObservedGeneration)
	finish(&c, st, reason)
	return c, true
}

func (w *Watcher) fromStatefulSet(s *appsv1.StatefulSet, nss map[string]nsMeta) (model.Component, bool) {
	c, ok := w.base("StatefulSet", s.ObjectMeta, nss)
	if !ok {
		return c, false
	}
	c.Desired = 1
	if s.Spec.Replicas != nil {
		c.Desired = *s.Spec.Replicas
	}
	c.Ready = s.Status.ReadyReplicas
	c.Updated = s.Status.UpdatedReplicas
	st, reason := derive(c, s.Annotations, false, s.Generation, s.Status.ObservedGeneration)
	finish(&c, st, reason)
	return c, true
}

func (w *Watcher) fromDaemonSet(d *appsv1.DaemonSet, nss map[string]nsMeta) (model.Component, bool) {
	c, ok := w.base("DaemonSet", d.ObjectMeta, nss)
	if !ok {
		return c, false
	}
	c.Desired = d.Status.DesiredNumberScheduled
	c.Ready = d.Status.NumberReady
	c.Updated = d.Status.UpdatedNumberScheduled
	st, reason := derive(c, d.Annotations, false, d.Generation, d.Status.ObservedGeneration)
	finish(&c, st, reason)
	return c, true
}

// derive turns replica counts into a status and a short reason.
func derive(c model.Component, ann map[string]string, rolloutStuck bool, gen, observedGen int64) (model.Status, string) {
	if ann[AnnMaintenance] == "true" {
		return model.StatusMaintenance, "Marked as under maintenance"
	}
	if c.Desired == 0 {
		return model.StatusMaintenance, "Scaled to zero"
	}
	switch {
	case c.Ready == 0:
		return model.StatusOutage, fmt.Sprintf("0 of %d replicas ready", c.Desired)
	case c.Ready < c.Desired:
		if rolloutStuck {
			return model.StatusDegraded, fmt.Sprintf("Rollout stuck, %d of %d replicas ready", c.Ready, c.Desired)
		}
		return model.StatusDegraded, fmt.Sprintf("%d of %d replicas ready", c.Ready, c.Desired)
	}
	if rolloutStuck {
		return model.StatusDegraded, "Rollout stuck"
	}
	if observedGen < gen {
		return model.StatusOperational, "Rollout in progress"
	}
	return model.StatusOperational, ""
}

// ---- the checks around a workload ----

// surround adds the pod, endpoint and ingress signals for one component,
// as its check configuration allows, and recomputes its observed status.
func (w *Watcher) surround(c *model.Component, owner types.UID, podLabels map[string]string, env surroundings, now time.Time, seenPods map[types.UID]bool) {
	cfg := c.Checks
	pods := env.podsByOwner[owner]
	stability, scheduling, notReadyWhy := w.podSignals(pods, now, seenPods, cfg)
	if notReadyWhy != "" && c.Ready < c.Desired && c.Desired > 0 {
		c.Signals[0].Detail += " · " + notReadyWhy
	}
	if cfg.Enabled(model.SignalStability) {
		c.Signals = append(c.Signals, stability)
	}
	if cfg.Enabled(model.SignalScheduling) {
		c.Signals = append(c.Signals, scheduling)
	}

	var services []*corev1.Service
	if cfg.Services != nil {
		var missing []string
		services, missing = servicesByName(env.services, c.Namespace, cfg.Services)
		if len(missing) > 0 {
			configError(c, "Check config", fmt.Sprintf("%s: no Service named %s in namespace %s", checks.AnnServices, strings.Join(missing, ", "), c.Namespace))
		}
	} else {
		services = matchServices(env.services, c.Namespace, podLabels)
	}

	var tlsHosts []string
	if len(services) > 0 {
		if cfg.Enabled(model.SignalEndpoints) {
			c.Signals = append(c.Signals, endpointSignal(services, env.slices, c, cfg.MinReady()))
		}
		names := map[string]bool{}
		for _, s := range services {
			names[s.Name] = true
		}
		for _, ing := range env.ingresses {
			if ing.Namespace != c.Namespace || !ingressUses(ing, names) {
				continue
			}
			if cfg.Enabled(model.SignalIngress) {
				c.Signals = append(c.Signals, ingressSignal(ing))
			}
			for _, t := range ing.Spec.TLS {
				for _, h := range t.Hosts {
					tlsHosts = appendUnique(tlsHosts, h)
				}
			}
		}
		for _, r := range env.routes {
			if r.GetNamespace() != c.Namespace {
				continue
			}
			sig, host, tls, ok := routeSignal(r, names)
			if !ok {
				continue
			}
			if cfg.Enabled(model.SignalIngress) {
				c.Signals = append(c.Signals, sig)
			}
			if tls && host != "" {
				tlsHosts = appendUnique(tlsHosts, host)
			}
		}
	}
	// The certificate check covers discovered hosts and any configured ones.
	// The prober checks TLSHosts, so leaving it empty turns the check off.
	if cfg.Enabled(model.SignalTLS) {
		for _, h := range cfg.TLSHosts {
			tlsHosts = appendUnique(tlsHosts, h)
		}
		c.TLSHosts = tlsHosts
	}
	c.Recompute()
}

// servicesByName picks the named Services in ns, in the order given, and
// lists the names that do not exist.
func servicesByName(services []*corev1.Service, ns string, names []string) (found []*corev1.Service, missing []string) {
	for _, n := range names {
		var hit *corev1.Service
		for _, s := range services {
			if s.Namespace == ns && s.Name == n {
				hit = s
				break
			}
		}
		if hit == nil {
			missing = append(missing, n)
			continue
		}
		found = append(found, hit)
	}
	return found, missing
}

func appendUnique(list []string, s string) []string {
	if contains(list, s) {
		return list
	}
	return append(list, s)
}

// podSignals derives the Stability and Scheduling signals from a workload's
// pods, and a short reason for replicas that are not ready. Restarts are
// tracked even when the stability check is off, so turning it back on does
// not start from a blank slate.
func (w *Watcher) podSignals(pods []*corev1.Pod, now time.Time, seen map[types.UID]bool, cfg model.Checks) (stability, scheduling model.Signal, notReadyWhy string) {
	restartWindow := cfg.Window()
	var crashLoop, oom, recentRestarts, unschedulable, pullProblems, running int
	var reasons []string
	note := func(s string) {
		if s != "" && !contains(reasons, s) {
			reasons = append(reasons, s)
		}
	}
	var schedMsg string

	w.mu.Lock()
	for _, p := range pods {
		seen[p.UID] = true
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		running++
		if p.Status.Phase == corev1.PodPending {
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
					unschedulable++
					if schedMsg == "" {
						schedMsg = cond.Message
					}
				}
			}
		}
		var restarts int32
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			restarts += cs.RestartCount
			if wst := cs.State.Waiting; wst != nil {
				switch wst.Reason {
				case "CrashLoopBackOff":
					crashLoop++
					note("CrashLoopBackOff")
				case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
					pullProblems++
					note(wst.Reason)
				case "CreateContainerConfigError", "CreateContainerError":
					pullProblems++
					note(wst.Reason)
				case "ContainerCreating", "PodInitializing":
					note("starting")
				}
			}
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" && now.Sub(t.FinishedAt.Time) < restartWindow {
				oom++
			}
			if cs.State.Running != nil && !cs.Ready {
				note("readiness probe failing")
			}
		}
		tr := w.restarts[p.UID]
		if tr == nil {
			tr = &restartTrack{count: restarts}
			w.restarts[p.UID] = tr
		}
		if restarts > tr.count {
			for i := tr.count; i < restarts; i++ {
				tr.events = append(tr.events, now)
			}
			tr.count = restarts
		}
		tr.seen = now
		kept := tr.events[:0]
		for _, t := range tr.events {
			if now.Sub(t) < restartWindow {
				kept = append(kept, t)
			}
		}
		tr.events = kept
		recentRestarts += len(tr.events)
	}
	w.mu.Unlock()

	window := fmtWindow(restartWindow)
	tolerated := cfg.RestartsTolerated
	stability = model.Signal{Kind: model.SignalStability, Name: "Stability", Status: model.StatusOperational, Detail: "no restarts in " + window}
	switch {
	case crashLoop > 0:
		// A crash loop is never tolerated: the pod is not coming back.
		stability.Status = model.StatusDegraded
		if running > 0 && crashLoop >= running {
			stability.Status = model.StatusOutage
		}
		stability.Detail = fmt.Sprintf("%s in CrashLoopBackOff", plural(crashLoop, "pod"))
	case oom > tolerated:
		stability.Status = model.StatusDegraded
		stability.Detail = fmt.Sprintf("OOM killed %s in %s", plural(oom, "time"), window)
	case recentRestarts > tolerated:
		stability.Status = model.StatusDegraded
		stability.Detail = fmt.Sprintf("%s in %s", plural(recentRestarts, "restart"), window)
	case recentRestarts > 0:
		stability.Detail = fmt.Sprintf("%s in %s, up to %d tolerated", plural(recentRestarts, "restart"), window, tolerated)
	}

	scheduling = model.Signal{Kind: model.SignalScheduling, Name: "Scheduling", Status: model.StatusOperational, Detail: "all pods scheduled"}
	switch {
	case unschedulable > 0:
		scheduling.Status = model.StatusDegraded
		scheduling.Detail = fmt.Sprintf("%s unschedulable", plural(unschedulable, "pod"))
		if schedMsg != "" {
			// The scheduler's message can name nodes, taints and labels.
			scheduling.PublicDetail = scheduling.Detail
			scheduling.Detail += ": " + schedMsg
		}
	case pullProblems > 0:
		scheduling.Status = model.StatusDegraded
		scheduling.Detail = fmt.Sprintf("%s cannot start: %s", plural(pullProblems, "pod"), strings.Join(reasons, ", "))
	}
	if running == 0 && len(pods) == 0 {
		scheduling.Detail = "no pods"
	}
	return stability, scheduling, strings.Join(reasons, ", ")
}

// fmtWindow writes a restart window the way the page reads: 15 min, 2 h,
// 1 h 30 min.
func fmtWindow(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	default:
		return fmt.Sprintf("%d h %d min", h, m)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (w *Watcher) pruneRestarts(seen map[types.UID]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for uid := range w.restarts {
		if !seen[uid] {
			delete(w.restarts, uid)
		}
	}
}

// matchServices returns the Services in ns whose selector matches the
// workload's pod template labels.
func matchServices(services []*corev1.Service, ns string, podLabels map[string]string) []*corev1.Service {
	var out []*corev1.Service
	for _, s := range services {
		if s.Namespace != ns || len(s.Spec.Selector) == 0 {
			continue
		}
		if labels.SelectorFromSet(s.Spec.Selector).Matches(labels.Set(podLabels)) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// endpointSignal reports how many ready endpoints each matching Service
// has. Pods being Ready while a Service has none is the case it exists for
// (an outage); fewer than min ready endpoints is degraded.
func endpointSignal(services []*corev1.Service, slices map[string][]*discoveryv1.EndpointSlice, c *model.Component, min int) model.Signal {
	sig := model.Signal{Kind: model.SignalEndpoints, Name: "Endpoints", Status: model.StatusOperational}
	var parts, counts []string
	for _, s := range services {
		ready := 0
		for _, es := range slices[s.Namespace+"/"+s.Name] {
			for _, e := range es.Endpoints {
				if e.Conditions.Ready == nil || *e.Conditions.Ready {
					ready++
				}
			}
		}
		parts = append(parts, fmt.Sprintf("%s %d ready", s.Name, ready))
		counts = append(counts, fmt.Sprintf("%d ready", ready))
		switch {
		case ready == 0 && c.Ready > 0:
			sig.Status = model.StatusOutage
			sig.Detail = fmt.Sprintf("%s has no ready endpoints although pods are ready", s.Name)
			sig.PublicDetail = "a Service has no ready endpoints although pods are ready"
		case ready > 0 && ready < min && sig.Status < model.StatusDegraded:
			sig.Status = model.StatusDegraded
			sig.Detail = fmt.Sprintf("%s has %d ready, needs %d", s.Name, ready, min)
			sig.PublicDetail = fmt.Sprintf("a Service has %d ready, needs %d", ready, min)
		}
	}
	if sig.Detail == "" {
		sig.Detail = strings.Join(parts, ", ")
		sig.PublicDetail = strings.Join(counts, ", ")
	}
	return sig
}

func ingressUses(ing *networkingv1.Ingress, services map[string]bool) bool {
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil && services[b.Service.Name] {
		return true
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service != nil && services[p.Backend.Service.Name] {
				return true
			}
		}
	}
	return false
}

func ingressSignal(ing *networkingv1.Ingress) model.Signal {
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	sig := model.Signal{Kind: model.SignalIngress, Name: "Ingress " + ing.Name, PublicName: "Ingress", Status: model.StatusOperational, Detail: strings.Join(hosts, ", ")}
	if len(ing.Status.LoadBalancer.Ingress) == 0 {
		sig.Status = model.StatusDegraded
		sig.Detail = "no load balancer address"
	}
	return sig
}

// routeSignal reads an OpenShift Route. ok is false if the Route does not
// point at one of the component's Services.
func routeSignal(r *unstructured.Unstructured, services map[string]bool) (sig model.Signal, host string, tls bool, ok bool) {
	kind, _, _ := unstructured.NestedString(r.Object, "spec", "to", "kind")
	name, _, _ := unstructured.NestedString(r.Object, "spec", "to", "name")
	if (kind != "" && kind != "Service") || !services[name] {
		return sig, "", false, false
	}
	host, _, _ = unstructured.NestedString(r.Object, "spec", "host")
	_, tls, _ = unstructured.NestedMap(r.Object, "spec", "tls")
	sig = model.Signal{Kind: model.SignalIngress, Name: "Route " + r.GetName(), PublicName: "Route", Status: model.StatusOperational, Detail: host}
	admitted := false
	ingresses, _, _ := unstructured.NestedSlice(r.Object, "status", "ingress")
	for _, i := range ingresses {
		im, _ := i.(map[string]interface{})
		conds, _, _ := unstructured.NestedSlice(im, "conditions")
		for _, cnd := range conds {
			cm, _ := cnd.(map[string]interface{})
			if cm["type"] == "Admitted" {
				if cm["status"] == "True" {
					admitted = true
				} else if msg, _ := cm["message"].(string); msg != "" {
					sig.Detail = msg
				}
			}
		}
	}
	if !admitted {
		sig.Status = model.StatusOutage
		if sig.Detail == host || sig.Detail == "" {
			sig.Detail = "not admitted by the router"
		}
		// The router's own message can name other namespaces and Routes.
		sig.PublicDetail = "not admitted by the router"
	}
	return sig, host, tls, true
}
