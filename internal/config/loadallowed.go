package config

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// loadallowed.go -- S9, "load only where it is allowed" (; HTTP Load).
//
//	# argus-config.yaml, TOP LEVEL, beside `targets:` (never under it)
//	load_allowed_targets:
//	  msgbus-lab:                # a name under targets.message_broker_targets (AMQP Load)
//	    max_sessions: 1000        # optional ceiling, default 2000 (the scenario validator's own bound)
//	  api-lab:                    # or a name under targets.http_targets (HTTP Load); max_sessions caps its users
//
// ⛔ OFF BY DEFAULT: an absent or empty block, or a name not listed, means no target allows load.
// ⛔ TOP-LEVEL, never a field of the target: `targets` is decoded STRICTLY, so an executor built before
// this key existed would REFUSE a new key under it and a rollback would take the executor down; a
// top-level key falls into that executor's lenient inline map and is ignored (design fact 3). Whether the
// operator marked a target is the operator's call, which is also why it is not a scenario field.

// LoadAllow is one entry of the top-level `load_allowed_targets` block.
type LoadAllow struct {
	// MaxSessions is the largest step this target may be asked for: AMQP sessions, or HTTP users (the same
	// thing, one JMeter thread each). Absent = DefaultLoadMaxSessions. A pointer so an explicit `0` is refused
	// instead of reading as "absent".
	MaxSessions *int `yaml:"max_sessions"`
}

// DefaultLoadMaxSessions is the ceiling of an entry that declares none: the scenario validator's own
// per-step bound (scenario.AMQPLoadProfile and HTTPLoadProfile Steps entries are 1..2000).
const DefaultLoadMaxSessions = 2000

// maxLoadMaxSessions bounds a declared ceiling.
const maxLoadMaxSessions = 10000

func (a LoadAllow) ceiling() int {
	if a.MaxSessions == nil {
		return DefaultLoadMaxSessions
	}
	return *a.MaxSessions
}

// validateLoadAllowed is the load-time check (so validate-config and onboarding see it). It never echoes
// a URL: the refusal names the entry, not the broker or the host.
//
// an entry names a targets.message_broker_targets entry (AMQP Load) OR a targets.http_targets
// entry (HTTP Load). A name declared under neither is refused; the shared/production rule applies to whichever
// is declared.
func (c *Config) validateLoadAllowed() error {
	var errs []string
	for _, n := range sortedKeys(c.LoadAllowedTargets) {
		where := "load_allowed_targets." + n
		mq := c.Targets.MessageBrokerTargets[n]
		ht := c.Targets.HTTPTargets[n]
		switch {
		case isPlainSlotKey(n):
			errs = append(errs, fmt.Sprintf("%s: the plain targets.message_broker slot can never be a load target — name an entry under targets.message_broker_targets", where))
			continue
		case mq == nil && ht == nil:
			msg := fmt.Sprintf("%s: no targets.message_broker_targets.%s is declared", where, n)
			if near := nearest(n, c.NamedTargets(KindMessageBroker)); near != "" {
				msg += fmt.Sprintf(" — did you mean %s?", near)
			} else if near := nearest(n, c.NamedTargets(KindHTTP)); near != "" {
				msg += fmt.Sprintf(" (nor targets.http_targets.%s) — did you mean %s?", n, near)
			}
			errs = append(errs, msg)
			continue
		}
		if m := c.LoadAllowedTargets[n].MaxSessions; m != nil && (*m < 1 || *m > maxLoadMaxSessions) {
			errs = append(errs, fmt.Sprintf("%s.max_sessions = %d is out of bounds [1, %d]", where, *m, maxLoadMaxSessions))
		}
		if mq != nil && (looksSharedOrProd(mq.URL) || looksSharedOrProd(mq.ManagementURL)) {
			errs = append(errs, fmt.Sprintf("%s: its url looks shared/production — the rule is \"live = heartbeat only\": use a lab or a dedicated test broker", where))
		}
		if ht != nil && looksSharedOrProd(ht.BaseURL) {
			errs = append(errs, fmt.Sprintf("%s: its base_url looks shared/production — the rule is \"live = heartbeat only\": load only a lab or a dedicated test deployment", where))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// loadDrive is what a load scenario drives: the named target, its kind as the refusal names it, the unit of a
// step, and the top step.
type loadDrive struct {
	name, kind, unit string
	top              int
}

// loadDrivesTarget reports whether a scenario drives load at a named target (an AMQP Load scenario at a
// message_broker target, an HTTP Load scenario at an http target), and which.
func loadDrivesTarget(s *scenario.Scenario) (loadDrive, bool) {
	if s == nil {
		return loadDrive{}, false
	}
	switch scenario.PrimaryLayer(s) {
	case scenario.AMQPLoadLayer:
		return loadDrive{name: s.Target, kind: "message_broker", unit: "sessions", top: topStep(s)}, true
	case scenario.HTTPLoadLayer:
		return loadDrive{name: s.Target, kind: "http", unit: "users", top: scenario.HTTPLoadTopStep(s.HTTPLoad)}, true
	}
	return loadDrive{}, false
}

func topStep(s *scenario.Scenario) int {
	top := 0
	if s.AMQPLoad != nil {
		for _, n := range s.AMQPLoad.Steps {
			if n > top {
				top = n
			}
		}
	}
	return top
}

// listedFor: the target is listed under load_allowed_targets AND, for HTTP Load, the listed name is declared as an
// http target. An entry that exists only as a broker does not allow an HTTP ramp at a same-named http target the
// operator never declared (and the run would refuse an undeclared target anyway). The AMQP rule is unchanged.
func (c *Config) listedFor(d loadDrive) (LoadAllow, bool) {
	a, listed := c.LoadAllowedTargets[d.name]
	if !listed {
		return a, false
	}
	if d.kind == "http" && c.Targets.HTTPTargets[d.name] == nil {
		return a, false
	}
	return a, true
}

// LoadRefusal says why a load scenario (AMQP Load or HTTP Load) may not run; "" = allowed. It is the text a run
// reports as its Observed line: it NEVER echoes a URL, host or credential (the entry's name only), and the
// scenario's declared thresholds are not in it. The AMQP Load wording is unchanged.
func (c *Config) LoadRefusal(s *scenario.Scenario) string {
	d, ok := loadDrivesTarget(s)
	if !ok {
		return ""
	}
	a, listed := c.listedFor(d)
	if !listed {
		what := "the broker is a lab or a dedicated test broker"
		if d.kind == "http" {
			what = "the target is a lab or a dedicated test deployment"
		}
		return fmt.Sprintf("refused before firing: this scenario drives load at %s target %q, and the operator has not marked that target as allowing load. "+
			"Add it under load_allowed_targets in argus-config.yaml (top level, beside targets:) only if %s, never a live one. "+
			"Nothing was sent. - preflight", d.kind, d.name, what)
	}
	if d.top > a.ceiling() {
		return fmt.Sprintf("refused before firing: this scenario drives load at %s target %q, but its Steps reach %d %s and load_allowed_targets.%s.max_sessions is %d. "+
			"Nothing was sent. - preflight", d.kind, d.name, d.top, d.unit, d.name, a.ceiling())
	}
	return ""
}

// loadValidateMessage is the authoring-time wording (validate-config), "" when the scenario is fine.
func (c *Config) loadValidateMessage(s *scenario.Scenario) string {
	d, ok := loadDrivesTarget(s)
	if !ok {
		return ""
	}
	a, listed := c.listedFor(d)
	if !listed {
		return fmt.Sprintf("this scenario drives load at %s target %q, which load_allowed_targets does not list", d.kind, d.name)
	}
	if d.top > a.ceiling() {
		return fmt.Sprintf("this scenario's Steps reach %d %s but load_allowed_targets.%s.max_sessions is %d", d.top, d.unit, d.name, a.ceiling())
	}
	return ""
}
