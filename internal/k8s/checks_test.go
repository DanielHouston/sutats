package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/DanielHouston/sutats/internal/checks"
	"github.com/DanielHouston/sutats/internal/model"
)

// app is a Deployment with one ReplicaSet, its pods, a Service selecting
// them with a ready endpoint each, and an Ingress with TLS in front.
func app(ns, name string, ann map[string]string, pods ...*corev1.Pod) []runtime.Object {
	uid := types.UID("dep-" + name)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: uid, Annotations: ann},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(len(pods))), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
		}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: int32(len(pods))},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-1", UID: types.UID("rs-" + name), OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: name, UID: uid}}}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: name}}}
	for range pods {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{name + ".example.com"}}},
			Rules: []networkingv1.IngressRule{{Host: name + ".example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name}}}},
			}}}},
		},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "1.2.3.4"}}}},
	}
	out := []runtime.Object{dep, rs, svc, slice, ing}
	for _, p := range pods {
		p.Namespace = ns
		p.Labels = map[string]string{"app": name}
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-1", UID: types.UID("rs-" + name)}}
		out = append(out, p)
	}
	return out
}

func byName(comps []model.Component) map[string]model.Component {
	out := map[string]model.Component{}
	for _, c := range comps {
		out[c.Name] = c
	}
	return out
}

func kinds(c model.Component) string {
	var k []string
	for _, s := range c.Signals {
		k = append(k, s.Kind)
	}
	return strings.Join(k, ",")
}

func TestCheckConfigurationLayers(t *testing.T) {
	crashy := func(name string, restarts int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("pod-" + name)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "c", Ready: false, RestartCount: restarts,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}},
		}
	}
	healthy := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("pod-" + name)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "c", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}},
		}
	}
	// The namespace turns off TLS and makes stability info for everything
	// in it; one workload overrides that, and one has a typo.
	team := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", Annotations: map[string]string{
		"sutats.io/check.tls":       "off",
		"sutats.io/check.stability": "info",
		"sutats.io/check.nonsense":  "on",
	}}}
	objs := []runtime.Object{team}
	objs = append(objs, app("team", "worker", map[string]string{AnnComponent: "Worker"}, crashy("worker-a", 4))...)
	objs = append(objs, app("team", "web", map[string]string{
		AnnComponent:                "Web",
		"sutats.io/check.tls":       "warn=21d on",
		"sutats.io/check.stability": "outage",
		"sutats.io/check.ingress":   "off",
		"sutats.io/check.endpoints": "min=3",
		"sutats.io/probe":           "https://web.example.com/healthz",
	}, healthy("web-a"), healthy("web-b"))...)
	objs = append(objs, app("team", "typo", map[string]string{
		AnnComponent:                 "Typo",
		"sutats.io/check.scheduling": "of",
		"sutats.io/check.http":       "off",
		"sutats.io/probe":            "https://typo.example.com/",
	}, healthy("typo-a"))...)

	client := fake.NewSimpleClientset(objs...)
	// The flags make ingress problems only degrade, cluster-wide.
	flagLayer := checks.Layer{}
	e, err := checks.ParseValue("ingress", "degraded")
	if err != nil {
		t.Fatal(err)
	}
	flagLayer["ingress"] = e

	w := NewWithClient(client, Options{Checks: flagLayer})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	comps, err := w.Components()
	if err != nil {
		t.Fatal(err)
	}
	got := byName(comps)

	// Worker: crash looping, but the namespace says stability is info. The
	// row is there and failing; the component is fine. TLS is off.
	worker := got["Worker"]
	if s := signal(worker, model.SignalStability); s == nil || s.Status != model.StatusOutage || s.CountNote() != "not counted" {
		t.Errorf("worker stability = %+v", s)
	}
	if worker.Status != model.StatusOperational || len(worker.TLSHosts) != 0 || worker.Checks.Mode(model.SignalIngress) != model.ModeDegraded {
		t.Errorf("worker = %v tls %v ingress mode %v", worker.Status, worker.TLSHosts, worker.Checks.Mode(model.SignalIngress))
	}
	// The namespace typo shows on every component in it, prefixed.
	if s := signal(worker, model.SignalConfig); s == nil || !strings.HasPrefix(s.Detail, `namespace team: sutats.io/check.nonsense: unknown check "nonsense"`) {
		t.Errorf("worker config = %+v", s)
	}

	// Web: overrides the namespace on both counts, turns off the ingress row
	// but keeps the TLS host it found there, and needs three endpoints.
	web := got["Web"]
	if k := kinds(web); k != "pods,config,stability,scheduling,endpoints" {
		t.Errorf("web signal kinds = %s", k)
	}
	if web.Checks.Mode(model.SignalStability) != model.ModeOutage || web.Checks.Warn() != 21*24*time.Hour {
		t.Errorf("web checks = %+v", web.Checks)
	}
	if strings.Join(web.TLSHosts, ",") != "web.example.com" || len(web.Probes) != 1 {
		t.Errorf("web tls hosts = %v, probes %d", web.TLSHosts, len(web.Probes))
	}
	if s := signal(web, model.SignalEndpoints); s == nil || s.Status != model.StatusDegraded || s.Detail != "web has 2 ready, needs 3" {
		t.Errorf("web endpoints = %+v", s)
	}
	if web.Status != model.StatusDegraded {
		t.Errorf("web status = %v", web.Status)
	}

	// Typo: the bad mode is reported and the check stays on; http off drops
	// the probes so the prober never runs them.
	typo := got["Typo"]
	if !typo.Checks.Enabled(model.SignalScheduling) || signal(typo, model.SignalScheduling) == nil {
		t.Error("a typo must not turn a check off")
	}
	if len(typo.Probes) != 0 {
		t.Errorf("http off should drop probes, got %d", len(typo.Probes))
	}
	s := signal(typo, model.SignalConfig)
	if s == nil || s.Name != "Check config" || !strings.Contains(s.Detail, `sutats.io/check.scheduling: "of" is not a mode`) || typo.Status != model.StatusOperational {
		t.Errorf("typo config = %+v, status %v", s, typo.Status)
	}
}

func TestServicesAnnotationAndTolerance(t *testing.T) {
	restarting := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-a", UID: "pod-api-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "c", Ready: true, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	objs := app("prod", "api", map[string]string{
		AnnComponent:                "API",
		"sutats.io/check.stability": "tolerate=2",
		checks.AnnServices:          "api, api-admin",
	}, restarting)
	// A second Service that also selects the pods, which the annotation
	// leaves out, with its own Ingress that must not be picked up.
	objs = append(objs,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-headless"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}}},
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "internal"},
			Spec:       networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api-headless"}}},
		},
	)
	client := fake.NewSimpleClientset(objs...)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	comps, _ := w.Components()
	c := comps[0]
	if s := signal(c, model.SignalEndpoints); s == nil || s.Detail != "api 1 ready" {
		t.Errorf("endpoints = %+v (only the named Service should count)", s)
	}
	for _, s := range c.Signals {
		if s.Name == "Ingress internal" {
			t.Error("the Ingress in front of an unlisted Service must not count")
		}
	}
	if s := signal(c, model.SignalConfig); s == nil || s.Detail != "sutats.io/services: no Service named api-admin in namespace prod" {
		t.Errorf("config = %+v", s)
	}

	// Two restarts are within tolerance, three are not.
	w.mu.Lock()
	w.restarts[restarting.UID].count = -1
	w.mu.Unlock()
	comps, _ = w.Components()
	if s := signal(comps[0], model.SignalStability); s.Status != model.StatusOperational || s.Detail != "2 restarts in 15 min, up to 2 tolerated" {
		t.Errorf("tolerated = %+v", s)
	}
	w.mu.Lock()
	w.restarts[restarting.UID].count = 0
	w.mu.Unlock()
	comps, _ = w.Components()
	if s := signal(comps[0], model.SignalStability); s.Status != model.StatusDegraded || s.Detail != "3 restarts in 15 min" {
		t.Errorf("over tolerance = %+v", s)
	}
}
