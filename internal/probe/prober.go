package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

// FailuresToOutage is how many consecutive failed checks it takes before a
// probe reports Outage. Until then the previous status is kept so a single
// blip does not change the page.
const FailuresToOutage = 3

// maxBody is how much of a response we read when checking expect_body.
const maxBody = 64 << 10

// result is the outcome of one check.
type result struct {
	at      time.Time
	ok      bool
	slow    bool
	latency time.Duration
	detail  string
}

type state struct {
	spec     model.ProbeSpec
	failures int
	last     *result
	status   model.Status
	detail   string
	latency  time.Duration
}

// Prober runs probes and remembers their latest state.
type Prober struct {
	client   *http.Client
	insecure *http.Client
	parallel int

	mu     sync.Mutex
	states map[string]*state
}

// New builds a Prober. parallel bounds how many checks run at once.
func New(parallel int) *Prober {
	if parallel <= 0 {
		parallel = 16
	}
	mk := func(insecure bool) *http.Client {
		t := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}
		if insecure {
			t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per probe
		}
		return &http.Client{Transport: t}
	}
	return &Prober{client: mk(false), insecure: mk(true), parallel: parallel, states: map[string]*state{}}
}

func key(component, probe string) string { return component + "::" + probe }

// State is the exported form of one probe's latest result, so follower
// replicas (which do not probe) can show what the leader saw.
type State struct {
	Status    model.Status `json:"status"`
	Detail    string       `json:"detail"`
	LatencyMS int64        `json:"latency_ms"`
	Failures  int          `json:"failures"`
}

// Export returns every probe's current state keyed by component and probe.
func (p *Prober) Export() map[string]State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]State, len(p.states))
	for k, st := range p.states {
		if st.last == nil {
			continue
		}
		out[k] = State{Status: st.status, Detail: st.detail, LatencyMS: st.latency.Milliseconds(), Failures: st.failures}
	}
	return out
}

// Import replaces the prober's state with an exported map. Used by followers.
func (p *Prober) Import(states map[string]State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = make(map[string]*state, len(states))
	for k, st := range states {
		p.states[k] = &state{
			status: st.Status, detail: st.Detail, latency: time.Duration(st.LatencyMS) * time.Millisecond,
			failures: st.Failures, last: &result{},
		}
	}
}

// tlsName is the signal name for a host's certificate check.
func tlsName(host string) string { return "TLS " + host }

// Run executes every probe on every component once, in parallel, and stores
// the results. It returns when all checks finish or ctx ends.
func (p *Prober) Run(ctx context.Context, comps []model.Component) {
	type job struct {
		component string
		spec      model.ProbeSpec
		tlsHost   string
		tlsWarn   time.Duration
	}
	var jobs []job
	live := map[string]bool{}
	for _, c := range comps {
		for _, spec := range c.Probes {
			jobs = append(jobs, job{component: c.Key(), spec: spec})
			live[key(c.Key(), spec.Name)] = true
		}
		for _, h := range c.TLSHosts {
			jobs = append(jobs, job{component: c.Key(), tlsHost: h, tlsWarn: c.Checks.Warn()})
			live[key(c.Key(), tlsName(h))] = true
		}
	}

	// Drop state for probes that no longer exist.
	p.mu.Lock()
	for k := range p.states {
		if !live[k] {
			delete(p.states, k)
		}
	}
	p.mu.Unlock()

	if len(jobs) == 0 {
		return
	}
	sem := make(chan struct{}, p.parallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			if j.tlsHost != "" {
				p.updateTLS(j.component, j.tlsHost, p.checkTLS(ctx, j.tlsHost, j.tlsWarn))
				return
			}
			r := p.check(ctx, j.spec)
			p.update(j.component, j.spec, r)
		}(j)
	}
	wg.Wait()
}

// tlsResult is the outcome of one certificate check.
type tlsResult struct {
	status model.Status
	detail string
}

// checkTLS connects to host:443, reads the leaf certificate and judges its
// expiry: Degraded within warn of it, Outage once expired. Verification is skipped on purpose: the question is when the
// certificate expires, not whether this process trusts the chain. A failed
// connection is Unknown, not an alarm; the HTTP probes cover reachability.
func (p *Prober) checkTLS(ctx context.Context, host string, warn time.Duration) tlsResult {
	addr := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = net.JoinHostPort(host, "443")
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}} //nolint:gosec // expiry check only
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return tlsResult{status: model.StatusUnknown, detail: "could not connect: " + describeErr(err, 5*time.Second)}
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return tlsResult{status: model.StatusUnknown, detail: "no certificate presented"}
	}
	left := time.Until(certs[0].NotAfter)
	switch {
	case left <= 0:
		return tlsResult{status: model.StatusOutage, detail: fmt.Sprintf("certificate expired %s ago", fmtAge(-left))}
	case left < warn:
		return tlsResult{status: model.StatusDegraded, detail: "certificate expires in " + fmtAge(left)}
	default:
		return tlsResult{status: model.StatusOperational, detail: "certificate expires in " + fmtAge(left)}
	}
}

func fmtAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func (p *Prober) updateTLS(component, host string, r tlsResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := key(component, tlsName(host))
	st := p.states[k]
	if st == nil {
		st = &state{}
		p.states[k] = st
	}
	st.last = &result{}
	st.status = r.status
	st.detail = r.detail
	st.latency = 0
	st.failures = 0
}

func (p *Prober) check(ctx context.Context, spec model.ProbeSpec) result {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL, nil)
	if err != nil {
		return result{at: start, detail: "bad request: " + err.Error()}
	}
	req.Header.Set("User-Agent", "sutats-probe/1")
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	client := p.client
	if spec.Insecure {
		client = p.insecure
	}
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return result{at: start, latency: latency, detail: describeErr(err, spec.Timeout)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	latency = time.Since(start)

	r := result{at: start, latency: latency}
	switch {
	case spec.ExpectStatus != 0 && resp.StatusCode != spec.ExpectStatus:
		r.detail = fmt.Sprintf("%d, expected %d", resp.StatusCode, spec.ExpectStatus)
	case spec.ExpectStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode > 299):
		r.detail = fmt.Sprintf("%d", resp.StatusCode)
	case spec.ExpectBody != "" && !strings.Contains(string(body), spec.ExpectBody):
		r.detail = fmt.Sprintf("%d, body did not contain expected text", resp.StatusCode)
	default:
		r.ok = true
		r.slow = spec.DegradedAfter > 0 && latency > spec.DegradedAfter
		r.detail = fmt.Sprintf("%d · %s", resp.StatusCode, fmtLatency(latency))
		if r.slow {
			r.detail += " slow"
		}
	}
	return r
}

func describeErr(err error, timeout time.Duration) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("timeout after %s", fmtLatency(timeout))
	}
	s := err.Error()
	// Trim the "Get "https://...": " prefix the http client adds, which would
	// otherwise leak the URL onto the page.
	if i := strings.LastIndex(s, "\": "); i >= 0 {
		s = s[i+3:]
	}
	switch {
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "no such host"):
		return "DNS lookup failed"
	case strings.Contains(s, "certificate"), strings.Contains(s, "tls:"), strings.Contains(s, "x509"):
		return "TLS error"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return "host unreachable"
	case strings.Contains(s, "connection reset"):
		return "connection reset"
	}
	// Network errors carry the address ("dial tcp 10.1.2.3:80: connect:
	// ..."), which may be internal. Keep only the last part, the reason.
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func fmtLatency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1 ms"
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
}

func (p *Prober) update(component string, spec model.ProbeSpec, r result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := key(component, spec.Name)
	st := p.states[k]
	if st == nil {
		st = &state{status: model.StatusUnknown}
		p.states[k] = st
	}
	st.spec = spec
	st.last = &r
	st.latency = r.latency
	switch {
	case r.ok && r.slow:
		st.failures = 0
		st.status = model.StatusDegraded
		st.detail = r.detail
	case r.ok:
		st.failures = 0
		st.status = model.StatusOperational
		st.detail = r.detail
	default:
		st.failures++
		if st.failures >= FailuresToOutage || st.status == model.StatusUnknown {
			// Either enough failures in a row, or no good result ever seen:
			// report it. A probe that has never succeeded is not "fine".
			if st.failures >= FailuresToOutage {
				st.status = model.StatusOutage
				st.detail = r.detail
			} else {
				st.status = model.StatusUnknown
				st.detail = fmt.Sprintf("%s (%d of %d)", r.detail, st.failures, FailuresToOutage)
			}
		} else {
			st.detail = fmt.Sprintf("%s (%d of %d)", r.detail, st.failures, FailuresToOutage)
		}
		slog.Debug("probe failed", "component", component, "probe", spec.Name, "failures", st.failures, "detail", r.detail)
	}
}

// Attach adds an HTTP signal per probe to each component from the stored
// state and recomputes the component's observed status. Components without
// probes are left untouched.
func (p *Prober) Attach(comps []model.Component) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range comps {
		c := &comps[i]
		if len(c.Probes) == 0 && len(c.TLSHosts) == 0 {
			continue
		}
		// Replace any previous probe signals so Attach is idempotent.
		kept := c.Signals[:0:0]
		for _, s := range c.Signals {
			if s.Kind != model.SignalHTTP && s.Kind != model.SignalTLS {
				kept = append(kept, s)
			}
		}
		c.Signals = kept
		for _, h := range c.TLSHosts {
			sig := model.Signal{Kind: model.SignalTLS, Name: tlsName(h), Status: model.StatusUnknown, Detail: "waiting for first check"}
			if st, ok := p.states[key(c.Key(), tlsName(h))]; ok && st.last != nil {
				sig.Status = st.status
				sig.Detail = st.detail
			}
			c.Signals = append(c.Signals, sig)
		}
		for _, spec := range c.Probes {
			sig := model.Signal{Kind: model.SignalHTTP, Name: spec.Name, Status: model.StatusUnknown, Detail: "waiting for first check"}
			if st, ok := p.states[key(c.Key(), spec.Name)]; ok && st.last != nil {
				sig.Status = st.status
				sig.Detail = st.detail
				sig.LatencyMS = st.latency.Milliseconds()
			}
			c.Signals = append(c.Signals, sig)
		}
		c.Recompute()
	}
}
