package config

import (
	"fmt"
	"regexp"
	"time"
)

// preconditionprobe.go -- the precondition probe's image and bounds.
//
//	# argus-config.yaml, TOP LEVEL (never under targets:), all keys optional
//	precondition_probe:
//	  image: <ref@sha256:…>      # default: busybox:1.36.1 pinned by digest (precondition.DefaultImage); needs busybox `nc`; pulled by the executor's namespace
//	  ready_timeout: 90s         # how long a probe pod may take to run / finish
//	  connect_timeout: 2s        # one connection attempt
//	  attempts: 4                # attempts per control
//	  settle: 5s                 # pause after a NetworkPolicy changes, for the policy engine to program it
//
// TOP-LEVEL for load_allowed_targets' reason: an executor built before this key existed ignores it.
// An absent block means the built-in defaults (internal/precondition.Default*). The probe only ever runs in the
// executor's OWN namespace; there is no key that names another one.

// PreconditionProbe is the top-level `precondition_probe` block.
type PreconditionProbe struct {
	Image          string `yaml:"image"`
	ReadyTimeout   string `yaml:"ready_timeout"`
	ConnectTimeout string `yaml:"connect_timeout"`
	Attempts       int    `yaml:"attempts"`
	Settle         string `yaml:"settle"`
}

var probeImageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)

// Bounds a configured value must stay inside.
const (
	probeMaxTimeout  = 10 * time.Minute
	probeMaxAttempts = 20
)

// PreconditionProbeSettings is the probe's configuration, zero where the block leaves a key unset (the
// precondition package applies its defaults to zeros).
type PreconditionProbeSettings struct {
	Image                                string
	ReadyTimeout, ConnectTimeout, Settle time.Duration
	Attempts                             int
}

func parseProbeDuration(key, v string, positive bool) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || (positive && d == 0) {
		return 0, fmt.Errorf("precondition_probe.%s %q is not a positive Go duration (\"5s\", \"90s\")", key, v)
	}
	if d > probeMaxTimeout {
		return 0, fmt.Errorf("precondition_probe.%s of %s is more than the %s bound", key, d, probeMaxTimeout)
	}
	return d, nil
}

// PreconditionProbe resolves the block. A malformed value is an error (validate-config refuses it); an absent
// block is the zero settings.
func (c *Config) PreconditionProbeSettings() (PreconditionProbeSettings, error) {
	var s PreconditionProbeSettings
	if c == nil || c.PreconditionProbe == nil {
		return s, nil
	}
	p := c.PreconditionProbe
	if p.Image != "" && !probeImageRe.MatchString(p.Image) {
		return s, fmt.Errorf("precondition_probe.image %q is not an image reference", p.Image)
	}
	if p.Attempts < 0 || p.Attempts > probeMaxAttempts {
		return s, fmt.Errorf("precondition_probe.attempts is %d, outside 0..%d", p.Attempts, probeMaxAttempts)
	}
	s.Image, s.Attempts = p.Image, p.Attempts
	var err error
	if s.ReadyTimeout, err = parseProbeDuration("ready_timeout", p.ReadyTimeout, true); err != nil {
		return s, err
	}
	if s.ConnectTimeout, err = parseProbeDuration("connect_timeout", p.ConnectTimeout, true); err != nil {
		return s, err
	}
	if s.Settle, err = parseProbeDuration("settle", p.Settle, false); err != nil {
		return s, err
	}
	return s, nil
}
