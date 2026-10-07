package argus

import (
	"strings"
	"testing"
)

// JMeter-as-a-service (D2): when JMeter is bundled into the serve image, the runner invokes
// it as a LOCAL subprocess — no `docker compose exec`, no docker socket. buildArgs is pure +
// deterministic (sorted -J props) so the invocation is unit-testable without a live JMeter.
func TestLocalJMeterRunner_BuildArgs(t *testing.T) {
	r := &LocalJMeterRunner{TemplatesDir: "/templates"}
	args := r.buildArgs("http-ingestion", "/results/x.jtl", "", map[string]string{
		"scenario.id": "ORD-001", "correlation.id": "tr-1", "http.host": "my-api",
	})
	joined := strings.Join(args, " ")

	for _, want := range []string{"-n", "-t /templates/http-ingestion.jmx", "-l /results/x.jtl", "-j /results/x.jtl.log",
		"-Jcorrelation.id=tr-1", "-Jhttp.host=my-api", "-Jscenario.id=ORD-001"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n  %s", want, joined)
		}
	}
	// deterministic, sorted -J order (correlation.id < http.host < scenario.id)
	if !(strings.Index(joined, "-Jcorrelation.id") < strings.Index(joined, "-Jhttp.host") &&
		strings.Index(joined, "-Jhttp.host") < strings.Index(joined, "-Jscenario.id")) {
		t.Errorf("-J props must be sorted for determinism:\n  %s", joined)
	}
}

func TestLocalJMeterRunner_Defaults(t *testing.T) {
	r := &LocalJMeterRunner{} // no TemplatesDir → defaults to /templates; bin defaults to jmeter
	args := r.buildArgs("database-state", "/results/d.jtl", "", nil)
	if !strings.Contains(strings.Join(args, " "), "-t /templates/database-state.jmx") {
		t.Errorf("default TemplatesDir should be /templates: %v", args)
	}
	if r.bin() != "jmeter" {
		t.Errorf("default bin should be jmeter, got %q", r.bin())
	}
}

// It satisfies both the Runner and the HealthChecker (RO-04 preflight) interfaces.
func TestLocalJMeterRunner_ImplementsInterfaces(t *testing.T) {
	var _ Runner = &LocalJMeterRunner{}
	var _ HealthChecker = &LocalJMeterRunner{}
}
