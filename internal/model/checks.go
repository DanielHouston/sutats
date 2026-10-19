package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Mode is how one check is used. Each check produces signals of the kind
// with the same name (pods, stability, scheduling, endpoints, ingress, tls,
// http), so a mode applies to every signal of that kind.
type Mode string

const (
	// ModeOn runs the check and counts it as it reports. The default.
	ModeOn Mode = "on"
	// ModeOff does not run the check; it has no row and no bar.
	ModeOff Mode = "off"
	// ModeInfo runs the check and shows its row, but the component's status
	// and uptime ignore it.
	ModeInfo Mode = "info"
	// ModeDegraded caps the check at Degraded: an outage it reports only
	// degrades the component.
	ModeDegraded Mode = "degraded"
	// ModeOutage raises the check to Outage: anything it reports as degraded
	// takes the component down.
	ModeOutage Mode = "outage"
)

// Modes lists every mode, for parsing and error messages.
var Modes = []Mode{ModeOn, ModeOff, ModeInfo, ModeDegraded, ModeOutage}

// Counts returns what a signal reporting s contributes to its component.
// Unknown means it contributes nothing.
func (m Mode) Counts(s Status) Status {
	switch m {
	case ModeInfo, ModeOff:
		return StatusUnknown
	case ModeDegraded:
		if s == StatusOutage {
			return StatusDegraded
		}
	case ModeOutage:
		if s == StatusDegraded {
			return StatusOutage
		}
	}
	return s
}

// Defaults for check settings that are not configured.
const (
	DefaultRestartWindow = 15 * time.Minute
	DefaultTLSWarn       = 7 * 24 * time.Hour
	DefaultMinEndpoints  = 1
)

// Checks is the resolved check configuration for one component. The zero
// value means every check on with default settings; the accessor methods
// fill in defaults, so always read settings through them.
type Checks struct {
	// Modes holds each check's mode by name. A missing entry means on.
	Modes map[string]Mode `json:"modes,omitempty"`

	// Stability: how far back restarts count, and how many are tolerated
	// in that window before the check reports Degraded.
	RestartWindow     Duration `json:"restart_window,omitempty"`
	RestartsTolerated int      `json:"restarts_tolerated,omitempty"`

	// Endpoints: ready endpoints each Service needs; fewer is Degraded.
	// Services, when set, names the Services that belong to the workload
	// instead of matching them by selector. It also decides which Ingresses
	// and Routes count, since those are found through the Services.
	MinEndpoints int      `json:"min_endpoints,omitempty"`
	Services     []string `json:"services,omitempty"`

	// TLS: how close to expiry a certificate is Degraded, and hosts to check
	// on top of those found on Ingresses and Routes.
	TLSWarn  Duration `json:"tls_warn,omitempty"`
	TLSHosts []string `json:"tls_hosts,omitempty"`
}

// Mode returns the mode for a check, on when not configured.
func (c Checks) Mode(check string) Mode {
	if m, ok := c.Modes[check]; ok && m != "" {
		return m
	}
	return ModeOn
}

// Enabled reports whether a check runs at all.
func (c Checks) Enabled(check string) bool { return c.Mode(check) != ModeOff }

// Window is the stability check's restart window.
func (c Checks) Window() time.Duration {
	if c.RestartWindow > 0 {
		return time.Duration(c.RestartWindow)
	}
	return DefaultRestartWindow
}

// Warn is how close to expiry a certificate is Degraded.
func (c Checks) Warn() time.Duration {
	if c.TLSWarn > 0 {
		return time.Duration(c.TLSWarn)
	}
	return DefaultTLSWarn
}

// MinReady is the number of ready endpoints each Service needs.
func (c Checks) MinReady() int {
	if c.MinEndpoints > 0 {
		return c.MinEndpoints
	}
	return DefaultMinEndpoints
}

// Duration is a time.Duration that reads and writes as text such as 30m,
// 2h or 14d, both in JSON and in annotations.
type Duration time.Duration

// MarshalText writes the shortest exact form: 14d, 2h, 30m, 90s.
func (d Duration) MarshalText() ([]byte, error) { return []byte(FormatDuration(time.Duration(d))), nil }

// UnmarshalText reads anything ParseDuration accepts.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration reads a Go duration (30m, 1h30m) or a whole number of days
// (14d), since days are the natural unit for certificate warnings.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("cannot parse %q (use values like 30m, 12h or 14d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q (use values like 30m, 12h or 14d)", s)
	}
	return d, nil
}

// FormatDuration writes d in the largest unit that divides it exactly.
func FormatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
