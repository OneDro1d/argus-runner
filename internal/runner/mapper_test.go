package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func TestMapReport_StripsEvidenceAndKeepsIndex(t *testing.T) {
	exp := "SECRET_EXPECTED_VALUE"
	rep := &report.Report{
		Project: "orders",
		Summary: report.Summary{Total: 3, Passed: 1, Failed: 1, Errored: 1},
		Layers: []report.Layer{{
			Layer: "HTTP Ingestion",
			Scenarios: []report.ScenarioResult{
				{ID: "S1", Status: "passed", DurationMs: 10},
				{ID: "S2", Status: "failed", DurationMs: 20, CorrelationID: "tr-secret-corr",
					Failure: &report.Failure{Expected: &exp, Observed: "SECRET_OBSERVED_VALUE"}},
				{ID: "S3", Status: "error", DurationMs: 5},
			},
		}},
	}

	ann := federation.Annotations{Commit: "abc123", PR: "42", Label: "nightly"}
	push := mapReport(rep, "run-1", "rr-1", "full", "sethash-1", "https://g/x", ann, time.Time{}, time.Time{}, "sha256:digest-1")

	// index preserved
	if push.RunID != "run-1" || push.RunRequestID != "rr-1" || push.Scope != "full" || push.SetHash != "sethash-1" {
		t.Fatalf("index fields lost: %+v", push)
	}
	// annotations (metadata, not evidence) ECHO through to the push (UC040).
	if push.Annotations != ann {
		t.Errorf("annotations not echoed: got %+v want %+v", push.Annotations, ann)
	}
	if push.Status != "failed" {
		t.Errorf("status = %q, want failed (a failure present)", push.Status)
	}
	if push.Tallies != (federation.Tallies{Passed: 1, Failed: 1, Errored: 1, Total: 3}) {
		t.Errorf("tallies = %+v, want {1,1,1,3}", push.Tallies)
	}
	if len(push.Scenarios) != 3 {
		t.Fatalf("scenarios = %d, want 3", len(push.Scenarios))
	}
	wantOutcome := map[string]string{"S1": "passed", "S2": "failed", "S3": "errored"}
	for _, sc := range push.Scenarios {
		if wantOutcome[sc.ID] != sc.Outcome {
			t.Errorf("%s outcome = %q, want %q", sc.ID, sc.Outcome, wantOutcome[sc.ID])
		}
	}

	// THE LOCALITY GUARANTEE: no evidence crosses. Serialize the whole push and scan for the secrets +
	// any evidence-shaped key.
	b, _ := json.Marshal(push)
	s := string(b)
	for _, secret := range []string{"SECRET_EXPECTED_VALUE", "SECRET_OBSERVED_VALUE", "tr-secret-corr"} {
		if strings.Contains(s, secret) {
			t.Errorf("results push leaked in-env evidence %q: %s", secret, s)
		}
	}
	var m any
	_ = json.Unmarshal(b, &m)
	assertNoEvidenceKeys(t, m)
}

// TestMapReport_EvidenceHashPerScenario is AC-6 item 1: each scenario carries a sha256 evidence_hash
// over the runner's own in-env evidence record (report.ScenarioResult) — a scenario that differs in
// its evidence (even without changing Status) must hash differently, and the same evidence must hash
// the same way every time (D-FED.4: evidence never crosses, only its digest, and the digest must be
// reproducible for the idempotent-retry guarantee AC-6 item 3 depends on).
func TestMapReport_EvidenceHashPerScenario(t *testing.T) {
	exp := "SECRET_EXPECTED_VALUE"
	rep := &report.Report{
		Summary: report.Summary{Total: 2, Passed: 1, Failed: 1},
		Layers: []report.Layer{{
			Layer: "HTTP Ingestion",
			Scenarios: []report.ScenarioResult{
				{ID: "S1", Status: "passed", DurationMs: 10},
				{ID: "S2", Status: "failed", DurationMs: 20,
					Failure: &report.Failure{Expected: &exp, Observed: "SECRET_OBSERVED_VALUE"}},
			},
		}},
	}
	push := mapReport(rep, "run-eh", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")

	byID := map[string]federation.ScenarioResult{}
	for _, sc := range push.Scenarios {
		byID[sc.ID] = sc
	}
	s1, s2 := byID["S1"], byID["S2"]
	if s1.EvidenceHash == "" || s2.EvidenceHash == "" {
		t.Fatalf("evidence_hash empty: S1=%q S2=%q", s1.EvidenceHash, s2.EvidenceHash)
	}
	if len(s1.EvidenceHash) != 64 { // hex(sha256) = 64 chars
		t.Errorf("S1 evidence_hash length = %d, want 64 (hex sha256): %q", len(s1.EvidenceHash), s1.EvidenceHash)
	}
	if s1.EvidenceHash == s2.EvidenceHash {
		t.Errorf("S1 and S2 have different evidence but the same hash: %q", s1.EvidenceHash)
	}

	// same input, computed again, must reproduce the SAME hash (idempotent-retry requirement).
	again := mapReport(rep, "run-eh", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	for _, sc := range again.Scenarios {
		if sc.ID == "S1" && sc.EvidenceHash != s1.EvidenceHash {
			t.Errorf("S1 evidence_hash not reproducible: %q vs %q", sc.EvidenceHash, s1.EvidenceHash)
		}
	}
}

// TestMapReport_EvidenceBundleHash is AC-6 item 2: evidence_bundle_hash is sha256 over the
// per-scenario evidence hashes IN SCENARIO-ID ORDER, independent of the order scenarios were
// executed/reported in (layers group scenarios; the bundle must not depend on that grouping).
func TestMapReport_EvidenceBundleHash(t *testing.T) {
	rep := &report.Report{
		Summary: report.Summary{Total: 2, Passed: 2},
		Layers: []report.Layer{{
			Layer: "Z",
			Scenarios: []report.ScenarioResult{
				{ID: "Zeta", Status: "passed"},
				{ID: "Alpha", Status: "passed"},
			},
		}},
	}
	push := mapReport(rep, "run-bh", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	if push.EvidenceBundleHash == "" {
		t.Fatal("evidence_bundle_hash is empty")
	}

	ids := make([]string, 0, len(push.Scenarios))
	byID := make(map[string]string, len(push.Scenarios))
	for _, sc := range push.Scenarios {
		ids = append(ids, sc.ID)
		byID[sc.ID] = sc.EvidenceHash
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(byID[id]))
	}
	want := hex.EncodeToString(h.Sum(nil))
	if push.EvidenceBundleHash != want {
		t.Errorf("evidence_bundle_hash = %q, want %q (sha256 of per-scenario hashes in id order)", push.EvidenceBundleHash, want)
	}
}

// TestMapReport_OutcomeIsTheRunVerdict is AC-6 item 2: Outcome carries the run's pass/fail verdict —
// distinct from Status, which is the push's delivery/lifecycle state — and is empty until the run is
// terminal.
func TestMapReport_OutcomeIsTheRunVerdict(t *testing.T) {
	passing := &report.Report{Summary: report.Summary{Total: 1, Passed: 1}}
	if p := mapReport(passing, "run-p", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, ""); p.Outcome != "passed" {
		t.Errorf("all-passing run outcome = %q, want %q", p.Outcome, "passed")
	}
	failing := &report.Report{Summary: report.Summary{Total: 2, Passed: 1, Failed: 1}}
	if p := mapReport(failing, "run-f", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, ""); p.Outcome != "failed" {
		t.Errorf("a failing run outcome = %q, want %q", p.Outcome, "failed")
	}
	empty := &report.Report{Summary: report.Summary{Total: 0}}
	if p := mapReport(empty, "run-z", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, ""); p.Outcome != "failed" {
		t.Errorf("a zero-scenario run outcome = %q, want %q (never a silent green)", p.Outcome, "failed")
	}
}

// TestMapReport_ArtifactDigestEchoed is AC-6: the digest a `final` assignment carried rides the push
// unchanged, so RecordResults can bind the run's verdict to it without a second lookup.
func TestMapReport_ArtifactDigestEchoed(t *testing.T) {
	rep := &report.Report{Summary: report.Summary{Total: 1, Passed: 1}}
	const digest = "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	p := mapReport(rep, "run-d", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, digest)
	if p.ArtifactDigest != digest {
		t.Errorf("ArtifactDigest = %q, want %q", p.ArtifactDigest, digest)
	}
}

// A run that executed ZERO scenarios must never be a green ledger row — at ANY scope.
//
// OWNER RULING 2026-07-22, superseding the earlier carve-out this test used to assert ("a
// full-scope run against a legitimately empty instance stays completed"): if there is nothing
// to run, the run cannot happen. Runs are now refused before execution and before enqueue, so a
// 0-total row arriving here means something went wrong upstream and must be visible, not green.
func TestMapReport_ZeroScenariosIsNeverGreen(t *testing.T) {
	empty := &report.Report{Summary: report.Summary{Total: 0}}
	for _, scope := range []string{"single", "layer", "tag", "full"} {
		p := mapReport(empty, "run-z", "", scope, "", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
		// Must be a VALID, non-green run_ledger status. The run_ledger CHECK is
		// ('running','completed','failed') — "errored" would violate it and drop the whole push — so the
		// correct non-green value is "failed".
		if p.Status != "failed" {
			t.Errorf("scope %q zero-scenario run mapped to %q, want \"failed\" (valid non-green ledger status)", scope, p.Status)
		}
	}
	// Guard the obvious regression in the other direction: a real run with results must still be
	// able to come back green, or this rule would have made every run red.
	ok := &report.Report{Summary: report.Summary{Total: 3, Passed: 3}}
	if p := mapReport(ok, "run-ok", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, ""); p.Status != "completed" {
		t.Errorf("a real all-passing run mapped to %q, want completed", p.Status)
	}
}

// AC-11: a run with no failure but at least one DEGRADED scenario maps to push.Status="degraded"
// — never "completed" (that would hide the distress signal) and never "errored" (the run_ledger
// CHECK has no such value at all; this exact confusion is what F7 warns against, mapper.go:69).
func TestMapReport_DegradedStatusPropagates(t *testing.T) {
	rep := &report.Report{
		Project: "orders",
		Summary: report.Summary{Total: 2, Passed: 1, Degraded: 1},
		Layers: []report.Layer{{
			Layer: "HTTP Ingestion",
			Scenarios: []report.ScenarioResult{
				{ID: "S1", Status: "passed", DurationMs: 10},
				{ID: "S2", Status: report.StatusDegraded, DurationMs: 20,
					Load: &report.LoadStats{Degraded: true, DegradedNote: "pod throttled"}},
			},
		}},
	}
	push := mapReport(rep, "run-1", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, "")

	if push.Status != "degraded" {
		t.Fatalf("push.Status = %q, want degraded", push.Status)
	}
	if push.Tallies.Degraded != 1 {
		t.Errorf("Tallies.Degraded = %d, want 1", push.Tallies.Degraded)
	}
	// The verdict (AC-6's Outcome) carries the distress too: never "passed" for a degraded run.
	if push.Outcome != "degraded" {
		t.Errorf("push.Outcome = %q, want degraded", push.Outcome)
	}
	var s2 *federation.ScenarioResult
	for i := range push.Scenarios {
		if push.Scenarios[i].ID == "S2" {
			s2 = &push.Scenarios[i]
		}
	}
	if s2 == nil || s2.Outcome != "degraded" {
		t.Fatalf("S2 outcome = %+v, want degraded", s2)
	}
	// The degraded reason (SUT-side evidence) does not cross the wire — only the generic summary.
	if s2.Summary == "" || strings.Contains(s2.Summary, "pod throttled") {
		t.Errorf("S2 summary must be generic, not echo the in-env note: %q", s2.Summary)
	}
}

// AC-11: DEGRADED never overrides a real failure — a run with both a failure and a degraded
// scenario is still "failed", the worse word.
func TestMapReport_FailedOutranksDegraded(t *testing.T) {
	rep := &report.Report{
		Summary: report.Summary{Total: 2, Failed: 1, Degraded: 1},
		Layers: []report.Layer{{Layer: "L", Scenarios: []report.ScenarioResult{
			{ID: "S1", Status: "failed"},
			{ID: "S2", Status: report.StatusDegraded},
		}}},
	}
	push := mapReport(rep, "run-2", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	if push.Status != "failed" {
		t.Fatalf("push.Status = %q, want failed (outranks degraded)", push.Status)
	}
}

func assertNoEvidenceKeys(t *testing.T, v any) {
	t.Helper()
	forbidden := map[string]bool{"failure": true, "expected": true, "observed": true, "correlation_id": true,
		"steps": true, "mcp_envelope": true, "logs": true, "sagas": true, "verdict": true}
	var walk func(any)
	walk = func(x any) {
		switch t2 := x.(type) {
		case map[string]any:
			for k, vv := range t2 {
				if forbidden[k] {
					t.Errorf("push carries forbidden evidence key %q", k)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t2 {
				walk(vv)
			}
		}
	}
	walk(v)
}

// Spec 26 P1 (A1): the sandbox_policy block is in-env evidence. It is folded into the scenario's
// evidence_hash like every other evidence field, it never crosses in the push itself, and a row
// WITHOUT the block hashes exactly as it did before the field existed (the golden bytes below are
// json.Marshal of this row on b45d824, generated there — report/sandboxpolicy_test.go holds the same).
func TestEvidenceHash_StableWithSandboxPolicy(t *testing.T) {
	const golden = `{"id":"S-1","status":"failed","duration_ms":1200,"correlation_id":"tr-r-S-1-ab","failure":{"observed":"HTTP 500"},"req_success":1,"req_failed":2,"assertions_enforced_count":3}`
	row := func() report.ScenarioResult {
		return report.ScenarioResult{ID: "S-1", Status: "failed", DurationMs: 1200, CorrelationID: "tr-r-S-1-ab",
			Failure: &report.Failure{Observed: "HTTP 500"}, ReqSuccess: 1, ReqFailed: 2, AssertionsEnforcedCount: 3,
			WindowStart: time.Unix(1775014130, 0), WindowEnd: time.Unix(1775014140, 0)}
	}
	sum := sha256.Sum256([]byte(golden))
	if got := evidenceHash(row()); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("a row with no sandbox_policy must hash as before the field existed: %s", got)
	}
	denied := 1
	withBlock := func(target string) report.ScenarioResult {
		r := row()
		r.SandboxPolicy = &report.SandboxPolicy{Source: "loki", Sandbox: "sb-1", Coverage: report.CoverageComplete,
			CoverageReason: "read 2 lines", DeniedCount: &denied, Events: []report.SandboxPolicyEvent{{Class: "NET:OPEN", Action: "Denied", Target: target}}}
		return r
	}
	a, b := evidenceHash(withBlock("SANDBOX-TARGET-CANARY:443")), evidenceHash(withBlock("SANDBOX-TARGET-CANARY:443"))
	if a != b {
		t.Fatalf("equal rows, different hashes: %s / %s", a, b)
	}
	if a == evidenceHash(withBlock("other.example:443")) || a == evidenceHash(row()) {
		t.Fatal("a different sandbox event (or no block) must give a different evidence hash")
	}
	rep := &report.Report{Summary: report.Summary{Total: 1, Failed: 1}, Layers: []report.Layer{{Layer: "HTTP Ingestion",
		Scenarios: []report.ScenarioResult{withBlock("SANDBOX-TARGET-CANARY:443")}}}}
	push := mapReport(rep, "run-26", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	pb, _ := json.Marshal(push)
	if strings.Contains(string(pb), "SANDBOX-TARGET-CANARY") || strings.Contains(string(pb), "sandbox_policy") {
		t.Fatalf("the sandbox_policy block must never cross in the results push: %s", pb)
	}
}
