package runner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// VR6-W1 — the SUT observation reaches the control plane ON THE POLL.
//
// ── WHY THE POLL, WHICH IS NOT A PREFERENCE BUT THE INT-029 LESSON ────────────────────────────────
//
// pollRequestFor's own header states the rule: EVERY executor-only fact belongs on the poll, never on
// register. Register-on-start is refused with
//
//	403 {"error":"re-register requires the machine JWT of the registered identity"}
//
// for any instance the onboard already claimed — measured on compose as well as k3d/aks. A field that
// rides only on register therefore reaches the control plane exactly once, on a first onboard, and never
// again. SUT reachability is the worst possible fact to freeze at start-up: a SUT that dies later is
// precisely the case this exists for, and memstore-compose died at a reboot, hours after onboarding.
//
// ── AND WHY IT MUST BE OMITTED WHEN UNKNOWN ───────────────────────────────────────────────────────
//
// omitempty on a *bool means an executor that has not probed sends NOTHING, rather than a false the
// control plane would render as a red verdict on a SUT nobody dialled. This is the same reason the
// Replicas* fields in the same struct are pointers, and their comment already says it: "so 'not
// reported' is distinguishable from zero."

func TestPollRequestFor_CarriesAReachableSUT(t *testing.T) {
	at := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	yes := true
	req := pollRequestFor("0.3.23", nil, nil, nil, nil, nil,
		federation.SUTObservation{Reachable: &yes, CheckedAt: &at}, nil, nil)

	if req.SUTReachable == nil || !*req.SUTReachable {
		t.Fatal("a reachable SUT did not reach the poll payload")
	}
	if req.SUTCheckedAt == nil || !req.SUTCheckedAt.Equal(at) {
		t.Fatalf("checked-at = %v, want %v — the page needs WHEN in order to grey out a stale reading",
			req.SUTCheckedAt, at)
	}
}

// 🚩 THE ONE THAT MATTERS — memstore-compose and social-mcp-k3d, the two measured cases.
func TestPollRequestFor_CarriesAnUnreachableSUT(t *testing.T) {
	at := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	no := false
	req := pollRequestFor("0.3.23", nil, nil, nil, nil, nil,
		federation.SUTObservation{Reachable: &no, CheckedAt: &at}, nil, nil)

	if req.SUTReachable == nil {
		t.Fatal("an UNREACHABLE SUT was dropped from the poll payload — which is the state the whole " +
			"requirement exists to make visible")
	}
	if *req.SUTReachable {
		t.Error("an unreachable SUT was reported as reachable")
	}
}

// ⚠ An executor that has not probed must send NOTHING. A false here becomes a red dot on a SUT nobody
// dialled — the same defect as V18-008, inverted.
func TestPollRequestFor_AnUnprobedSUTIsOmittedEntirelyRatherThanSentAsFalse(t *testing.T) {
	req := pollRequestFor("0.3.23", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)

	if req.SUTReachable != nil {
		t.Fatalf("an unprobed SUT sent %v; want the field absent", *req.SUTReachable)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := m["sut_reachable"]; present {
		t.Errorf("sut_reachable is present on the wire for an unprobed SUT: %s", raw)
	}
	if _, present := m["sut_checked_at"]; present {
		t.Errorf("sut_checked_at is present on the wire for an unprobed SUT: %s", raw)
	}
}

// The wire names are part of the contract with the control plane and its migration. Pinned so a rename
// is a decision rather than a silent drop to nil on the far side.
func TestPollRequestFor_UsesTheAgreedWireNames(t *testing.T) {
	at := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	no := false
	raw, err := json.Marshal(pollRequestFor("0.3.23", nil, nil, nil, nil, nil,
		federation.SUTObservation{Reachable: &no, CheckedAt: &at}, nil, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"sut_reachable", "sut_checked_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("field %q missing from the poll payload: %s", k, raw)
		}
	}
}

// A reachability answer with no timestamp is not usable: the page cannot tell a current reading from a
// twenty-hour-old one, and its staleness rule is the difference between green and grey.
func TestPollRequestFor_AVerdictWithoutATimestampIsNotCarried(t *testing.T) {
	yes := true
	req := pollRequestFor("0.3.23", nil, nil, nil, nil, nil,
		federation.SUTObservation{Reachable: &yes}, nil, nil)

	if req.SUTReachable != nil {
		t.Fatal("a verdict with no checked-at was carried anyway. The control plane would then hold a " +
			"reachability it can never age out, and a SUT that died a day ago stays green forever")
	}
}
