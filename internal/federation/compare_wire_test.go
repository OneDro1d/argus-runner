package federation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ARGUS-CMP-2: every additive wire field of the compare mission lands here at once, omitempty, so the
// later PRs never conflict in wire.go. With them all empty the bytes must be what they were.

func baselinePush() ResultsPush {
	now := time.Unix(1_700_000_000, 0).UTC()
	return ResultsPush{
		RunID: "run_1", RunRequestID: "rr_1", Scope: "full", Status: "completed",
		Tallies:     Tallies{Passed: 2, Failed: 1, Total: 3},
		Scenarios:   []ScenarioResult{{ID: "S1", Outcome: "passed", DurationMs: 12, Summary: "ok", EvidenceHash: "ee"}},
		Annotations: Annotations{Commit: "abc"},
		SetHash:     "deadbeef", StartedAt: &now, FinishedAt: &now, DurationMs: 50,
		EvidenceBundleHash: "bb", Outcome: "passed",
	}
}

func TestCompareWire_EmptyFieldsLeaveTheBytesUnchanged(t *testing.T) {
	const wantPush = `{"run_id":"run_1","run_request_id":"rr_1","scope":"full","status":"completed",` +
		`"tallies":{"passed":2,"failed":1,"errored":0,"total":3},` +
		`"scenarios":[{"id":"S1","outcome":"passed","duration_ms":12,"summary":"ok","evidence_hash":"ee"}],` +
		`"annotations":{"commit":"abc"},"set_hash":"deadbeef",` +
		`"started_at":"2023-11-14T22:13:20Z","finished_at":"2023-11-14T22:13:20Z","duration_ms":50,` +
		`"evidence_bundle_hash":"bb","outcome":"passed"}`
	b, err := json.Marshal(baselinePush())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantPush {
		t.Fatalf("ResultsPush bytes changed:\n got %s\nwant %s", b, wantPush)
	}
	const wantRun = `{"run_request_id":"rr","scope":"full","selection":{},"annotations":{},"set_hash":"h","scenarios":null}`
	rb, err := json.Marshal(RunAssignment{RunRequestID: "rr", Scope: "full", SetHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if string(rb) != wantRun {
		t.Fatalf("RunAssignment bytes changed:\n got %s\nwant %s", rb, wantRun)
	}
}

func TestCompareWire_NewFieldsRoundTripUnderTheirOwnKeys(t *testing.T) {
	p := baselinePush()
	p.Outputs = json.RawMessage(`[{"scenario_id":"S1","v":1}]`)
	p.OutputsRoot = "root"
	p.EnvFingerprint = "fp"
	b, _ := json.Marshal(p)
	for _, want := range []string{`"outputs":[{"scenario_id":"S1","v":1}]`, `"outputs_root":"root"`, `"env_fingerprint":"fp"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	var back ResultsPush
	if err := json.Unmarshal(b, &back); err != nil || back.OutputsRoot != "root" || back.EnvFingerprint != "fp" || string(back.Outputs) != `[{"scenario_id":"S1","v":1}]` {
		t.Fatalf("round trip: %+v %v", back, err)
	}

	a := RunAssignment{RunRequestID: "rr", Scope: "full", SetHash: "h", Mode: "compare", CompareTarget: "graph",
		CompareScenarios: []ScenarioPayload{{Path: "a.md", Body: "x"}}}
	ab, _ := json.Marshal(a)
	for _, want := range []string{`"compare_scenarios":[{"path":"a.md","body":"x"}]`, `"compare_target":"graph"`, `"mode":"compare"`} {
		if !strings.Contains(string(ab), want) {
			t.Errorf("missing %s in %s", want, ab)
		}
	}
	if !strings.Contains(string(ab), `"scenarios":null`) {
		t.Errorf("a compare assignment leaves scenarios empty (design 11.2): %s", ab)
	}
	// an old executor ignores the unknown keys
	var old struct {
		Scenarios []ScenarioPayload `json:"scenarios"`
	}
	if err := json.Unmarshal(ab, &old); err != nil || len(old.Scenarios) != 0 {
		t.Fatalf("an old reader must see an empty scenarios list: %+v %v", old, err)
	}
}

// Nothing is added to Annotations: it reaches alerts, the public page and chains.
func TestCompareWire_AnnotationsUntouched(t *testing.T) {
	b, _ := json.Marshal(Annotations{Commit: "c", PR: "p", Label: "l"})
	if string(b) != `{"commit":"c","pr":"p","label":"l"}` {
		t.Fatalf("Annotations = %s", b)
	}
}
