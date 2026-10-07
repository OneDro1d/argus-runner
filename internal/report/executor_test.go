package report

import "testing"

// RO-04: an executor/harness failure (JMeter container down, docker exec error) must be
// classified `error` (DEC-07/VR-L3), DISTINCT from a SUT `failed`. IsExecutorFailure
// recognises the rig-down signatures so a dead rig never reads as "the tests failed".
func TestIsExecutorFailure(t *testing.T) {
	hits := []string{
		`jmeter exec failed: exit status 1: service "jmeter" is not running`,
		"jmeter run error: jmeter exec failed: exit status 1: service \"jmeter\" is not running",
		"no such service: jmeter",
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock",
		"error during connect: this error may indicate that the docker daemon is not running",
		"executor unavailable: bring up the stack",
	}
	for _, h := range hits {
		if !IsExecutorFailure(h) {
			t.Errorf("should classify as executor failure: %q", h)
		}
	}
	misses := []string{
		"responder returned status 400 (codes=[400])",
		"mandated control_action saga absent for correlation_id tr-x (saga-presence FAIL)",
		"content assertion failed (no matching row/message for the correlation_id)",
		"BODY-ASSERT-FAIL",
		"",
	}
	for _, m := range misses {
		if IsExecutorFailure(m) {
			t.Errorf("should NOT classify as executor failure (it is a real SUT/test signal): %q", m)
		}
	}
}
