package federation

import (
	"encoding/json"
	"strings"
	"testing"
)

// (UI-6): the `test_targets` wire field on RegisterRequest and PollRequest.

func TestTestTargets_WireTagAndOmission(t *testing.T) {
	tt := json.RawMessage(`[{"name":"live","match":{"tags":["x"]}}]`)
	for name, v := range map[string]any{
		"register": RegisterRequest{InstanceID: "i", TestTargets: tt},
		"poll":     PollRequest{TestTargets: tt},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"test_targets":[{"name":"live"`) {
			t.Errorf("%s: payload %s does not carry the declaration under the json tag test_targets", name, raw)
		}
	}
	// nil = not reported = omitted; [] = reported none = present.
	raw, _ := json.Marshal(PollRequest{})
	if strings.Contains(string(raw), "test_targets") {
		t.Errorf("an unreported declaration was sent: %s", raw)
	}
	raw, _ = json.Marshal(PollRequest{TestTargets: json.RawMessage(`[]`)})
	if !strings.Contains(string(raw), `"test_targets":[]`) {
		t.Errorf("a reported-none declaration was dropped: %s", raw)
	}
}

func TestTestTargets_RoundTripThroughTheWire(t *testing.T) {
	in := RegisterRequest{InstanceID: "i", TestTargets: json.RawMessage(`[{"name":"lab","namespace":"msgbus-lab","match":{"tags":["lab"]}}]`)}
	raw, _ := json.Marshal(in)
	var out RegisterRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if string(out.TestTargets) != string(in.TestTargets) {
		t.Fatalf("round trip: %s -> %s", in.TestTargets, out.TestTargets)
	}
}

// An OLD executor's payload (no field) decodes to nil — "not reported".
func TestTestTargets_OldExecutorPayloadDecodesToNil(t *testing.T) {
	var reg RegisterRequest
	var poll PollRequest
	if err := json.Unmarshal([]byte(`{"instance_id":"i","money_writes_allow":[]}`), &reg); err != nil || reg.TestTargets != nil {
		t.Fatalf("register: %v, TestTargets=%s", err, reg.TestTargets)
	}
	if err := json.Unmarshal([]byte(`{"runner_version":"0.3.39"}`), &poll); err != nil || poll.TestTargets != nil {
		t.Fatalf("poll: %v, TestTargets=%s", err, poll.TestTargets)
	}
}

// An OLD control plane's body types have no test_targets field and no DisallowUnknownFields: the new
// field is dropped, everything else decodes.
func TestTestTargets_OldControlPlaneIgnoresTheField(t *testing.T) {
	type oldPoll struct {
		RunnerVersion string          `json:"runner_version,omitempty"`
		MoneyWrites   json.RawMessage `json:"money_writes_allow,omitempty"`
	}
	raw, _ := json.Marshal(PollRequest{RunnerVersion: "0.3.50", MoneyWritesAllow: json.RawMessage(`[]`),
		TestTargets: json.RawMessage(`[{"name":"live","match":{"tags":["x"]}}]`)})
	var o oldPoll
	if err := json.NewDecoder(strings.NewReader(string(raw))).Decode(&o); err != nil {
		t.Fatalf("an older control plane's decode failed: %v", err)
	}
	if o.RunnerVersion != "0.3.50" || string(o.MoneyWrites) != "[]" {
		t.Fatalf("older decode lost known fields: %+v", o)
	}
}
