package argus

import (
	"os"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

func loadScenarioFile(t *testing.T) scenarioFile {
	t.Helper()
	md := "**ID**: LOAD-001\n" +
		"**Layer**: HTTP Ingestion\n\n" +
		"## TRIGGER\nGET `/health`\n\n" +
		"## LOAD\n**Users**: 10\n**Ramp Seconds**: 5\n**Duration Seconds**: 30\n**Target P95 Ms**: 200\n**Max Error Rate**: 0.01\n\n" +
		"## EXPECT\n- status=200\n"
	s := scenario.Parse(md)
	if s.Load == nil {
		t.Fatal("test fixture did not parse a LOAD profile — fix the fixture, not the test")
	}
	return scenarioFile{s: s, path: "LOAD-001.md"}
}

func plainScenarioFile(t *testing.T) scenarioFile {
	t.Helper()
	md := "**ID**: PLAIN-001\n**Layer**: HTTP Ingestion\n\n## TRIGGER\nGET `/health`\n\n## EXPECT\n- status=200\n"
	s := scenario.Parse(md)
	if s.Load != nil {
		t.Fatal("test fixture unexpectedly parsed a LOAD profile")
	}
	return scenarioFile{s: s, path: "PLAIN-001.md"}
}

func TestAnyLoadDeclared(t *testing.T) {
	if anyLoadDeclared(nil) {
		t.Error("anyLoadDeclared(nil) = true")
	}
	if anyLoadDeclared([]scenarioFile{plainScenarioFile(t)}) {
		t.Error("anyLoadDeclared = true for a run with no LOAD-declaring scenario")
	}
	if !anyLoadDeclared([]scenarioFile{plainScenarioFile(t), loadScenarioFile(t)}) {
		t.Error("anyLoadDeclared = false despite one LOAD-declaring scenario in the set")
	}
}

// TestCaptureEnvironmentIfNeeded_NoLoad_ReturnsNil is the report.Report.Environment field's
// contract: a run with no `## LOAD` section must get no environment field at all (nil, omitempty)
// — never an attempted-and-failed one, because it was never asked for.
func TestCaptureEnvironmentIfNeeded_NoLoad_ReturnsNil(t *testing.T) {
	got := captureEnvironmentIfNeeded([]scenarioFile{plainScenarioFile(t)})
	if got != nil {
		t.Errorf("captureEnvironmentIfNeeded = %+v, want nil for a non-load run", got)
	}
}

// TestCaptureEnvironmentIfNeeded_LoadNoNamespace_NamesTheReason: a load run with no
// ARGUS_SUT_NAMESPACE declared must still get a NON-nil Environment (Captured=false, a named
// Reason) — never silently absent, which is indistinguishable from "not a load run" to a reader.
func TestCaptureEnvironmentIfNeeded_LoadNoNamespace_NamesTheReason(t *testing.T) {
	old, had := os.LookupEnv(sutNamespaceEnvVar)
	_ = os.Unsetenv(sutNamespaceEnvVar)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(sutNamespaceEnvVar, old)
		}
	})

	got := captureEnvironmentIfNeeded([]scenarioFile{loadScenarioFile(t)})
	if got == nil {
		t.Fatal("captureEnvironmentIfNeeded = nil for a load run — want a non-nil Capture naming why it could not run")
	}
	if got.Captured {
		t.Error("Captured = true with no SUT namespace declared")
	}
	if got.Reason == "" {
		t.Error("Reason is empty — a load run's missing capture must always say why")
	}
}
