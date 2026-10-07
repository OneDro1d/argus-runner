package report

import (
	"encoding/json"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// TestReport_Environment_OmittedWhenNil: a non-load report must carry NO "environment" key at all
// — not `"environment": null` — so an older reader (or a byte-diff against a pre-existing fixture)
// sees no change on the runs that never declared a LOAD profile.
func TestReport_Environment_OmittedWhenNil(t *testing.T) {
	r := Report{Project: "order-service"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["environment"]; ok {
		t.Errorf("report carries an \"environment\" key with Environment == nil: %s", b)
	}
}

// TestReport_Environment_RoundTrips: a captured (or a named-reason) environment survives a
// marshal/unmarshal unchanged — the shape a consumer (CLI, UI, a future comparison surface) reads.
func TestReport_Environment_RoundTrips(t *testing.T) {
	cap := envcapture.Capture{
		Captured:  true,
		Namespace: "checkout",
		Pods: []envcapture.Pod{{
			Name: "checkout-abc", OwnerKind: "Deployment", OwnerName: "checkout",
			Containers: []envcapture.Container{{Name: "checkout", Image: "img:1", RequestsCPU: "250m"}},
		}},
		Fingerprint: "deadbeef",
	}
	r := Report{Project: "order-service", Environment: &cap}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Environment == nil {
		t.Fatal("Environment lost across round-trip")
	}
	if got.Environment.Fingerprint != "deadbeef" || !got.Environment.Captured || len(got.Environment.Pods) != 1 {
		t.Errorf("Environment round-tripped wrong: %+v", got.Environment)
	}
}
