// Package checks reads the configuration of the checks sutats runs around
// each workload, and merges it from its three sources: command-line flags
// (the defaults), Namespace annotations and workload annotations. Later
// sources win, per check and per setting.
//
// Every check is configured with one value, the same in all three places:
//
//	sutats.io/check.stability: "info window=30m tolerate=2"
//	--check.stability="info window=30m tolerate=2"
//
// The value is a list of words separated by spaces: at most one mode (on,
// off, info, degraded, outage) and any number of key=value settings. List
// values are comma separated with no spaces: hosts=a.example.com,b.example.com.
//
// To add a setting, add a field to model.Checks, an entry to its check's
// settings in the catalogue below, and use it where the check is derived.
package checks

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

// Annotation keys.
const (
	// AnnPrefix starts the per-check annotation: sutats.io/check.<name>.
	AnnPrefix = "sutats.io/check."
	// AnnServices names the Services that belong to a workload, comma
	// separated. Only read on workloads.
	AnnServices = "sutats.io/services"
)

// setting is one key=value a check accepts. parse validates the value and
// returns a function that applies it, so values are checked once, where the
// mistake can be reported, and applied later during the merge.
type setting struct {
	key   string
	parse func(v string) (func(*model.Checks), error)
}

// spec describes one check.
type spec struct {
	name     string
	about    string // for flag help
	noOff    string // why the check cannot be turned off, if it cannot
	settings []setting
}

// catalogue is every configurable check in display order. The names are
// the signal kinds the checks produce.
var catalogue = []spec{
	{
		name:  model.SignalPods,
		about: "replicas ready",
		noOff: "it is what the component's status is built on; use info to show it without counting it",
	},
	{
		name:  model.SignalStability,
		about: "restarts, crash loops and OOM kills; settings window=<duration>, tolerate=<restarts>",
		settings: []setting{
			{"window", duration(time.Minute, 24*time.Hour, func(c *model.Checks, d time.Duration) { c.RestartWindow = model.Duration(d) })},
			{"tolerate", count(0, func(c *model.Checks, n int) { c.RestartsTolerated = n })},
		},
	},
	{name: model.SignalScheduling, about: "unschedulable pods and image pull failures"},
	{
		name:  model.SignalEndpoints,
		about: "ready endpoints behind the workload's Services; setting min=<endpoints>",
		settings: []setting{
			{"min", count(1, func(c *model.Checks, n int) { c.MinEndpoints = n })},
		},
	},
	{name: model.SignalIngress, about: "Ingresses and Routes in front of the workload"},
	{
		name:  model.SignalTLS,
		about: "certificate expiry on ingress hosts; settings warn=<duration>, hosts=<host,host>",
		settings: []setting{
			{"warn", duration(time.Hour, 365*24*time.Hour, func(c *model.Checks, d time.Duration) { c.TLSWarn = model.Duration(d) })},
			{"hosts", hosts(func(c *model.Checks, h []string) { c.TLSHosts = h })},
		},
	},
	{name: model.SignalHTTP, about: "the HTTP probes declared with sutats.io/probe annotations"},
}

func lookup(name string) (spec, bool) {
	for _, s := range catalogue {
		if s.name == name {
			return s, true
		}
	}
	return spec{}, false
}

// Names lists every check in display order.
func Names() []string {
	out := make([]string, len(catalogue))
	for i, s := range catalogue {
		out[i] = s.name
	}
	return out
}

// Entry is what one source says about one check: a mode (empty if it does
// not say) and the settings it sets.
type Entry struct {
	Mode model.Mode
	set  map[string]func(*model.Checks)
}

// Layer is one source of configuration, by check name.
type Layer map[string]Entry

// ParseValue reads one check's value, such as "info window=30m". Valid
// words are kept even when others are wrong, so one typo does not undo the
// rest; the error lists every word that was wrong.
func ParseValue(check, value string) (Entry, error) {
	sp, ok := lookup(check)
	if !ok {
		return Entry{}, fmt.Errorf("unknown check %q (known: %s)", check, strings.Join(Names(), ", "))
	}
	e := Entry{set: map[string]func(*model.Checks){}}
	var errs []error
	for _, word := range strings.Fields(value) {
		key, val, isSetting := strings.Cut(word, "=")
		if !isSetting {
			m, err := parseMode(sp, word)
			switch {
			case err != nil:
				errs = append(errs, err)
			case e.Mode != "" && e.Mode != m:
				errs = append(errs, fmt.Errorf("two modes, %s and %s; use one", e.Mode, m))
			default:
				e.Mode = m
			}
			continue
		}
		st, ok := sp.setting(strings.ToLower(key))
		if !ok {
			errs = append(errs, sp.unknownSetting(key))
			continue
		}
		apply, err := st.parse(val)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.key, err))
			continue
		}
		e.set[st.key] = apply
	}
	return e, errors.Join(errs...)
}

func parseMode(sp spec, word string) (model.Mode, error) {
	m := model.Mode(strings.ToLower(word))
	for _, known := range model.Modes {
		if m != known {
			continue
		}
		if m == model.ModeOff && sp.noOff != "" {
			return "", fmt.Errorf("%s cannot be turned off: %s", sp.name, sp.noOff)
		}
		return m, nil
	}
	names := make([]string, len(model.Modes))
	for i, k := range model.Modes {
		names[i] = string(k)
	}
	if len(sp.settings) > 0 {
		return "", fmt.Errorf("%q is not a mode (%s) or a setting like %s=…", word, strings.Join(names, ", "), sp.settings[0].key)
	}
	return "", fmt.Errorf("%q is not a mode (%s)", word, strings.Join(names, ", "))
}

func (sp spec) setting(key string) (setting, bool) {
	for _, st := range sp.settings {
		if st.key == key {
			return st, true
		}
	}
	return setting{}, false
}

func (sp spec) unknownSetting(key string) error {
	if len(sp.settings) == 0 {
		return fmt.Errorf("%s has no settings, only a mode (%q given)", sp.name, key)
	}
	keys := make([]string, len(sp.settings))
	for i, st := range sp.settings {
		keys[i] = st.key
	}
	return fmt.Errorf("%s has no setting %q (it has %s)", sp.name, key, strings.Join(keys, ", "))
}

// FromAnnotations reads every sutats.io/check.<name> annotation. The error
// joins one error per bad annotation, each prefixed with its key.
func FromAnnotations(ann map[string]string) (Layer, error) {
	var keys []string
	for k := range ann {
		if strings.HasPrefix(k, AnnPrefix) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	l := Layer{}
	var errs []error
	for _, k := range keys {
		name := strings.ToLower(strings.TrimPrefix(k, AnnPrefix))
		e, err := ParseValue(name, ann[k])
		if err != nil {
			errs = append(errs, prefix(k, err))
		}
		if e.set != nil { // nil only for an unknown check
			l[name] = e
		}
	}
	return l, errors.Join(errs...)
}

// prefix puts where on every line of a joined error.
func prefix(where string, err error) error {
	var out []error
	for _, line := range strings.Split(err.Error(), "\n") {
		out = append(out, fmt.Errorf("%s: %s", where, line))
	}
	return errors.Join(out...)
}

// Flags registers --check.<name> on fs for every check, defaulting to the
// SUTATS_CHECK_<NAME> environment variable. Call the returned function
// after fs.Parse to get the layer; its error describes every bad flag.
func Flags(fs *flag.FlagSet) func() (Layer, error) {
	values := map[string]*string{}
	for _, sp := range catalogue {
		values[sp.name] = fs.String("check."+sp.name, os.Getenv("SUTATS_CHECK_"+strings.ToUpper(sp.name)),
			fmt.Sprintf("default for the %s check (%s): a mode (on, off, info, degraded, outage) and settings", sp.name, sp.about))
	}
	return func() (Layer, error) {
		l := Layer{}
		var errs []error
		for _, sp := range catalogue {
			v := strings.TrimSpace(*values[sp.name])
			if v == "" {
				continue
			}
			e, err := ParseValue(sp.name, v)
			if err != nil {
				errs = append(errs, prefix("--check."+sp.name, err))
			}
			l[sp.name] = e
		}
		return l, errors.Join(errs...)
	}
}

// Resolve merges layers, lowest precedence first, into the configuration
// in force. For each check the last layer that states a mode wins, and
// each setting takes the value from the last layer that sets it.
func Resolve(layers ...Layer) model.Checks {
	c := model.Checks{Modes: make(map[string]model.Mode, len(catalogue))}
	for _, sp := range catalogue {
		mode := model.ModeOn
		applied := map[string]func(*model.Checks){}
		for _, l := range layers {
			e, ok := l[sp.name]
			if !ok {
				continue
			}
			if e.Mode != "" {
				mode = e.Mode
			}
			for k, fn := range e.set {
				applied[k] = fn
			}
		}
		c.Modes[sp.name] = mode
		// Apply in catalogue order so the result does not depend on map order.
		for _, st := range sp.settings {
			if fn, ok := applied[st.key]; ok {
				fn(&c)
			}
		}
	}
	// Show the defaults in force, so the JSON says what is actually used.
	c.RestartWindow = model.Duration(c.Window())
	c.MinEndpoints = c.MinReady()
	c.TLSWarn = model.Duration(c.Warn())
	return c
}

var serviceName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Services reads the sutats.io/services annotation. A nil result means
// the annotation is absent and Services are matched by selector.
func Services(ann map[string]string) ([]string, error) {
	raw, ok := ann[AnnServices]
	if !ok {
		return nil, nil
	}
	var out, bad []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
		case serviceName.MatchString(s):
			out = append(out, s)
		default:
			bad = append(bad, s)
		}
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("%s: %s is not a Service name", AnnServices, strings.Join(bad, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty; remove it to match Services by selector", AnnServices)
	}
	return out, nil
}

// ---- setting parsers ----

func duration(min, max time.Duration, set func(*model.Checks, time.Duration)) func(string) (func(*model.Checks), error) {
	return func(v string) (func(*model.Checks), error) {
		d, err := model.ParseDuration(v)
		if err != nil {
			return nil, err
		}
		if d < min || d > max {
			return nil, fmt.Errorf("%s is outside %s to %s", v, model.FormatDuration(min), model.FormatDuration(max))
		}
		return func(c *model.Checks) { set(c, d) }, nil
	}
}

func count(min int, set func(*model.Checks, int)) func(string) (func(*model.Checks), error) {
	return func(v string) (func(*model.Checks), error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < min {
			return nil, fmt.Errorf("%q is not a whole number of at least %d", v, min)
		}
		return func(c *model.Checks) { set(c, n) }, nil
	}
}

func hosts(set func(*model.Checks, []string)) func(string) (func(*model.Checks), error) {
	return func(v string) (func(*model.Checks), error) {
		var out []string
		for _, h := range strings.Split(v, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			if !validHost(h) {
				return nil, fmt.Errorf("%q is not a host name (give the name only, such as shop.example.com or shop.example.com:8443)", h)
			}
			out = append(out, h)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no hosts given")
		}
		return func(c *model.Checks) { set(c, append([]string(nil), out...)) }, nil
	}
}

var hostName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validHost(h string) bool {
	host := h
	if hh, port, err := net.SplitHostPort(h); err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
		host = hh
	}
	return len(host) <= 253 && hostName.MatchString(host)
}
