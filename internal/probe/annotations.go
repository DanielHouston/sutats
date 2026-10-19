// Package probe runs HTTP health checks declared on workloads through
// annotations and turns the results into component signals.
package probe

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/DanielHouston/sutats/internal/model"
)

// Annotation keys.
const (
	AnnProbe       = "sutats.io/probe"  // single URL
	AnnProbePrefix = "sutats.io/probe." // sutats.io/probe.<name>: URL
	AnnProbes      = "sutats.io/probes" // YAML list with full settings
)

// Defaults applied when a probe does not set them.
const (
	DefaultTimeout       = 5 * time.Second
	DefaultDegradedAfter = 2 * time.Second
)

// entry is the YAML shape of one item in sutats.io/probes.
type entry struct {
	Name          string            `json:"name"`
	URL           string            `json:"url"`
	Method        string            `json:"method"`
	Headers       map[string]string `json:"headers"`
	ExpectStatus  int               `json:"expect_status"`
	ExpectBody    string            `json:"expect_body"`
	Timeout       string            `json:"timeout"`
	DegradedAfter string            `json:"degraded_after"`
	Insecure      bool              `json:"insecure"`
}

// ParseAnnotations reads every supported probe annotation. All forms may be
// combined; the result is ordered: the single probe, then suffixed ones by
// key, then the YAML list in its own order. The error, if any, describes the
// first invalid entry; valid entries are still returned.
func ParseAnnotations(ann map[string]string) ([]model.ProbeSpec, error) {
	var specs []model.ProbeSpec
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	if u := strings.TrimSpace(ann[AnnProbe]); u != "" {
		spec, err := build(entry{Name: "HTTP", URL: u})
		if err != nil {
			fail(fmt.Errorf("%s: %w", AnnProbe, err))
		} else {
			specs = append(specs, spec)
		}
	}

	var suffixed []string
	for k := range ann {
		if strings.HasPrefix(k, AnnProbePrefix) && len(k) > len(AnnProbePrefix) {
			suffixed = append(suffixed, k)
		}
	}
	sort.Strings(suffixed)
	for _, k := range suffixed {
		spec, err := build(entry{Name: humanise(strings.TrimPrefix(k, AnnProbePrefix)), URL: strings.TrimSpace(ann[k])})
		if err != nil {
			fail(fmt.Errorf("%s: %w", k, err))
			continue
		}
		specs = append(specs, spec)
	}

	if raw := strings.TrimSpace(ann[AnnProbes]); raw != "" {
		var entries []entry
		if err := yaml.Unmarshal([]byte(raw), &entries); err != nil {
			fail(fmt.Errorf("%s: %w", AnnProbes, err))
		} else {
			for i, e := range entries {
				spec, err := build(e)
				if err != nil {
					fail(fmt.Errorf("%s[%d]: %w", AnnProbes, i, err))
					continue
				}
				specs = append(specs, spec)
			}
		}
	}

	// Names must be unique within a component because they key the prober's
	// state and label the chips on the page.
	seen := map[string]int{}
	for i := range specs {
		n := specs[i].Name
		seen[n]++
		if seen[n] > 1 {
			specs[i].Name = fmt.Sprintf("%s %d", n, seen[n])
		}
	}
	return specs, firstErr
}

func build(e entry) (model.ProbeSpec, error) {
	spec := model.ProbeSpec{
		Name:          strings.TrimSpace(e.Name),
		URL:           strings.TrimSpace(e.URL),
		Method:        strings.ToUpper(strings.TrimSpace(e.Method)),
		Headers:       e.Headers,
		ExpectStatus:  e.ExpectStatus,
		ExpectBody:    e.ExpectBody,
		Insecure:      e.Insecure,
		Timeout:       DefaultTimeout,
		DegradedAfter: DefaultDegradedAfter,
	}
	if spec.URL == "" {
		return spec, fmt.Errorf("missing url")
	}
	u, err := url.Parse(spec.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return spec, fmt.Errorf("invalid url %q (must start with http:// or https://)", spec.URL)
	}
	if spec.Name == "" {
		spec.Name = u.Host
	}
	if spec.Method == "" {
		spec.Method = "GET"
	}
	if spec.ExpectStatus != 0 && (spec.ExpectStatus < 100 || spec.ExpectStatus > 599) {
		return spec, fmt.Errorf("expect_status %d is not an HTTP status", spec.ExpectStatus)
	}
	if e.Timeout != "" {
		d, err := parseDuration(e.Timeout)
		if err != nil {
			return spec, fmt.Errorf("timeout: %w", err)
		}
		if d <= 0 {
			return spec, fmt.Errorf("timeout must be positive")
		}
		spec.Timeout = d
	}
	if e.DegradedAfter != "" {
		d, err := parseDuration(e.DegradedAfter)
		if err != nil {
			return spec, fmt.Errorf("degraded_after: %w", err)
		}
		spec.DegradedAfter = d
	}
	return spec, nil
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	// YAML turns a bare "off" or "no" into false before we see it, so accept
	// that spelling as well as the words people actually write.
	if s == "off" || s == "false" || s == "no" || s == "0" || s == "none" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q (use values like 500ms, 2s, 1m, or off)", s)
	}
	return d, nil
}

// humanise turns an annotation suffix like "public-api" into "Public api".
func humanise(s string) string {
	s = strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "HTTP"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
