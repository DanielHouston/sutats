package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

func TestParseAnnotationsForms(t *testing.T) {
	ann := map[string]string{
		AnnProbe:                   "https://api.example.com/healthz",
		AnnProbePrefix + "graphql": "https://api.example.com/graphql",
		AnnProbePrefix + "docs":    "https://docs.example.com/",
		AnnProbes: `
- name: Internal
  url: http://api.prod.svc:8080/readyz
  method: post
  timeout: 1s
  degraded_after: off
  expect_status: 204
  expect_body: ok
  headers:
    X-Token: abc
`,
	}
	specs, err := ParseAnnotations(ann)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	if got := strings.Join(names, ","); got != "HTTP,Docs,Graphql,Internal" {
		t.Fatalf("order/names = %s", got)
	}
	in := specs[3]
	if in.Method != "POST" || in.Timeout != time.Second || in.DegradedAfter != 0 || in.ExpectStatus != 204 || in.ExpectBody != "ok" || in.Headers["X-Token"] != "abc" {
		t.Fatalf("internal spec = %+v", in)
	}
	if specs[0].Timeout != DefaultTimeout || specs[0].DegradedAfter != DefaultDegradedAfter {
		t.Fatalf("defaults not applied: %+v", specs[0])
	}
}

func TestParseAnnotationsErrors(t *testing.T) {
	specs, err := ParseAnnotations(map[string]string{
		AnnProbe:                "ftp://nope",
		AnnProbePrefix + "good": "http://ok.example/",
		AnnProbes:               "- url: http://also.ok/\n- url: http://bad/\n  timeout: soon",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(specs) != 2 {
		t.Fatalf("valid entries should survive, got %d: %+v", len(specs), specs)
	}
	if _, err := ParseAnnotations(map[string]string{AnnProbes: "not: [yaml"}); err == nil {
		t.Fatal("expected yaml error")
	}
	if specs, _ := ParseAnnotations(nil); len(specs) != 0 {
		t.Fatal("no annotations should mean no probes")
	}
}

func TestParseAnnotationsDuplicateNames(t *testing.T) {
	specs, _ := ParseAnnotations(map[string]string{
		AnnProbePrefix + "api": "http://a/",
		AnnProbes:              "- name: Api\n  url: http://b/",
	})
	if specs[0].Name != "Api" || specs[1].Name != "Api 2" {
		t.Fatalf("names = %q, %q", specs[0].Name, specs[1].Name)
	}
}

// runOnce probes a single component with one spec and returns its HTTP signal.
func runOnce(t *testing.T, p *Prober, spec model.ProbeSpec) model.Signal {
	t.Helper()
	comps := []model.Component{{Name: "C", Probes: []model.ProbeSpec{spec}, Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational}}}}
	p.Run(context.Background(), comps)
	p.Attach(comps)
	for _, s := range comps[0].Signals {
		if s.Kind == model.SignalHTTP {
			return s
		}
	}
	t.Fatal("no http signal attached")
	return model.Signal{}
}

func TestProbeSuccessAndHysteresis(t *testing.T) {
	var code atomic.Int32
	code.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "sutats-probe/1" {
			t.Errorf("user agent = %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(int(code.Load()))
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	p := New(4)
	spec := model.ProbeSpec{Name: "Health", URL: srv.URL, Method: "GET", Timeout: time.Second, DegradedAfter: time.Second, ExpectBody: `"ok"`}

	if s := runOnce(t, p, spec); s.Status != model.StatusOperational || !strings.HasPrefix(s.Detail, "200 · ") {
		t.Fatalf("healthy signal = %+v", s)
	}

	code.Store(503)
	for i := 1; i < FailuresToOutage; i++ {
		s := runOnce(t, p, spec)
		if s.Status != model.StatusOperational {
			t.Fatalf("failure %d should keep previous status, got %+v", i, s)
		}
		if !strings.Contains(s.Detail, "503") {
			t.Fatalf("detail should mention the failure: %+v", s)
		}
	}
	if s := runOnce(t, p, spec); s.Status != model.StatusOutage || s.Detail != "503" {
		t.Fatalf("after %d failures = %+v", FailuresToOutage, s)
	}

	code.Store(200)
	if s := runOnce(t, p, spec); s.Status != model.StatusOperational {
		t.Fatalf("recovery should be immediate, got %+v", s)
	}
}

func TestProbeNeverSucceededIsNotHidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	p := New(1)
	spec := model.ProbeSpec{Name: "H", URL: srv.URL, Method: "GET", Timeout: time.Second}
	s := runOnce(t, p, spec)
	if s.Status != model.StatusUnknown {
		t.Fatalf("first failure with no prior success should be unknown, got %v", s.Status)
	}
	runOnce(t, p, spec)
	if s := runOnce(t, p, spec); s.Status != model.StatusOutage {
		t.Fatalf("third failure = %+v", s)
	}
}

func TestProbeSlowAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := New(1)

	slow := model.ProbeSpec{Name: "Slow", URL: srv.URL, Method: "GET", Timeout: time.Second, DegradedAfter: 10 * time.Millisecond}
	if s := runOnce(t, p, slow); s.Status != model.StatusDegraded || !strings.HasSuffix(s.Detail, "slow") || s.LatencyMS < 50 {
		t.Fatalf("slow signal = %+v", s)
	}

	short := model.ProbeSpec{Name: "Short", URL: srv.URL, Method: "GET", Timeout: 10 * time.Millisecond}
	for i := 0; i < FailuresToOutage; i++ {
		runOnce(t, p, short)
	}
	if s := runOnce(t, p, short); s.Status != model.StatusOutage || !strings.HasPrefix(s.Detail, "timeout after") {
		t.Fatalf("timeout signal = %+v", s)
	}
}

func TestProbeExpectStatusAndConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(302) }))
	url := srv.URL
	p := New(1)

	exp := model.ProbeSpec{Name: "E", URL: url, Method: "GET", Timeout: time.Second, ExpectStatus: 200}
	s := runOnce(t, p, exp)
	if s.Status != model.StatusUnknown || s.Detail == "" || !strings.Contains(s.Detail, "expected 200") {
		t.Fatalf("expect_status signal = %+v", s)
	}

	srv.Close()
	refused := model.ProbeSpec{Name: "R", URL: url, Method: "GET", Timeout: time.Second}
	for i := 0; i < FailuresToOutage; i++ {
		s = runOnce(t, p, refused)
	}
	if s.Status != model.StatusOutage || s.Detail != "connection refused" {
		t.Fatalf("refused signal = %+v", s)
	}
	if strings.Contains(s.Detail, url) {
		t.Fatal("detail must not leak the URL")
	}
}

func TestAttachRecomputesAndIsIdempotent(t *testing.T) {
	p := New(1)
	c := model.Component{
		Name:    "C",
		Probes:  []model.ProbeSpec{{Name: "H", URL: "http://127.0.0.1:9/", Method: "GET", Timeout: time.Second}},
		Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusDegraded, Detail: "1 of 2 replicas ready"}},
	}
	comps := []model.Component{c}
	p.Attach(comps)
	p.Attach(comps)
	if n := len(comps[0].Signals); n != 2 {
		t.Fatalf("signals = %d, want 2 (pods + http)", n)
	}
	if comps[0].Observed != model.StatusDegraded || comps[0].Reason != "1 of 2 replicas ready" {
		t.Fatalf("component = %v %q", comps[0].Observed, comps[0].Reason)
	}
	if comps[0].Signals[1].Detail != "waiting for first check" {
		t.Fatalf("unprobed signal = %+v", comps[0].Signals[1])
	}
}

func TestExportImport(t *testing.T) {
	p := New(1)
	spec := model.ProbeSpec{Name: "H", URL: "http://127.0.0.1:9/", Method: "GET", Timeout: time.Second}
	p.update("C", spec, result{ok: true, latency: 84 * time.Millisecond, detail: "200 · 84 ms"})
	exported := p.Export()
	if st, ok := exported["C::H"]; !ok || st.Status != model.StatusOperational || st.LatencyMS != 84 {
		t.Fatalf("export = %+v", exported)
	}
	follower := New(1)
	follower.Import(exported)
	comps := []model.Component{{Name: "C", Probes: []model.ProbeSpec{spec}, Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational}}}}
	follower.Attach(comps)
	if sig := comps[0].Signals[1]; sig.Detail != "200 · 84 ms" || sig.Status != model.StatusOperational {
		t.Fatalf("follower signal = %+v", sig)
	}
}

// tlsServer starts a TLS listener presenting a self-signed certificate
// that expires at notAfter and returns its host:port.
func tlsServer(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _ = c.(*tls.Conn).Handshake(); c.Close() }(c)
		}
	}()
	return ln.Addr().String()
}

func TestTLSExpiry(t *testing.T) {
	p := New(2)
	cases := []struct {
		name     string
		notAfter time.Time
		warn     time.Duration // 0 is the default
		want     model.Status
		detail   string
	}{
		{"healthy", time.Now().Add(90 * 24 * time.Hour), 0, model.StatusOperational, "certificate expires in 89 days"},
		{"soon", time.Now().Add(3 * 24 * time.Hour), 0, model.StatusDegraded, "certificate expires in 2 days"},
		{"expired", time.Now().Add(-30 * time.Hour), 0, model.StatusOutage, "certificate expired 30 h ago"},
		{"outside the default warning", time.Now().Add(10 * 24 * time.Hour), 0, model.StatusOperational, "certificate expires in 9 days"},
		{"inside a longer warning", time.Now().Add(10 * 24 * time.Hour), 14 * 24 * time.Hour, model.StatusDegraded, "certificate expires in 9 days"},
	}
	for _, c := range cases {
		addr := tlsServer(t, c.notAfter)
		comps := []model.Component{{Name: "C", TLSHosts: []string{addr}, Checks: model.Checks{TLSWarn: model.Duration(c.warn)}, Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational}}}}
		p.Run(context.Background(), comps)
		p.Attach(comps)
		var got *model.Signal
		for i := range comps[0].Signals {
			if comps[0].Signals[i].Kind == model.SignalTLS {
				got = &comps[0].Signals[i]
			}
		}
		if got == nil || got.Status != c.want || got.Detail != c.detail || got.Name != "TLS "+addr {
			t.Errorf("%s: signal = %+v, want %v %q", c.name, got, c.want, c.detail)
		}
	}
	// Unreachable hosts are unknown, never an alarm.
	comps := []model.Component{{Name: "C", TLSHosts: []string{"127.0.0.1:9"}, Signals: []model.Signal{{Kind: model.SignalPods, Name: "Pods", Status: model.StatusOperational}}}}
	p.Run(context.Background(), comps)
	p.Attach(comps)
	if s := comps[0].Signals[1]; s.Status != model.StatusUnknown || !strings.HasPrefix(s.Detail, "could not connect") || comps[0].Status != model.StatusOperational {
		t.Errorf("unreachable = %+v, component %v", s, comps[0].Status)
	}
}

func TestRunDropsStaleState(t *testing.T) {
	p := New(1)
	p.update("gone", model.ProbeSpec{Name: "x"}, result{ok: true})
	p.Run(context.Background(), nil)
	if len(p.states) != 0 {
		t.Fatal("state for removed probes should be dropped")
	}
}

// Error text never carries the probe's URL or the address it dialled.
func TestDescribeErrHidesAddresses(t *testing.T) {
	cases := map[string]string{
		`Get "http://api.prod.svc:8080/healthz": dial tcp 10.1.2.3:8080: connect: connection refused`:                    "connection refused",
		`Get "http://api.prod.svc:8080/healthz": dial tcp 10.1.2.3:8080: connect: no route to host`:                      "host unreachable",
		`Get "http://api.prod.svc:8080/healthz": dial tcp: lookup api.prod.svc: no such host`:                            "DNS lookup failed",
		`Get "http://api.prod.svc:8080/healthz": read tcp 10.0.0.9:51234->10.1.2.3:8080: read: connection reset by peer`: "connection reset",
		`Get "http://api.prod.svc:8080/healthz": dial tcp 10.1.2.3:8080: socket: too many open files`:                    "too many open files",
		`Get "http://api.prod.svc:8080/healthz": EOF`:                                                                    "EOF",
	}
	for in, want := range cases {
		got := describeErr(errors.New(in), time.Second)
		if got != want {
			t.Errorf("describeErr(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "10.") || strings.Contains(got, "svc") {
			t.Errorf("describeErr(%q) leaks an address: %q", in, got)
		}
	}
}
