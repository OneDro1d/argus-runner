package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// in mode `compare`: the run captures with NO `## LOAD` declared, so the checks whose targets
// decide the namespace are every check of the run, not the (absent) load checks.

// stubEnvCaptureRead swaps the Kubernetes seam for one that records the namespaces asked for and answers 403.
func stubEnvCaptureRead(t *testing.T) *[]string {
	t.Helper()
	old := envCaptureRead
	t.Cleanup(func() { envCaptureRead = old })
	asked := &[]string{}
	envCaptureRead = func(ns string) *envcapture.Capture {
		*asked = append(*asked, ns)
		return &envcapture.Capture{Namespace: ns, Reason: envcapture.ForbiddenReason(ns)}
	}
	return asked
}

func plainScenarioWithID(t *testing.T, id string) scenarioFile {
	t.Helper()
	p := plainScenarioFile(t)
	p.s.ID = id
	return p
}

func TestCaptureEnvironment_CompareModeNoLoad_OutsideClusterTargetStillAsksKubernetes(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	asked := stubEnvCaptureRead(t)
	tg := testtargets.List{{Name: "site", OutsideCluster: true, Match: testtargets.Match{ScenarioPrefixes: []string{"SITE-"}}}}
	got := captureEnvironmentFor([]scenarioFile{plainScenarioWithID(t, "SITE-1")}, report.ModeCompare, tg)
	t.Logf("capture=%+v asked=%v", got, *asked)
	if len(*asked) != 0 {
		t.Fatalf("Kubernetes asked for %v although every check belongs to an outside_cluster target", *asked)
	}
	if got == nil || got.Captured || got.Namespace != "" {
		t.Fatalf("capture = %+v, want non-nil, not captured, no namespace", got)
	}
	if !strings.HasPrefix(got.Reason, "not applicable: the target of these checks is declared outside the cluster") || !strings.Contains(got.Reason, "site") {
		t.Errorf("reason = %q, want the not-applicable reason naming the target, in the words of a compare run", got.Reason)
	}
	if strings.Contains(got.Reason, "load") {
		t.Errorf("reason %q talks of load checks, but this run declares none", got.Reason)
	}
}

func TestCaptureEnvironment_CompareModeNoLoad_OneInClusterPlusOutside_ReadsItAndNotesTheRest(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	asked := stubEnvCaptureRead(t)
	got := captureEnvironmentFor([]scenarioFile{plainScenarioWithID(t, "LAB-1"), plainScenarioWithID(t, "SITE-1")}, report.ModeCompare, outsideTargets())
	if len(*asked) != 1 || (*asked)[0] != "msgbus-lab" {
		t.Fatalf("asked %q, want exactly the in-cluster namespace msgbus-lab", *asked)
	}
	if got == nil || got.Namespace != "msgbus-lab" || len(got.Warnings) != 1 ||
		!strings.Contains(got.Warnings[0], "site") || !strings.Contains(got.Warnings[0], "outside the cluster") {
		t.Fatalf("capture = %+v, want msgbus-lab read and one warning naming the outside target", got)
	}
}

func TestCaptureEnvironment_CompareModeNoLoad_SeveralNamespaces_ReadsNothingAndSaysWhy(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	asked := stubEnvCaptureRead(t)
	got := captureEnvironmentFor([]scenarioFile{plainScenarioWithID(t, "LAB-1"), plainScenarioWithID(t, "LIVE-1"), plainScenarioWithID(t, "SITE-1")}, report.ModeCompare, outsideTargets())
	if len(*asked) != 0 {
		t.Fatalf("Kubernetes asked for %v, want nothing for several namespaces", *asked)
	}
	if got == nil || got.Namespace != "" || !strings.Contains(got.Reason, "more than one") || !strings.HasPrefix(got.Reason, "this run's checks") {
		t.Fatalf("capture = %+v, want the several-namespaces refusal in the words of a compare run", got)
	}
}

func TestCaptureEnvironment_CompareModeNoLoad_NoTargetDeclared_EnvVarAsToday(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	asked := stubEnvCaptureRead(t)
	for name, tg := range map[string]testtargets.List{"no test_targets": nil, "unrelated targets": nsTargets()} {
		*asked = nil
		got := captureEnvironmentFor([]scenarioFile{plainScenarioWithID(t, "OTHER-1")}, report.ModeCompare, tg)
		if len(*asked) != 1 || (*asked)[0] != "msgbus" || got == nil || got.Namespace != "msgbus" {
			t.Errorf("%s: capture = %+v, asked %q, want ARGUS_SUT_NAMESPACE (msgbus) read as before", name, got, *asked)
		}
	}
}

// follow-up: the 403 text tells the reader to declare `outside_cluster: true`. In EVERY mode that
// can produce the 403, doing exactly that must stop the read; otherwise the advice points at a key that cannot help.
func TestForbiddenReason_DeclaringOutsideClusterReallyStopsTheRead(t *testing.T) {
	modes := map[string]struct {
		mode string
		scn  func(t *testing.T) scenarioFile
	}{
		"load run":    {mode: "", scn: func(t *testing.T) scenarioFile { s := loadScenarioFile(t); s.s.ID = "SITE-1"; return s }},
		"compare run": {mode: report.ModeCompare, scn: func(t *testing.T) scenarioFile { return plainScenarioWithID(t, "SITE-1") }},
	}
	for name, m := range modes {
		t.Run(name, func(t *testing.T) {
			t.Setenv(sutNamespaceEnvVar, "msgbus")
			asked := stubEnvCaptureRead(t)
			site := testtargets.Target{Name: "site", Match: testtargets.Match{ScenarioPrefixes: []string{"SITE-"}}}
			before := captureEnvironmentFor([]scenarioFile{m.scn(t)}, m.mode, testtargets.List{site})
			if before == nil || !strings.Contains(before.Reason, "outside_cluster: true") {
				t.Fatalf("setup: capture = %+v, want a 403 reason that advises outside_cluster: true", before)
			}
			*asked = nil
			site.OutsideCluster = true // what the reason says to do
			after := captureEnvironmentFor([]scenarioFile{m.scn(t)}, m.mode, testtargets.List{site})
			if len(*asked) != 0 {
				t.Fatalf("after declaring outside_cluster: true Kubernetes was still asked for %v", *asked)
			}
			if after == nil || strings.Contains(after.Reason, "forbidden") || !strings.HasPrefix(after.Reason, "not applicable") {
				t.Fatalf("capture = %+v, want the not-applicable reason, not the 403 again", after)
			}
		})
	}
}
