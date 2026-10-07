// Package testtargets is the pure core of "one Argus instance declares several named test targets"
// (, UI-6 backend): the declaration shape, its load-time validation, the scenario →
// target mapping, and the wire encoding the executor reports and the control plane stores.
//
// ⚠ NAMING. "test target" is NOT the connection `targets:` block of argus-config (strict, http/database/…)
// and NOT federation's `declared_targets` (the connection-target keys). It is a named slice of ONE
// instance's checks — e.g. `live` (namespace msgbus) and `lab` (namespace msgbus-lab) — that the UI
// shows as separate cards. The word is `test_targets` everywhere in code, config, wire and columns.
//
// The types carry yaml AND json tags so config.Config decodes them from argus-config.yaml and the same
// struct is the wire element, with no second copy to drift. This package imports nothing from the rest of
// the module, so both the executor (internal/runner, internal/config) and the control plane
// (internal/control/store) can use it.
package testtargets

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MaxTargets is the most test targets one instance may declare.
const MaxTargets = 8

// Unassigned is the pseudo-target of a scenario that matches no declared target. It is not declarable.
const Unassigned = "unassigned"

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Match says which scenarios belong to a target. A scenario's target is the FIRST target (declaration
// order) whose ScenarioPrefixes match its id; failing that, the first whose Tags intersect its tags.
type Match struct {
	ScenarioPrefixes []string `yaml:"scenario_prefixes,omitempty" json:"scenario_prefixes,omitempty"`
	Tags             []string `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// Target is one declared test target.
type Target struct {
	Name string `yaml:"name" json:"name"`
	// Label is display text (defaults to Name in the UI). Namespace is INFORMATIONAL: shown, never dialled.
	Label       string `yaml:"label,omitempty" json:"label,omitempty"`
	Namespace   string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Match       Match  `yaml:"match" json:"match"`
	// DashboardLinkTemplate optionally overrides the instance-wide dashboard link template (UI-2). It is
	// carried verbatim; the template's own rules belong to UI-2.
	DashboardLinkTemplate string `yaml:"dashboard_link_template,omitempty" json:"dashboard_link_template,omitempty"`
	// LoadTest is `load_test: never` or absent. never = this target must never be put under
	// load: validate-config refuses a load check that belongs to it, the executor refuses to fire one, and the
	// Capacity answer says "not load tested, by declaration". Absent ("") is today's behaviour and the ONLY other
	// accepted value. Anything else is refused by Validate. On the wire, a declaration reported without the key
	// (an older executor) decodes to "" = not declared, never to never.
	LoadTest string `yaml:"load_test,omitempty" json:"load_test,omitempty"`
	// OutsideCluster is `outside_cluster: true` or absent. true = the system under test this
	// target names does not run in a Kubernetes cluster (a hosted app behind a CDN), so a load run's environment
	// capture has nothing to read for it and says so, instead of asking the Kubernetes API and reporting a refusal.
	// It is refused together with a `namespace` on the same target. Absent (false) is today's behaviour; on the
	// wire a declaration reported without the key (an older executor) decodes to false.
	OutsideCluster bool `yaml:"outside_cluster,omitempty" json:"outside_cluster,omitempty"`
}

// LoadTestNever is the one value of Target.LoadTest.
const LoadTestNever = "never"

// NeverLoadTested reports whether the target declares `load_test: never`.
func (t Target) NeverLoadTested() bool { return t.LoadTest == LoadTestNever }

// DisplayName returns the display text of the target: its label, else its name.
func (t Target) DisplayName() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Name
}

// ByName returns the declared target called name.
func (l List) ByName(name string) (Target, bool) {
	for _, t := range l {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// List is the ordered `test_targets` declaration. nil = not declared; empty = declared none.
type List []Target

// Validate applies the load-time rules (new executors only; an older one ignores the key entirely).
func (l List) Validate() error {
	if len(l) > MaxTargets {
		return fmt.Errorf("test_targets: declares %d targets, at most %d are allowed", len(l), MaxTargets)
	}
	seen := map[string]bool{}
	for i, t := range l {
		where := fmt.Sprintf("test_targets[%d]", i)
		if t.Name == Unassigned {
			return fmt.Errorf("%s: the name %q is reserved for scenarios that match no target", where, Unassigned)
		}
		if !nameRE.MatchString(t.Name) {
			return fmt.Errorf("%s: name %q must match ^[a-z][a-z0-9-]{0,31}$", where, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: duplicate name %q — test_targets names must be unique", where, t.Name)
		}
		seen[t.Name] = true
		if len(t.Match.ScenarioPrefixes) == 0 && len(t.Match.Tags) == 0 {
			return fmt.Errorf("%s (%s): match is empty — give scenario_prefixes and/or tags, or the target can never own a check", where, t.Name)
		}
		for _, p := range t.Match.ScenarioPrefixes {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("%s (%s): match.scenario_prefixes has a blank entry", where, t.Name)
			}
		}
		for _, g := range t.Match.Tags {
			if strings.TrimSpace(g) == "" {
				return fmt.Errorf("%s (%s): match.tags has a blank entry", where, t.Name)
			}
		}
		if t.LoadTest != "" && t.LoadTest != LoadTestNever {
			return fmt.Errorf("%s (%s): load_test %q is not accepted — the only value is %q (or leave the key out)", where, t.Name, t.LoadTest, LoadTestNever)
		}
		if t.OutsideCluster && t.Namespace != "" {
			return fmt.Errorf("%s (%s): outside_cluster: true and namespace %q cannot be declared together — a target outside the cluster has no Kubernetes namespace; remove one of the two keys", where, t.Name, t.Namespace)
		}
		if len(t.DashboardLinkTemplate) > 2048 {
			return fmt.Errorf("%s (%s): dashboard_link_template is longer than 2048 bytes", where, t.Name)
		}
	}
	return nil
}

// ValidName reports whether s has the shape of a test target name (^[a-z][a-z0-9-]{0,31}$) — the rule
// Validate applies to a declared name. Unassigned passes it: it is a real value to FILTER by (the web's
// `?target=`), only not a declarable one (Validate refuses that separately).
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Map returns the target a scenario belongs to: the first target whose scenario_prefixes match id, else
// the first whose tags intersect tags, else Unassigned.
func (l List) Map(id string, tags []string) string {
	for _, t := range l {
		for _, p := range t.Match.ScenarioPrefixes {
			if p != "" && strings.HasPrefix(id, p) {
				return t.Name
			}
		}
	}
	for _, t := range l {
		for _, want := range t.Match.Tags {
			for _, have := range tags {
				if want == have {
					return t.Name
				}
			}
		}
	}
	return Unassigned
}

// ForRun is the set of targets a run exercised, from the executor's own declaration and the scenario ids
// the run selected (tagsByID is the catalog's tags per id). Declaration order, Unassigned last.
//
//	nil           the instance declares no test targets: the run is not stamped (today's one-target view)
//	[]string{}    declared, but the run selected no scenario: stamped, with nothing to name
func (l List) ForRun(scenarioIDs []string, tagsByID map[string][]string) []string {
	if len(l) == 0 {
		return nil
	}
	hit := map[string]bool{}
	for _, id := range scenarioIDs {
		if id == "" {
			continue
		}
		hit[l.Map(id, tagsByID[id])] = true
	}
	out := []string{}
	for _, t := range l {
		if hit[t.Name] {
			out = append(out, t.Name)
		}
	}
	if hit[Unassigned] {
		out = append(out, Unassigned)
	}
	return out
}

// Encode is the wire form: a JSON array, and an explicit "[]" for an empty or nil list ("reported none",
// never nil, which on the wire means "not reported").
func Encode(l List) (json.RawMessage, error) {
	if len(l) == 0 {
		return json.RawMessage(`[]`), nil
	}
	return json.Marshal(l)
}

// Decode reads the wire form. reported is false for a nil/empty raw message ("not reported": leave
// whatever is stored). An invalid declaration is refused, so the control plane stores only what an
// executor could have loaded. Unknown keys inside an element are ignored (forward compatibility).
func Decode(raw json.RawMessage) (l List, reported bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, true, fmt.Errorf("test_targets: not a JSON array of targets: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, true, err
	}
	return l, true, nil
}
