package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Spec 26 P1 (A1, observe only): the per-scenario sandbox_policy block.

// goldenRowWithoutSandboxPolicy is this row's json.Marshal output on b45d824, the commit before the
// field existed (generated there, not hand-written). Invariant 1: with no block, report.json and the
// evidence hash (runner.evidenceHash marshals the same struct) stay byte-identical.
const goldenRowWithoutSandboxPolicy = `{"id":"S-1","status":"failed","duration_ms":1200,"correlation_id":"tr-r-S-1-ab","failure":{"observed":"HTTP 500"},"req_success":1,"req_failed":2,"assertions_enforced_count":3}`

func goldenRow() ScenarioResult {
	return ScenarioResult{ID: "S-1", Status: "failed", DurationMs: 1200, CorrelationID: "tr-r-S-1-ab",
		Failure: &Failure{Observed: "HTTP 500"}, ReqSuccess: 1, ReqFailed: 2, AssertionsEnforcedCount: 3,
		LoadWindowStartMs: 5, LoadWindowEndMs: 9}
}

func TestScenarioResult_BytesUnchangedWithoutSandboxPolicy(t *testing.T) {
	row := goldenRow()
	// The runtime-only window stamps are set, as the run loop sets them on every row: they must not
	// reach the bytes either.
	row.WindowStart = time.Unix(1775014130, 0)
	row.WindowEnd = time.Unix(1775014140, 0)
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != goldenRowWithoutSandboxPolicy {
		t.Fatalf("a row with no sandbox_policy changed its bytes:\n got  %s\n want %s", b, goldenRowWithoutSandboxPolicy)
	}
}

func TestScenarioResult_NoSandboxPolicyKeyWhenOff(t *testing.T) {
	b, err := json.Marshal(goldenRow())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sandbox_policy", "WindowStart", "WindowEnd", "window_start", "window_end"} {
		if strings.Contains(string(b), k) {
			t.Errorf("%q must not appear in a row with the feature off: %s", k, b)
		}
	}
}

// Invariant 3: coverage and coverage_reason are always present when the block is, denied_count too
// (on a measured block a 0 is a measured zero, not an absent field), and events is an array.
func TestSandboxPolicy_CoverageFieldsAlwaysSerialized(t *testing.T) {
	zero := 0
	b, err := json.Marshal(SandboxPolicy{Coverage: CoverageComplete, DeniedCount: &zero, Events: []SandboxPolicyEvent{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"source":`, `"sandbox":`, `"coverage":"complete"`, `"coverage_reason":`, `"denied_count":0`, `"events":[]`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("block JSON must carry %s: %s", k, b)
		}
	}
	for _, k := range []string{"events_omitted", "shared_with", `"window"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s is optional and must be absent when empty: %s", k, b)
		}
	}
}

// Spec 26 §4 and §7: an unavailable block never reads as "no denials". Its count is absent as a
// value but present as a key: "denied_count":null, never a 0 that reads as a measured zero.
func TestSandboxPolicy_UnsetCountSerialisesNull(t *testing.T) {
	b, err := json.Marshal(SandboxPolicy{Coverage: CoverageUnavailable, CoverageReason: "x", Events: []SandboxPolicyEvent{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"denied_count":null`) {
		t.Fatalf("a block with no count must say \"denied_count\":null: %s", b)
	}
}

func TestSandboxPolicyEvent_FieldNames(t *testing.T) {
	b, err := json.Marshal(SandboxPolicyEvent{Time: "t", Class: "NET:OPEN", Action: "Denied", Target: "httpbin.org:443",
		Process: "/usr/bin/curl", Rule: "-", Reason: "no matching policy"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"t","class":"NET:OPEN","action":"Denied","target":"httpbin.org:443","process":"/usr/bin/curl","rule":"-","reason":"no matching policy"}`
	if string(b) != want {
		t.Fatalf("event JSON = %s, want the spec 26 §4 shape %s", b, want)
	}
}
