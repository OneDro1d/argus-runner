package report

import (
	"bytes"
	"encoding/json"
	"testing"
)

const rawArgus = `{
  "project": "order-service",
  "timestamp": "2026-06-17T10:00:00Z",
  "mode": "ci",
  "summary": {"total": 2, "passed": 1, "failed": 1, "skipped": 0},
  "layers": [
    {"layer": "http-ingestion", "scenarios": [
      {"id": "ORD-001", "status": "passed", "duration_ms": 45},
      {"id": "ORD-003", "status": "failed", "duration_ms": 10004, "failure": "200: OK"}
    ]}
  ]
}`

func TestNormalize_TestHat_HasExpected(t *testing.T) {
	exp := map[string]string{"ORD-003": "status=409"}
	corr := map[string]string{"ORD-003": "tr-003"}
	r, err := Normalize([]byte(rawArgus), exp, corr)
	if err != nil {
		t.Fatal(err)
	}
	res, layer := r.Find("ORD-003")
	if res == nil {
		t.Fatal("ORD-003 not found")
	}
	if layer != "http-ingestion" {
		t.Errorf("layer = %q", layer)
	}
	if res.Failure == nil || res.Failure.Observed != "200: OK" {
		t.Errorf("observed = %+v", res.Failure)
	}
	if res.Failure.Expected == nil || *res.Failure.Expected != "status=409" {
		t.Errorf("expected not set for test hat: %+v", res.Failure)
	}
	if res.CorrelationID != "tr-003" {
		t.Errorf("correlation_id = %q", res.CorrelationID)
	}
	if !r.Failed() {
		t.Error("Failed() should be true")
	}
}

func TestNormalize_ProductHat_NoExpected(t *testing.T) {
	// Product hat: runner passes nil expecteds -> expected omitted.
	r, err := Normalize([]byte(rawArgus), nil, map[string]string{"ORD-003": "tr-003"})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := r.Find("ORD-003")
	if res.Failure == nil || res.Failure.Observed != "200: OK" {
		t.Fatalf("observed missing: %+v", res.Failure)
	}
	if res.Failure.Expected != nil {
		t.Errorf("product hat must not have expected: %v", *res.Failure.Expected)
	}
	// And the marshaled JSON must not carry an "expected" key.
	js, _ := json.Marshal(r)
	if bytes.Contains(js, []byte(`"expected"`)) {
		t.Errorf("product report JSON leaked an expected key: %s", js)
	}
}

// Test-requests panel (r3): the 3-way runner-fired request counts round-trip through the report
// JSON, omitempty keeps a no-request scenario byte-clean, and the per-request Requests slice is
// NOT serialized (runtime-only, json:"-").
func TestScenarioResult_ReqCountsRoundTrip(t *testing.T) {
	in := Report{Layers: []Layer{{Layer: "http-ingestion", Scenarios: []ScenarioResult{
		{ID: "PERM-001", Status: "passed", ReqFailed: 1}, // error-path: PASSES but the request returned a 4xx (negative RESPONSE)
		{ID: "ORD-001", Status: "passed", ReqSuccess: 1}, // happy path: one 2xx request
		{ID: "DWN-001", Status: "error", ReqError: 1},    // SUT unreachable: the request got no response
		{ID: "ERR-001", Status: "error"},                 // preflight/exec error: NO request fired → all 0
	}}}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// omitempty: a zero count emits no key (no-request scenario stays byte-clean).
	if !bytes.Contains(b, []byte(`"req_failed":1`)) || !bytes.Contains(b, []byte(`"req_success":1`)) || !bytes.Contains(b, []byte(`"req_error":1`)) {
		t.Errorf("non-zero counts must serialize: %s", b)
	}
	var out Report
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	got := out.Layers[0].Scenarios
	if got[0].ReqFailed != 1 || got[0].ReqSuccess != 0 || got[0].ReqError != 0 {
		t.Errorf("PERM-001 round-trip: %+v", got[0])
	}
	if got[1].ReqSuccess != 1 || got[1].ReqFailed != 0 || got[1].ReqError != 0 {
		t.Errorf("ORD-001 round-trip: %+v", got[1])
	}
	if got[2].ReqError != 1 || got[2].ReqSuccess != 0 || got[2].ReqFailed != 0 {
		t.Errorf("DWN-001 round-trip: %+v", got[2])
	}
	if got[3].ReqSuccess != 0 || got[3].ReqFailed != 0 || got[3].ReqError != 0 {
		t.Errorf("ERR-001 (no request fired) must round-trip 0/0/0: %+v", got[3])
	}
}

// AddRequest is the single source of truth: each call appends a timestamped sample AND
// increments the matching 3-way count, so counts can never drift from samples. The Requests
// slice is runtime-only (json:"-") — it must NOT appear in the serialized report.
func TestScenarioResult_AddRequest(t *testing.T) {
	var r ScenarioResult
	r.AddRequest(1000, OutcomeSuccess)
	r.AddRequest(1010, OutcomeSuccess)
	r.AddRequest(1020, OutcomeFailed)
	r.AddRequest(1030, OutcomeError)
	if r.ReqSuccess != 2 || r.ReqFailed != 1 || r.ReqError != 1 {
		t.Errorf("counts drifted from samples: %+v", r)
	}
	if len(r.Requests) != 4 || r.Requests[0].AtMs != 1000 || r.Requests[3].Outcome != OutcomeError {
		t.Errorf("samples not recorded: %+v", r.Requests)
	}
	b, _ := json.Marshal(r)
	if bytes.Contains(b, []byte(`"Requests"`)) || bytes.Contains(b, []byte(`"AtMs"`)) {
		t.Errorf("Requests must be runtime-only (json:\"-\"), leaked: %s", b)
	}
}

func TestNormalize_PassedHasNoFailure(t *testing.T) {
	r, _ := Normalize([]byte(rawArgus), map[string]string{}, nil)
	res, _ := r.Find("ORD-001")
	if res.Failure != nil {
		t.Errorf("passed scenario must have no failure: %+v", res.Failure)
	}
	if r.Summary.Total != 2 || r.Summary.Passed != 1 || r.Summary.Failed != 1 {
		t.Errorf("summary = %+v", r.Summary)
	}
}
