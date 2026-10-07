// Package skills is the ONE definition of which agent skills onboarding installs, per hat.
//
// ⚠ WHAT THIS PACKAGE IS *NOT* FOR ANY MORE, and the comment here said otherwise until 2026-08-21.
//
// It was written because VR8-S1 was going to make TEARDOWN remove skills by name, which would have
// put a second copy of the list in Go beside the one in bash. The owner has since taken that whole
// topic out of the round:
//
//	"lets move it out from the current build scope with remark that this whole topic must be
//	 additionally analyzed because one test or prod agent folder can be used by multiple
//	 instances" ... "teardown do not remove any previously installed skills at all"
//
// So nothing removes skills, and cmd/argus/router_wire.go no longer imports this package. What
// remains is a DOCUMENTED, MACHINE-CHECKED statement of what the installer installs -- which is
// exactly what the deferred analysis will need, and it costs nothing to keep true in the meantime.
//
// drift_test.go PARSES onboard.sh's real install_skills call sites and fails the build if the two
// disagree. That check is no longer load-bearing for a deletion; it is load-bearing for this file
// still describing reality. Mutation-tested both directions: a name added here alone fails the
// test, and so does a name added to onboard.sh alone.
//
// ⚠ `argus skills --hat <hat>` exposes the same list to a shell, and an earlier version of this
// comment claimed onboarding READS IT BACK. It does not - onboard.sh still names the skills
// literally. The claim was false in the round whose whole theme is that the code must not lie, and
// an adversary found it. Making it true would mean two container round-trips at step 7 and a new
// runtime failure mode (what installs if the call fails?), to foreclose at RUN time a drift the
// test already forecloses at BUILD time. The test is the guarantee; the command is a convenience.
//
// ⛔ THE LIST IS NOT `ls skills/`. The kit ships `ai-code-review`, which NEITHER install site
// installs. Deriving the removal list from the directory would delete a skill Argus never put
// there — which is V26-010 restored in a new place. The list is what is INSTALLED, stated
// explicitly, and that is the whole point of this package.
package skills

import "github.com/OneDro1d/argus-runner/internal/role"

// runner + triage go to both hats: a product agent runs scenarios and diagnoses failures, it just
// never authors them.
const (
	scenarioRunner = "scenario-runner"
	failureTriage  = "failure-triage"
	// scenarioAuthor is TEST-HAT ONLY, and that asymmetry is VR-R4's hat separation expressed in
	// files: a product folder that could author scenarios could write its own expectations.
	scenarioAuthor = "scenario-author"
)

// Installed returns the skills onboarding installs into a folder wearing this hat.
//
// The returned slice is a fresh copy: a caller that sorted or truncated a shared backing array
// would change what every later caller sees.
//
// ⚠ IT USED TO SAY "what teardown deletes". Teardown deletes nothing under .claude any more
// (owner ruling 2026-08-21), so that reason no longer exists. The copy is still right; the
// justification had to change with the facts.
//
// An unrecognised hat returns NIL, not a default. A removal driven by a guess is exactly the class
// of bug this package exists to prevent — better to remove nothing and say so.
func Installed(hat role.Role) []string {
	switch hat {
	case role.Product:
		return []string{scenarioRunner, failureTriage}
	case role.Test:
		return []string{scenarioRunner, failureTriage, scenarioAuthor}
	default:
		return nil
	}
}
