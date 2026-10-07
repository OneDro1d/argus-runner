package runner

import "testing"

// V32 Release QA finding (c): the federated run materializes the set the control plane ASSIGNED and
// hands that directory to the runner core. The Env it builds must say so, or the report labels the
// catalog's set `local` (toolcore/assigned_set_test.go holds the measured case).
func TestExecEnv_TheMaterializedSetIsMarkedAssigned(t *testing.T) {
	env := execEnv(ExecConfig{InstanceID: "orderservice-compose", ResultsRoot: "/results"}, "/results/materialized/r1")
	if !env.AssignedSet {
		t.Error("execEnv runs a set the control plane assigned, so Env.AssignedSet must be true — " +
			"otherwise the report says scenario_source \"local\" for the catalog's set")
	}
}
