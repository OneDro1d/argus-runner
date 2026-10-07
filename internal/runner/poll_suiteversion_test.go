package runner

// INT-026, ROUND 2 (INT-029): the fix was in the wrong place, and the codebase already said so.
//
// Round 1 put SuiteVersion on the REGISTER payload. Measured live on 2026-08-08 after re-onboarding
// orderservice-compose onto the fixed image:
//
//   executor log:  register-on-start failed (will self-heal via polling): register: 403
//                  {"error":"re-register requires the machine JWT of the registered identity"}
//   CP /api/instances:  runner_version = "0.3.0+m3-iii36"   suite_version = ""
//
// So the executor WAS running the fixed binary, and runner_version DID update — via the POLL — while
// suite_version stayed empty because it rode only on a register call that never succeeds again after
// the first onboard.
//
// PollRequest's own comment (federation/wire.go) documents this exact class:
//
//   "These used to ride ONLY on register-on-start, and on the k8s tiers that call is REFUSED ... The
//    poll then 'self-heals' liveness while carrying none of the register-only fields — so ANY field
//    only the executor knows could never reach the control plane ... Moving them to the poll fixes the
//    CLASS, not just U7."
//
// U7 hit it, diagnosed it, and moved ProductDir/TestDir/OnboardHost to the poll. I then added a new
// register-only field and walked into the same wall. Two things follow, and the second is the finding:
//
//  1. SuiteVersion belongs on the POLL, like every other executor-only fact.
//  2. The comment says "on the k8s tiers". This was measured on COMPOSE — so the refusal is not
//     k8s-specific, and any future register-only field is broken on EVERY tier after the first
//     onboard. That is INT-029.

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func TestPollPayload_CarriesTheSuiteVersion(t *testing.T) {
	// THE defect. Register cannot deliver this after the first onboard; the poll is the only carrier
	// that runs continuously and is always authorised by the machine JWT.
	req := pollRequestFor("0.3.0+m3-iii36", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)

	if req.SuiteVersion == "" {
		t.Error("PollRequest.SuiteVersion is empty. It rode only on register-on-start, which returns 403 " +
			"once the instance is already registered — measured live on compose, not just k8s — so the " +
			"value never reaches the CP and UC102's drift chip stays uncomputable (INT-026/INT-029)")
	}
	if req.RunnerVersion == "" {
		t.Error("RunnerVersion empty — the guard proving this test is wired up")
	}
}

func TestPollPayload_SuiteAndRunnerAgreeWhileTheyAreOneImage(t *testing.T) {
	req := pollRequestFor("0.3.0+m3-iii36", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)
	if req.SuiteVersion != req.RunnerVersion {
		t.Errorf("suite=%q runner=%q — one image in M3; a divergence must be deliberate", req.SuiteVersion, req.RunnerVersion)
	}
}

func TestPollPayload_UnstampedStaysEmpty(t *testing.T) {
	// An unstamped build must not have a version invented for it — the honesty INT-001 is about.
	if got := pollRequestFor("", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil).SuiteVersion; got != "" {
		t.Errorf("SuiteVersion = %q, want empty when the runner has no version either", got)
	}
	_ = federation.ProtocolVersion
}
