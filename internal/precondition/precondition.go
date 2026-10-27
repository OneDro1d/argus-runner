// Package precondition is the precondition tier (issue #671): built-in probes that answer
// "is the mechanism this check relies on actually there?" BEFORE the checks that depend on it run.
//
// A check whose enforcement mechanism is absent cannot fail on the system's merits; it can only report the
// instrument. Without this tier a NetworkPolicy check on a cluster that does not enforce NetworkPolicy ends
// `failed` (the policy exists, the connection still works) - a statement about the cluster, filed under the
// system under test. With it the check is not executed and ends `not_applicable`, carrying the evidence.
//
// THREE RESULTS, and the third is the one that keeps the first two honest:
//
//	holds   the positive control (the path is alive) AND the negative control (a denied path is refused) held.
//	absent  the positive control held and the negative control was NOT refused: the mechanism does nothing here.
//	broken  the positive control failed, the probe could not be set up, or the negative control could not be
//	        trusted. The INSTRUMENT is broken. Dependents end `errored` - never not_applicable, because a
//	        broken probe that excused checks would be a way to skip any check by breaking the probe.
//
// Authors cannot supply probe code: a precondition names a BUILT-IN kind from a closed list
// (scenario.PreconditionKinds). Every probe runs ONLY in the executor's OWN instance namespace, with throwaway
// pods and NetworkPolicies it always removes, and never touches the system-under-test namespace.
package precondition

import (
	"fmt"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The three results.
const (
	Holds  = "holds"
	Absent = "absent"
	Broken = "broken"
)

// Result is one precondition's outcome. It is executor-attested (the executor ran the probe in its own
// namespace) and BOUND by the evidence bundle hash and the certificate: every field below is hashed.
type Result struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Verdict string `json:"verdict"`
	// Positive and Negative are the two controls' observations, in words a reader can check: "connected on
	// attempt 1", "not connected in 5 attempts (refused or dropped)", "not run: <why>". Never an address.
	Positive string `json:"positive"`
	Negative string `json:"negative"`
	// Residue is set only when the probe could not remove something it created ("may remain: ..."). It never
	// changes the verdict; absence of cleanup must never be silent.
	Residue string `json:"residue,omitempty"`
}

// Reason is the one-line statement dependents carry: the id, the verdict and both observations.
func (r Result) Reason() string {
	return fmt.Sprintf("precondition %s (%s) %s: positive control: %s; negative control: %s", r.ID, r.Kind, r.Verdict, r.Positive, r.Negative)
}

// Collect gathers the preconditions a set declares: the union over its files, each id once, in id order.
// Two files declaring one id with different kinds is a conflict the set cannot resolve: it is returned as a
// problem (and the run ends those dependents `errored`), never silently resolved by taking one.
func Collect(decls [][]scenario.Precondition) (all []scenario.Precondition, conflicts map[string]string) {
	byID := map[string]string{}
	conflicts = map[string]string{}
	for _, d := range decls {
		for _, p := range d {
			if k, ok := byID[p.ID]; ok && k != p.Kind {
				conflicts[p.ID] = fmt.Sprintf("precondition %q is declared as both %q and %q in this set", p.ID, k, p.Kind)
				continue
			}
			byID[p.ID] = p.Kind
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		all = append(all, scenario.Precondition{ID: id, Kind: byID[id]})
	}
	return all, conflicts
}

// Joined renders a list of ids for a message.
func Joined(ids []string) string { return strings.Join(ids, ", ") }
