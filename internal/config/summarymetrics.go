package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"gopkg.in/yaml.v3"
)

// SummaryMetrics (UI-7a, A'3) declares the environment's OWN metrics source and the few
// named numbers the executor reads from it and reports. Top-level key `summary_metrics`, decoded
// leniently like every key outside `targets` -- NEVER under `targets:` (strict decode: an old executor
// would refuse the whole config on a rollback). Absent = today's behaviour exactly.
//
// Bounds (the contract, enforced here at load time AND again by the executor and the control plane):
// `every` 1m..60m (default 5m), at most federation.SummaryMaxReadings (12) readings.
type SummaryMetrics struct {
	Every    string               `yaml:"every"`
	Source   SummarySource        `yaml:"source"`
	Readings []SummaryReadingDecl `yaml:"readings"`
}

// SummarySource is where the numbers are read.
//
//	type: prometheus        GET <url>/api/v1/query?query=<reading.query>; must return ONE finite sample
//	type: metrics_endpoint  GET <url>; reading.query is a series name with an optional {label="v"}
//	                        matcher; every matching sample is summed
//
// URL carries no userinfo and no query string (a credential does not belong in a URL that is logged and
// displayed). Credential is a ${VAR} reference ONLY -- the same rule as observability.betterstack.credential
// -- resolving to Basic-auth "user:password".
type SummarySource struct {
	Type       string `yaml:"type"`
	URL        string `yaml:"url"`
	Credential string `yaml:"credential"`
}

// SummaryReadingDecl is one declared reading. Name is the stable key; Target optionally names the test
// target (UI-6) it describes; Unit is a short display label; ComfortableLimit, when given, is the number
// above which the environment's owner stops calling the load comfortable (a display aid, never a gate).
type SummaryReadingDecl struct {
	Name             string   `yaml:"name"`
	Target           string   `yaml:"target"`
	Unit             string   `yaml:"unit"`
	Query            string   `yaml:"query"`
	ComfortableLimit *float64 `yaml:"comfortable_limit"`
}

// Source types.
const (
	SummarySourcePrometheus      = "prometheus"
	SummarySourceMetricsEndpoint = "metrics_endpoint"
)

const summaryQueryMax = 500

var (
	summaryNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	// a series name with an optional label matcher block: foo, foo{a="b",c="d"}
	summarySeriesRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^{}]*\})?$`)
)

// SummaryMetricsDecl returns the declared block, or nil when the config declares none.
func (c *Config) SummaryMetricsDecl() *SummaryMetrics { return c.SummaryMetrics }

// EveryDuration is the declared read interval, or the default (5m) when none is declared. A value that
// fails validate() never gets here from a loaded config; it is clamped anyway so no caller can read more
// often than the minimum.
func (s *SummaryMetrics) EveryDuration() time.Duration {
	if s == nil || strings.TrimSpace(s.Every) == "" {
		return federation.SummaryDefaultEvery
	}
	d, err := time.ParseDuration(strings.TrimSpace(s.Every))
	if err != nil || d < federation.SummaryMinEvery {
		return federation.SummaryMinEvery
	}
	if d > federation.SummaryMaxEvery {
		return federation.SummaryMaxEvery
	}
	return d
}

func (s *SummaryMetrics) validate() error {
	if s == nil {
		return nil
	}
	if e := strings.TrimSpace(s.Every); e != "" {
		d, err := time.ParseDuration(e)
		if err != nil {
			return fmt.Errorf("summary_metrics.every %q is not a duration (e.g. 5m): %v", s.Every, err)
		}
		if d < federation.SummaryMinEvery || d > federation.SummaryMaxEvery {
			return fmt.Errorf("summary_metrics.every %q is outside %s..%s", s.Every, federation.SummaryMinEvery, federation.SummaryMaxEvery)
		}
	}
	switch s.Source.Type {
	case SummarySourcePrometheus, SummarySourceMetricsEndpoint:
	default:
		return fmt.Errorf("summary_metrics.source.type %q must be %q or %q", s.Source.Type, SummarySourcePrometheus, SummarySourceMetricsEndpoint)
	}
	u, err := url.Parse(strings.TrimSpace(s.Source.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("summary_metrics.source.url must be an http(s) URL with a host")
	}
	if u.User != nil {
		return errors.New("summary_metrics.source.url carries a credential (userinfo); remove it and declare summary_metrics.source.credential: ${VAR}")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("summary_metrics.source.url carries a query string or fragment, which can hold a credential; remove it (a Prometheus API path needs none) and declare summary_metrics.source.credential: ${VAR} if the source needs one")
	}
	if cred := strings.TrimSpace(s.Source.Credential); cred != "" && !bsCredentialVarRe.MatchString(cred) {
		return errors.New("summary_metrics.source.credential must be a single ${VAR} reference (resolving to user:password), never a literal")
	}
	if len(s.Readings) == 0 {
		return errors.New("summary_metrics.readings must declare at least one reading")
	}
	if len(s.Readings) > federation.SummaryMaxReadings {
		return fmt.Errorf("summary_metrics.readings declares %d readings; at most %d are allowed", len(s.Readings), federation.SummaryMaxReadings)
	}
	seen := map[string]bool{}
	for i, r := range s.Readings {
		at := fmt.Sprintf("summary_metrics.readings[%d]", i)
		if !summaryNameRE.MatchString(r.Name) {
			return fmt.Errorf("%s.name %q must match ^[a-z][a-z0-9_]{0,39}$", at, r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("%s.name %q is a duplicate", at, r.Name)
		}
		seen[r.Name] = true
		if len(r.Unit) > federation.SummaryUnitMax || len(r.Target) > federation.SummaryTargetMax {
			return fmt.Errorf("%s: unit is at most %d and target at most %d characters", at, federation.SummaryUnitMax, federation.SummaryTargetMax)
		}
		q := strings.TrimSpace(r.Query)
		if q == "" {
			return fmt.Errorf("%s.query is required", at)
		}
		if len(q) > summaryQueryMax {
			return fmt.Errorf("%s.query is longer than %d characters", at, summaryQueryMax)
		}
		if s.Source.Type == SummarySourceMetricsEndpoint && !summarySeriesRE.MatchString(q) {
			return fmt.Errorf("%s.query %q must be a series name with an optional {label=\"value\"} matcher for a metrics_endpoint source", at, q)
		}
		if r.ComfortableLimit != nil && (math.IsNaN(*r.ComfortableLimit) || math.IsInf(*r.ComfortableLimit, 0)) {
			return fmt.Errorf("%s.comfortable_limit must be a finite number", at)
		}
	}
	return nil
}

// strictSummaryMetrics refuses an unknown key inside summary_metrics by name (a mistyped `querry:` would
// otherwise parse to an empty query). It decodes ONLY that node strictly -- the rest of the file stays
// lenient, which is what lets an older executor ignore the whole block.
func strictSummaryMetrics(b []byte) error {
	var d struct {
		SM yaml.Node `yaml:"summary_metrics"`
	}
	if err := yaml.Unmarshal(b, &d); err != nil || d.SM.Kind == 0 {
		return nil // the lenient pass already reported a syntax error, or the block is absent
	}
	nb, err := yaml.Marshal(&d.SM)
	if err != nil {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(nb))
	dec.KnownFields(true)
	var sm SummaryMetrics
	if err := dec.Decode(&sm); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("summary_metrics: %v", err)
	}
	return nil
}
