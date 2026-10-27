package federation

// tallies_planned_test.go -- Tallies.Planned: sent on the mid-run heartbeat only, invisible on every other push, and
// harmless to an older control plane (no DisallowUnknownFields on the wire).

import (
	"encoding/json"
	"strings"
	"testing"
)

// oldTallies / oldResultsPush are the wire types as an OLDER control plane has them: Tallies without `planned`.
type oldTallies struct {
	Passed          int `json:"passed"`
	Failed          int `json:"failed"`
	Errored         int `json:"errored"`
	Total           int `json:"total"`
	RateLimited     int `json:"rate_limited,omitempty"`
	RateLimitPauses int `json:"rate_limit_pauses,omitempty"`
	Degraded        int `json:"degraded,omitempty"`
}

type oldResultsPush struct {
	RunID     string     `json:"run_id"`
	Status    string     `json:"status"`
	Tallies   oldTallies `json:"tallies"`
	Scenarios []any      `json:"scenarios"`
}

// An old control plane's body type decodes a new heartbeat without error and reads every count it knows.
func TestTallies_OldBodyTypeDecodesANewHeartbeat(t *testing.T) {
	hb := ResultsPush{RunID: "r1", Status: "running",
		Tallies: Tallies{Passed: 3, Failed: 1, Errored: 1, Degraded: 1, Total: 6, Planned: 40}}
	b, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"planned":40`) {
		t.Fatalf("the heartbeat does not carry planned: %s", b)
	}
	var old oldResultsPush
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("an old body type refused a new heartbeat: %v", err)
	}
	if old.Tallies != (oldTallies{Passed: 3, Failed: 1, Errored: 1, Degraded: 1, Total: 6}) {
		t.Fatalf("old reader got %+v", old.Tallies)
	}
}

// Planned is omitempty: a push that does not set it marshals exactly as the type did before the field existed.
func TestTallies_PlannedIsAbsentUnlessSet(t *testing.T) {
	cur := Tallies{Passed: 2, Failed: 1, Errored: 2, Total: 6, RateLimited: 1, RateLimitPauses: 3, Degraded: 1}
	was := oldTallies{Passed: 2, Failed: 1, Errored: 2, Total: 6, RateLimited: 1, RateLimitPauses: 3, Degraded: 1}
	a, _ := json.Marshal(cur)
	b, _ := json.Marshal(was)
	if string(a) != string(b) {
		t.Fatalf("Tallies bytes changed for a push with no planned:\n now %s\n was %s", a, b)
	}
	zero, _ := json.Marshal(Tallies{})
	if string(zero) != `{"passed":0,"failed":0,"errored":0,"total":0}` {
		t.Fatalf("zero Tallies = %s", zero)
	}
}
