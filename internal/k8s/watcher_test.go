package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/DanielHouston/sutats/internal/model"
)

func deployment(ns, name string, ann map[string]string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(desired)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, UpdatedReplicas: ready},
	}
}

func TestComponentsFromAnnotatedWorkloads(t *testing.T) {
	client := fake.NewSimpleClientset(
		deployment("prod", "api", map[string]string{AnnComponent: "API", AnnGroup: "Edge", AnnOrder: "20"}, 3, 3),
		deployment("prod", "auth", map[string]string{AnnComponent: "Auth", AnnGroup: "Platform", AnnOrder: "10"}, 2, 1),
		deployment("prod", "unlisted", nil, 1, 1),
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg", Annotations: map[string]string{AnnComponent: "PostgreSQL", AnnOrder: "30"}},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 0},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ingress", Name: "nginx", Annotations: map[string]string{AnnComponent: "Ingress", AnnMaintenance: "true", AnnOrder: "40"}},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 4, NumberReady: 4},
		},
	)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !w.Synced() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.Synced() {
		t.Fatal("informers never synced")
	}

	comps, err := w.Components()
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name   string
		status model.Status
	}{
		{"Auth", model.StatusDegraded},
		{"API", model.StatusOperational},
		{"PostgreSQL", model.StatusOutage},
		{"Ingress", model.StatusMaintenance},
	}
	if len(comps) != len(want) {
		t.Fatalf("got %d components: %+v", len(comps), comps)
	}
	for i, c := range comps {
		if c.Name != want[i].name || c.Status != want[i].status {
			t.Errorf("[%d] = %s/%v, want %s/%v (reason %q)", i, c.Name, c.Status, want[i].name, want[i].status, c.Reason)
		}
	}
}

func TestAllWorkloadsMode(t *testing.T) {
	client := fake.NewSimpleClientset(
		deployment("prod", "unlisted", nil, 1, 1),
		deployment("prod", "hidden", map[string]string{AnnHide: "true"}, 1, 1),
	)
	w := NewWithClient(client, Options{AllWorkloads: true, Namespaces: []string{"prod"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	comps, _ := w.Components()
	if len(comps) != 1 || comps[0].Name != "prod/unlisted" || comps[0].Group != "prod" {
		t.Fatalf("comps = %+v", comps)
	}
}

func TestProbeAnnotationsBecomeProbesAndSignals(t *testing.T) {
	client := fake.NewSimpleClientset(
		deployment("prod", "api", map[string]string{
			AnnComponent:              "API",
			"sutats.io/probe":         "https://api.example.com/healthz",
			"sutats.io/probe.graphql": "https://api.example.com/graphql",
		}, 2, 2),
		deployment("prod", "bad", map[string]string{
			AnnComponent:       "Bad",
			"sutats.io/probes": "- url: notaurl",
		}, 1, 1),
	)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	comps, _ := w.Components()
	if len(comps) != 2 {
		t.Fatalf("comps = %d", len(comps))
	}
	api, bad := comps[0], comps[1]
	if len(api.Probes) != 2 || api.Probes[0].Name != "HTTP" || api.Probes[1].Name != "Graphql" {
		t.Fatalf("api probes = %+v", api.Probes)
	}
	if api.Signals[0].Kind != model.SignalPods || api.Signals[0].Detail != "2 of 2 ready" {
		t.Fatalf("api signals before probing = %+v", api.Signals)
	}
	for _, s := range api.Signals {
		if s.Kind == model.SignalHTTP {
			t.Fatalf("http signals only appear once the prober has run: %+v", api.Signals)
		}
	}
	if api.Status != model.StatusOperational {
		t.Fatalf("api status = %v", api.Status)
	}
	// A broken annotation is surfaced as a config signal but does not alarm.
	var cfg *model.Signal
	for i := range bad.Signals {
		if bad.Signals[i].Kind == model.SignalConfig {
			cfg = &bad.Signals[i]
		}
	}
	if cfg == nil || cfg.Status != model.StatusUnknown {
		t.Fatalf("bad signals = %+v", bad.Signals)
	}
	if bad.Status != model.StatusOperational {
		t.Fatalf("bad status = %v, config errors must not alarm", bad.Status)
	}
}

func TestPagesFromNamespaces(t *testing.T) {
	ns := func(name string, labels, ann map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: ann}}
	}
	client := fake.NewSimpleClientset(
		ns("pay", map[string]string{LabelPage: "payments"}, map[string]string{AnnPageTitle: "Payments", AnnPageHost: "Status.Pay.Example.com"}),
		ns("shared", map[string]string{LabelPage: "platform"}, map[string]string{AnnNSPages: "payments, Bad_Slug"}),
		ns("plain", nil, nil),
		deployment("pay", "checkout", map[string]string{AnnComponent: "Checkout"}, 1, 1),
		deployment("shared", "db", map[string]string{AnnComponent: "DB"}, 1, 1),
		deployment("plain", "svc", map[string]string{AnnComponent: "Svc", AnnPages: "ops"}, 1, 1),
	)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}

	pages := w.Pages()
	got := map[string]model.Page{}
	for _, p := range pages {
		got[p.Slug] = p
	}
	if len(pages) != 3 {
		t.Fatalf("pages = %+v", pages)
	}
	if p := got["payments"]; p.Title != "Payments" || p.Host != "status.pay.example.com" || len(p.Namespaces) != 2 {
		t.Errorf("payments = %+v", p)
	}
	if p := got["platform"]; p.Title != "Platform" || p.Host != "" || len(p.Namespaces) != 1 {
		t.Errorf("platform = %+v", p)
	}
	if p := got["ops"]; p.Title != "Ops" || len(p.Namespaces) != 0 {
		t.Errorf("ops (workload-only page) = %+v", p)
	}

	comps, _ := w.Components()
	byName := map[string]model.Component{}
	for _, c := range comps {
		byName[c.Name] = c
	}
	if c := byName["Checkout"]; len(c.Pages) != 1 || c.Pages[0] != "payments" || c.ID != "deployment/pay/checkout" {
		t.Errorf("Checkout = pages %v id %q", c.Pages, c.ID)
	}
	if c := byName["DB"]; len(c.Pages) != 2 || c.Pages[0] != "platform" || c.Pages[1] != "payments" {
		t.Errorf("DB pages = %v", c.Pages)
	}
	if c := byName["Svc"]; len(c.Pages) != 1 || c.Pages[0] != "ops" {
		t.Errorf("Svc pages = %v", c.Pages)
	}
	if !byName["DB"].OnPage(got["payments"]) || byName["Checkout"].OnPage(got["platform"]) || !byName["Checkout"].OnPage(model.Page{}) {
		t.Error("OnPage membership is wrong")
	}
}

func TestDerive(t *testing.T) {
	cases := []struct {
		desired, ready int32
		stuck          bool
		want           model.Status
	}{
		{3, 3, false, model.StatusOperational},
		{3, 2, false, model.StatusDegraded},
		{3, 0, false, model.StatusOutage},
		{0, 0, false, model.StatusMaintenance},
		{3, 3, true, model.StatusDegraded},
	}
	for _, c := range cases {
		got, _ := derive(model.Component{Desired: c.desired, Ready: c.ready}, nil, c.stuck, 1, 1)
		if got != c.want {
			t.Errorf("derive(%d/%d stuck=%v) = %v, want %v", c.ready, c.desired, c.stuck, got, c.want)
		}
	}
}
