package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/DanielHouston/sutats/internal/model"
)

func pod(ns, name string, owner metav1.OwnerReference, ready bool, cs ...corev1.ContainerStatus) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("pod-" + name), Labels: map[string]string{"app": "api"}, OwnerReferences: []metav1.OwnerReference{owner}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: cs},
	}
	if len(cs) == 0 {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", Ready: ready, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	}
	return p
}

func signal(c model.Component, kind string) *model.Signal {
	for i := range c.Signals {
		if c.Signals[i].Kind == kind {
			return &c.Signals[i]
		}
	}
	return nil
}

func TestSurroundingsFromPodsServicesIngressAndRoute(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api", UID: "dep-api", Annotations: map[string]string{AnnComponent: "API"}},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(2)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", "tier": "web"}},
		}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-1", UID: "rs-api", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "dep-api"}}}}
	rsOwner := metav1.OwnerReference{Kind: "ReplicaSet", Name: "api-1", UID: "rs-api"}
	healthy := pod("prod", "api-1-a", rsOwner, true)
	crashing := pod("prod", "api-1-b", rsOwner, false, corev1.ContainerStatus{
		Name: "c", Ready: false, RestartCount: 5,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}},
	})
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}}}
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "unrelated"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "db"}}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
			{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}},
		},
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"api.example.com"}}},
			Rules: []networkingv1.IngressRule{{Host: "api.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api"}}}},
			}}}},
		},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "1.2.3.4"}}}},
	}
	route := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "route.openshift.io/v1", "kind": "Route",
		"metadata": map[string]interface{}{"namespace": "prod", "name": "api-public"},
		"spec": map[string]interface{}{
			"host": "api.apps.example.com",
			"to":   map[string]interface{}{"kind": "Service", "name": "api"},
			"tls":  map[string]interface{}{"termination": "edge"},
		},
		"status": map[string]interface{}{"ingress": []interface{}{map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "False", "message": "host already claimed"}},
		}}},
	}}

	client := fake.NewSimpleClientset(dep, rs, healthy, crashing, svc, other, slice, ing)
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{routeGVR: "RouteList"}, route)
	w := NewWithClient(client, Options{Dynamic: dyn, Routes: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}

	comps, err := w.Components()
	if err != nil || len(comps) != 1 {
		t.Fatalf("comps = %d, %v", len(comps), err)
	}
	c := comps[0]

	if s := signal(c, model.SignalPods); s == nil || s.Detail != "1 of 2 replicas ready · CrashLoopBackOff" {
		t.Errorf("pods = %+v", s)
	}
	if s := signal(c, model.SignalStability); s == nil || s.Status != model.StatusDegraded || s.Detail != "1 pod in CrashLoopBackOff" {
		t.Errorf("stability = %+v", s)
	}
	if s := signal(c, model.SignalScheduling); s == nil || s.Status != model.StatusOperational {
		t.Errorf("scheduling = %+v", s)
	}
	if s := signal(c, model.SignalEndpoints); s == nil || s.Status != model.StatusOperational || s.Detail != "api 1 ready" {
		t.Errorf("endpoints = %+v (unrelated service must be ignored)", s)
	}
	var ingress, routeSig *model.Signal
	for i := range c.Signals {
		switch c.Signals[i].Name {
		case "Ingress api":
			ingress = &c.Signals[i]
		case "Route api-public":
			routeSig = &c.Signals[i]
		}
	}
	if ingress == nil || ingress.Status != model.StatusOperational || ingress.Detail != "api.example.com" {
		t.Errorf("ingress = %+v", ingress)
	}
	if routeSig == nil || routeSig.Status != model.StatusOutage || routeSig.Detail != "host already claimed" {
		t.Errorf("route = %+v", routeSig)
	}
	// Public pages get wording without object names or router messages.
	if ingress.PublicName != "Ingress" || routeSig.PublicName != "Route" || routeSig.PublicDetail != "not admitted by the router" {
		t.Errorf("public ingress %q, route %q %q", ingress.PublicName, routeSig.PublicName, routeSig.PublicDetail)
	}
	if s := signal(c, model.SignalEndpoints); s.PublicDetail != "1 ready" {
		t.Errorf("public endpoints detail = %q", s.PublicDetail)
	}
	if len(c.TLSHosts) != 2 || c.TLSHosts[0] != "api.example.com" || c.TLSHosts[1] != "api.apps.example.com" {
		t.Errorf("tls hosts = %v", c.TLSHosts)
	}
	// The worst signal wins: the route is an outage.
	if c.Status != model.StatusOutage {
		t.Errorf("component status = %v", c.Status)
	}
}

func TestEndpointMismatchAndSchedulingProblems(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web", UID: "dep-web", Annotations: map[string]string{AnnComponent: "Web"}},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(2)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web-1", UID: "rs-web", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "dep-web"}}}}
	owner := metav1.OwnerReference{Kind: "ReplicaSet", Name: "web-1", UID: "rs-web"}
	ready := pod("prod", "web-1-a", owner, true)
	pulling := pod("prod", "web-1-b", owner, false, corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}})
	pulling.Status.Phase = corev1.PodPending
	pending := pod("prod", "web-1-c", owner, false)
	pending.Status.Phase = corev1.PodPending
	pending.Status.ContainerStatuses = nil
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes are available: insufficient memory"}}
	// The Service selects a label the pods do not carry: no endpoints.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}}}
	emptySlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web-x", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}}}

	client := fake.NewSimpleClientset(dep, rs, ready, pulling, pending, svc, emptySlice)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	comps, _ := w.Components()
	c := comps[0]
	if s := signal(c, model.SignalEndpoints); s == nil || s.Status != model.StatusOutage || s.Detail != "web has no ready endpoints although pods are ready" || s.PublicDetail != "a Service has no ready endpoints although pods are ready" {
		t.Errorf("endpoints = %+v", s)
	}
	if s := signal(c, model.SignalScheduling); s == nil || s.Status != model.StatusDegraded || s.Detail != "1 pod unschedulable: 0/1 nodes are available: insufficient memory" || s.PublicDetail != "1 pod unschedulable" {
		t.Errorf("scheduling = %+v", s)
	}
	if s := signal(c, model.SignalPods); s == nil || s.Detail != "1 of 2 replicas ready · ImagePullBackOff" {
		t.Errorf("pods = %+v", s)
	}
	if s := signal(c, model.SignalStability); s == nil || s.Status != model.StatusOperational {
		t.Errorf("stability = %+v", s)
	}
	if len(signalsOfKind(c, model.SignalIngress)) != 0 {
		t.Error("no ingress expected")
	}
}

func TestRestartsAreDatedAndForgotten(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "w", UID: "dep-w", Annotations: map[string]string{AnnComponent: "W"}},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "w"}}}},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "w-1", UID: "rs-w", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "w", UID: "dep-w"}}}}
	owner := metav1.OwnerReference{Kind: "ReplicaSet", Name: "w-1", UID: "rs-w"}
	p := pod("prod", "w-1-a", owner, true, corev1.ContainerStatus{Name: "c", Ready: true, RestartCount: 3, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	client := fake.NewSimpleClientset(dep, rs, p)
	w := NewWithClient(client, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	for !w.Synced() {
		time.Sleep(10 * time.Millisecond)
	}
	// First sight: a restart count is a baseline, not a recent event.
	comps, _ := w.Components()
	if s := signal(comps[0], model.SignalStability); s.Status != model.StatusOperational {
		t.Fatalf("baseline restarts should not alarm: %+v", s)
	}
	// Count goes up: that is a recent restart.
	w.mu.Lock()
	w.restarts[p.UID].count = 1
	w.mu.Unlock()
	comps, _ = w.Components()
	if s := signal(comps[0], model.SignalStability); s.Status != model.StatusDegraded || s.Detail != "2 restarts in 15 min" {
		t.Fatalf("restart increment = %+v", s)
	}
	// Old events age out.
	w.mu.Lock()
	for i := range w.restarts[p.UID].events {
		w.restarts[p.UID].events[i] = time.Now().Add(-model.DefaultRestartWindow - time.Minute)
	}
	w.mu.Unlock()
	comps, _ = w.Components()
	if s := signal(comps[0], model.SignalStability); s.Status != model.StatusOperational {
		t.Fatalf("aged restarts should not alarm: %+v", s)
	}
}

func signalsOfKind(c model.Component, kind string) []model.Signal {
	var out []model.Signal
	for _, s := range c.Signals {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}
