package runner

// INT-026 (wave 1.6): the registration must carry a suite_version, or the version-drift chip can never
// render.
//
// `SuiteVersion` was plumbed END TO END and never assigned: it exists on the wire
// (federation.RegisterRequest, wire.go:24), the DB column is written and read back
// (store/access.go:305,359,372,400), and the instance view renders it (instanceview.go:38) — but no
// caller ever set it. Measured live: every one of the seven registered instances, and every row in the
// database, carried the empty string.
//
// UC044 lists "runner + suite versions" among the fields get_executor_status must return, and UC102.4
// wants a drift chip "when the instance's SUITE version is older than the newest registered in the
// workspace". With every value empty that comparison is silently vacuous — the chip is absent not
// because there is no drift, but because drift is uncomputable. And the estate WAS drifted at the time
// of measurement: social-aks-v1 on m3-iii35 against six on m3-iii34, exactly the condition the chip
// exists to surface, invisible.
//
// ROUND-2 CORRECTION (INT-029, 2026-08-08): this file is still correct but was NOT SUFFICIENT. The
// register payload only reaches the CP on a FIRST onboard; afterwards register-on-start takes
//   403 {"error":"re-register requires the machine JWT of the registered identity"}
// — measured on COMPOSE, so the refusal is not k8s-specific as PollRequest's comment implies. The
// carrier that always works is the POLL; see poll_suiteversion_test.go. Both paths now set the field.
//
// JUDGEMENT CALL, reversible: in M3 the executor binary and the test suite ship as ONE image, so the
// suite version IS the runner version — populating it duplicates a value rather than adding a new fact.
// The alternative is to delete the field and re-specify the chip against runner_version. Populating is
// the smaller and more reversible change (the wire field, the column, the view and two UCs all name
// suite_version), and it makes UC044 and UC102 correct today. The owner can reverse it.

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func TestRegisterPayload_CarriesASuiteVersion(t *testing.T) {
	fed := FedConfig{InstanceID: "probe-1", Tier: "k3d", RunnerVersion: "0.3.0+m3-iii36"}

	req := registerRequestFor(fed, "pubkey-b64", "", nil, nil, nil)

	if req.SuiteVersion == "" {
		t.Error("RegisterRequest.SuiteVersion is empty. It is plumbed end to end — wire.go:24, the DB " +
			"column, and instanceview.go:38 — but nothing assigned it, so every instance stored \"\" and " +
			"UC102's version-drift chip could never compute (INT-026)")
	}
	if req.RunnerVersion == "" {
		t.Error("RegisterRequest.RunnerVersion is empty — the guard that proves this test is wired up")
	}
}

func TestRegisterPayload_SuiteAndRunnerAgreeWhileTheyAreOneImage(t *testing.T) {
	// M3 ships one binary in one image. If these ever diverge, that is a real change in what is being
	// shipped — this test is what makes such a change deliberate rather than accidental.
	fed := FedConfig{InstanceID: "probe-2", Tier: "compose", RunnerVersion: "0.3.0+m3-iii36"}
	req := registerRequestFor(fed, "pk", "", nil, nil, nil)
	if req.SuiteVersion != req.RunnerVersion {
		t.Errorf("suite=%q runner=%q — they ship as ONE image in M3; a divergence here needs a "+
			"deliberate decision, not an accident", req.SuiteVersion, req.RunnerVersion)
	}
}

func TestRegisterPayload_EmptyVersionStaysEmpty(t *testing.T) {
	// A build with no stamp must not have a version invented for it — the same honesty INT-001 is about.
	req := registerRequestFor(FedConfig{InstanceID: "probe-3"}, "pk", "", nil, nil, nil)
	if req.SuiteVersion != "" {
		t.Errorf("SuiteVersion = %q, want empty when the runner has no version either", req.SuiteVersion)
	}
	_ = federation.ProtocolVersion
}
