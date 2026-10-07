package runner

// unsealed_test.go — AC-4/AC-4b: the runner's own refusal of a `final`/`scheduled`
// assignment whose scenarios arrive UNSEALED — defense in depth beside the control plane's own
// PickupNext refusal (internal/control/store), same posture as mode_digest_test.go's digest check:
// checked before any materialization or execution, so a control plane that skipped its own check
// still cannot make this executor run a certifying set that crossed in the clear.

import (
	"context"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

const unsealedTestDigest = "sha256:" + "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

// TestNewRunFunc_RefusesUnsealedFinalAssignment: a `final` assignment carrying a clear-body scenario
// (Sealed == nil) must be refused before any materialization or execution is attempted — the same
// clean-refusal proof mode_digest_test.go uses (a zero-value ExecConfig would fail loudly on any real
// I/O).
func TestNewRunFunc_RefusesUnsealedFinalAssignment(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	a := &federation.RunAssignment{
		Mode: "final", ArtifactDigest: unsealedTestDigest,
		Scenarios: []federation.ScenarioPayload{{Path: "http-ingestion/UNSEALED-001.md", Body: "clear EXPECT text"}},
	}
	_, err := run(context.Background(), a)
	if err == nil {
		t.Fatal("a final assignment with an unsealed scenario must be refused; got a nil error")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("the refusal must name \"sealed\"; got: %v", err)
	}
}

// TestNewRunFunc_RefusesUnsealedScheduledAssignment: the same refusal for `scheduled` (AC-12) —
// build runs are unchanged (mode_digest_test.go's own negative control already covers that).
func TestNewRunFunc_RefusesUnsealedScheduledAssignment(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	a := &federation.RunAssignment{
		Mode:      "scheduled",
		Scenarios: []federation.ScenarioPayload{{Path: "http-ingestion/UNSEALED-002.md", Body: "clear EXPECT text"}},
	}
	_, err := run(context.Background(), a)
	if err == nil {
		t.Fatal("a scheduled assignment with an unsealed scenario must be refused; got a nil error")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("the refusal must name \"sealed\"; got: %v", err)
	}
}

// TestNewRunFunc_RefusesUnsealedRehearsalAssignment: — a `rehearsal` runs the DRAFT
// certification set, so its EXPECT must not cross in the clear either.
func TestNewRunFunc_RefusesUnsealedRehearsalAssignment(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	a := &federation.RunAssignment{
		Mode:      "rehearsal",
		Scenarios: []federation.ScenarioPayload{{Path: "certification/http-ingestion/UNSEALED-003.md", Body: "clear EXPECT text"}},
	}
	_, err := run(context.Background(), a)
	if err == nil {
		t.Fatal("a rehearsal assignment with an unsealed scenario must be refused; got a nil error")
	}
	if !strings.Contains(err.Error(), "rehearsal") || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("the refusal must name the mode and \"sealed\"; got: %v", err)
	}
}

// TestNewRunFunc_FinalWithSealedScenarios_PassesTheUnsealedCheck is the negative control: a `final`
// assignment whose scenarios ALL carry a Sealed body must not be refused by this check — it fails
// later, on the missing SUT config, never here.
func TestNewRunFunc_FinalWithSealedScenarios_PassesTheUnsealedCheck(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	a := &federation.RunAssignment{
		Mode: "final", ArtifactDigest: unsealedTestDigest,
		Scenarios: []federation.ScenarioPayload{{Path: "http-ingestion/SEALED-001.md", Sealed: &federation.SealedBody{}}},
	}
	_, err := run(context.Background(), a)
	if err != nil && strings.Contains(err.Error(), "sealed") {
		t.Errorf("a fully-sealed final assignment must not be refused for being unsealed; got: %v", err)
	}
}
