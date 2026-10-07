package scenario

import "fmt"

// VR12-M5 (V30-003 §1) — `**ID**` UNIQUENESS. ⛔ THE SEAM ONLY: THE LOCUS IS AN OPEN OWNER QUESTION.
//
// The format half of M5 already exists and passes (`idFormat` in validate.go). Uniqueness cannot
// live there: `Validate(text)` takes MARKDOWN ALONE, so it can never see a second file, and the
// question is undecidable at that boundary by construction.
//
// ⛔ IT IS LEFT UNWIRED ON PURPOSE. "Unique" has THREE incompatible meanings and the product owner
// has not chosen, so picking one here would silently decide what the word means:
//
//	author__write_scenario on the CONTROL plane, against the `scenarios` registry
//	    ⇒ scope is PER INSTANCE — that is the key WriteScenario already carries
//	    (InstanceID, WorkspaceID; internal/control/cloudtools.go).
//	toolcore.WriteScenario's disk twin, against scenariosDir
//	    ⇒ scope is PER FOLDER.
//	the example-suite walk
//	    ⇒ scope is THE REPO, and only for what we ship.
//
// A colleague's pack that legitimately reuses an id from ours is fine under the first two and
// refused by the third. So this function is a PURE predicate with an INJECTED lookup: whichever
// locus is chosen becomes a three-line call site, and nothing about the rule changes.
//
// ⚠ NOTHING CALLS IT YET. That is the honest state, not an oversight — see the QA record's open
// items. A test proves the predicate works so the ruling costs no further build.

// CheckIDUnique returns a refusal message, or "" when the id is free.
//
// lookup answers "does this id already exist, and where?" — the caller supplies it, and the CALLER
// is what decides what "the catalogue" means.
func CheckIDUnique(id string, lookup func(string) (where string, exists bool)) string {
	if id == "" || lookup == nil {
		return ""
	}
	where, exists := lookup(id)
	if !exists {
		return ""
	}
	return fmt.Sprintf("the scenario id %q is already used by %s — an id is how a run, a report row, "+
		"a Grafana series and a triage conversation refer to the same test, so two scenarios sharing "+
		"one makes every one of those ambiguous (V30-003 M5)", id, where)
}
