package checks

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/DanielHouston/sutats/internal/model"
)

func TestParseValue(t *testing.T) {
	e, err := ParseValue("stability", "info window=30m tolerate=2")
	if err != nil {
		t.Fatal(err)
	}
	c := Resolve(Layer{"stability": e})
	if c.Mode("stability") != model.ModeInfo || c.Window() != 30*time.Minute || c.RestartsTolerated != 2 {
		t.Fatalf("resolved = %+v", c)
	}

	// Case and spacing are forgiving; an empty value says nothing.
	if e, err := ParseValue("tls", "  DEGRADED   Warn=14d "); err != nil || e.Mode != model.ModeDegraded {
		t.Fatalf("case-insensitive parse = %+v, %v", e, err)
	}
	if e, err := ParseValue("tls", ""); err != nil || e.Mode != "" || len(e.set) != 0 {
		t.Fatalf("empty value = %+v, %v", e, err)
	}
}

func TestParseValueErrors(t *testing.T) {
	cases := []struct{ check, value, want string }{
		{"nope", "on", `unknown check "nope"`},
		{"pods", "off", "pods cannot be turned off"},
		{"stability", "of", `"of" is not a mode (on, off, info, degraded, outage) or a setting like window=…`},
		{"ingress", "maybe", `"maybe" is not a mode (on, off, info, degraded, outage)`},
		{"stability", "info off", "two modes, info and off"},
		{"stability", "windw=5m", `stability has no setting "windw" (it has window, tolerate)`},
		{"ingress", "min=2", `ingress has no settings, only a mode ("min" given)`},
		{"stability", "window=5", `window: cannot parse "5"`},
		{"stability", "window=10s", "window: 10s is outside 1m to 1d"},
		{"stability", "tolerate=-1", `tolerate: "-1" is not a whole number of at least 0`},
		{"endpoints", "min=0", `min: "0" is not a whole number of at least 1`},
		{"tls", "hosts=https://shop.example.com", `hosts: "https://shop.example.com" is not a host name`},
		{"tls", "hosts=shop.example.com:99999", `is not a host name`},
		{"tls", "hosts=", "hosts: no hosts given"},
	}
	for _, c := range cases {
		_, err := ParseValue(c.check, c.value)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: err = %v, want it to contain %q", c.check, c.value, err, c.want)
		}
	}
}

func TestBadWordsDoNotUndoGoodOnes(t *testing.T) {
	e, err := ParseValue("stability", "info window=nonsense tolerate=3")
	if err == nil {
		t.Fatal("want an error for the bad window")
	}
	c := Resolve(Layer{"stability": e})
	if c.Mode("stability") != model.ModeInfo || c.RestartsTolerated != 3 || c.Window() != model.DefaultRestartWindow {
		t.Fatalf("resolved = %+v", c)
	}
}

func TestFromAnnotations(t *testing.T) {
	l, err := FromAnnotations(map[string]string{
		"sutats.io/check.TLS":        "warn=14d hosts=shop.example.com,Admin.Example.com:8443",
		"sutats.io/check.scheduling": "off",
		"sutats.io/check.bogus":      "off",
		"sutats.io/check.stability":  "window=2h banana",
		"sutats.io/component":        "Shop",
	})
	msgs := strings.Split(err.Error(), "\n")
	if len(msgs) != 2 ||
		msgs[0] != `sutats.io/check.bogus: unknown check "bogus" (known: pods, stability, scheduling, endpoints, ingress, tls, http)` ||
		msgs[1] != `sutats.io/check.stability: "banana" is not a mode (on, off, info, degraded, outage) or a setting like window=…` {
		t.Fatalf("errors = %q", msgs)
	}
	c := Resolve(l)
	if c.Warn() != 14*24*time.Hour || strings.Join(c.TLSHosts, " ") != "shop.example.com admin.example.com:8443" {
		t.Errorf("tls = %v %v", c.Warn(), c.TLSHosts)
	}
	if c.Enabled("scheduling") || c.Window() != 2*time.Hour {
		t.Errorf("scheduling on = %v, window %v", c.Enabled("scheduling"), c.Window())
	}
	if l, err := FromAnnotations(map[string]string{"sutats.io/component": "x"}); l != nil || err != nil {
		t.Errorf("no check annotations = %v, %v", l, err)
	}
}

// Flags are the defaults, the namespace overrides them and the workload
// overrides both, per check and per setting.
func TestResolvePrecedence(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := Flags(fs)
	if err := fs.Parse([]string{"--check.tls=warn=21d", "--check.stability=degraded window=30m", "--check.ingress=off"}); err != nil {
		t.Fatal(err)
	}
	flags, err := get()
	if err != nil {
		t.Fatal(err)
	}
	ns, _ := FromAnnotations(map[string]string{
		"sutats.io/check.stability": "tolerate=1",
		"sutats.io/check.tls":       "off",
	})
	own, _ := FromAnnotations(map[string]string{
		"sutats.io/check.stability": "info",
		"sutats.io/check.tls":       "on",
		"sutats.io/check.ingress":   "on",
	})
	c := Resolve(flags, ns, own)
	if c.Mode("stability") != model.ModeInfo || c.Window() != 30*time.Minute || c.RestartsTolerated != 1 {
		t.Errorf("stability = %v %v %v", c.Mode("stability"), c.Window(), c.RestartsTolerated)
	}
	if c.Mode("tls") != model.ModeOn || c.Warn() != 21*24*time.Hour {
		t.Errorf("tls = %v %v (the workload turns back on what the namespace turned off, keeping the flag's warn)", c.Mode("tls"), c.Warn())
	}
	if !c.Enabled("ingress") {
		t.Error("workload should re-enable ingress")
	}
	// Without the workload layer the namespace's choices stand.
	c = Resolve(flags, ns)
	if c.Enabled("tls") || c.Enabled("ingress") || c.Mode("stability") != model.ModeDegraded {
		t.Errorf("namespace layer = %+v", c.Modes)
	}
	// Defaults are written out so the API shows what is in force.
	c = Resolve()
	if c.RestartWindow != model.Duration(model.DefaultRestartWindow) || c.MinEndpoints != 1 || c.TLSWarn != model.Duration(model.DefaultTLSWarn) || len(c.Modes) != len(Names()) {
		t.Errorf("defaults = %+v", c)
	}
}

func TestFlagErrors(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := Flags(fs)
	_ = fs.Parse([]string{"--check.pods=off", "--check.tls=warn=soon"})
	_, err := get()
	if err == nil || !strings.Contains(err.Error(), "--check.pods: pods cannot be turned off") || !strings.Contains(err.Error(), `--check.tls: warn: cannot parse "soon"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestServices(t *testing.T) {
	if s, err := Services(map[string]string{}); s != nil || err != nil {
		t.Errorf("absent = %v, %v", s, err)
	}
	if s, err := Services(map[string]string{AnnServices: " api, api-admin ,"}); err != nil || strings.Join(s, " ") != "api api-admin" {
		t.Errorf("list = %v, %v", s, err)
	}
	if s, err := Services(map[string]string{AnnServices: "api, Bad_Name"}); err == nil || len(s) != 1 {
		t.Errorf("bad name = %v, %v", s, err)
	}
	if _, err := Services(map[string]string{AnnServices: " "}); err == nil {
		t.Error("an empty list should be an error")
	}
}
