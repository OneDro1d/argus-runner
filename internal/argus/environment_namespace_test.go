package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// ARGUS-TA-5: a load run's environment capture reads the namespace of the
// test target its LOAD checks belong to, not blindly ARGUS_SUT_NAMESPACE.

func nsTargets() testtargets.List {
	return testtargets.List{
		{Name: "lab-load", Namespace: "msgbus-lab", Match: testtargets.Match{ScenarioPrefixes: []string{"LAB-"}}},
		{Name: "live", Namespace: "msgbus", Match: testtargets.Match{ScenarioPrefixes: []string{"LIVE-"}}},
		{Name: "nons", Match: testtargets.Match{ScenarioPrefixes: []string{"NONS-"}}},
	}
}

func TestChooseCaptureNamespace_OneTargetNamespace_ReadsIt(t *testing.T) {
	got, reason := chooseCaptureNamespace(nsTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "LAB-2"}}, "msgbus")
	if got != "msgbus-lab" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus-lab, \"\")", got, reason)
	}
}

func TestChooseCaptureNamespace_NoTestTargets_EnvVar(t *testing.T) {
	got, reason := chooseCaptureNamespace(nil, []loadCheckRef{{ID: "LAB-1"}}, "msgbus")
	if got != "msgbus" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus, \"\")", got, reason)
	}
}

func TestChooseCaptureNamespace_UnassignedOrNoNamespace_EnvVar(t *testing.T) {
	for _, id := range []string{"OTHER-1", "NONS-1"} {
		got, reason := chooseCaptureNamespace(nsTargets(), []loadCheckRef{{ID: id}}, "msgbus")
		if got != "msgbus" || reason != "" {
			t.Fatalf("%s: got (%q, %q), want (msgbus, \"\")", id, got, reason)
		}
	}
}

func TestChooseCaptureNamespace_SeveralNamespaces_CapturesNothingAndSaysWhy(t *testing.T) {
	got, reason := chooseCaptureNamespace(nsTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "LIVE-1"}}, "msgbus")
	if got != "" {
		t.Fatalf("namespace = %q, want none for several", got)
	}
	for _, want := range []string{"msgbus-lab", "msgbus", "lab-load", "live", "more than one"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}

func TestChooseCaptureNamespace_MixedAssignedAndUnassigned_UnassignedCountsAsEnvVar(t *testing.T) {
	// assigned -> msgbus-lab, unassigned -> env var msgbus: two distinct namespaces, so nothing is guessed.
	got, reason := chooseCaptureNamespace(nsTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "OTHER-1"}}, "msgbus")
	if got != "" || !strings.Contains(reason, "msgbus-lab") || !strings.Contains(reason, "msgbus") {
		t.Fatalf("got (%q, %q), want empty namespace and a reason naming both", got, reason)
	}
	// when the env var equals the target's namespace the mix is ONE namespace.
	got, reason = chooseCaptureNamespace(nsTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "OTHER-1"}}, "msgbus-lab")
	if got != "msgbus-lab" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus-lab, \"\")", got, reason)
	}
}

func TestChooseCaptureNamespace_NoLoadChecks_EnvVar(t *testing.T) {
	got, reason := chooseCaptureNamespace(nsTargets(), nil, "msgbus")
	if got != "msgbus" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus, \"\")", got, reason)
	}
}

func TestChooseCaptureNamespace_TagMatch(t *testing.T) {
	l := testtargets.List{{Name: "t", Namespace: "ns-t", Match: testtargets.Match{Tags: []string{"lab"}}}}
	got, _ := chooseCaptureNamespace(l, []loadCheckRef{{ID: "X-1", Tags: []string{"lab"}}}, "")
	if got != "ns-t" {
		t.Fatalf("got %q, want ns-t", got)
	}
}

func TestLoadCheckRefs_OnlyLoadChecks(t *testing.T) {
	l, p := loadScenarioFile(t), plainScenarioFile(t)
	l.s.ID, p.s.ID = "LOAD-001", "PLAIN-001" // the shared fixtures' metadata form parses no ID; set it
	refs := loadCheckRefs([]scenarioFile{p, l})
	if len(refs) != 1 || refs[0].ID != "LOAD-001" {
		t.Fatalf("refs = %+v, want only LOAD-001", refs)
	}
}

func TestCaptureEnvironment_SeveralNamespaces_NonNilCaptureWithReason(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	a, b := loadScenarioFile(t), loadScenarioFile(t)
	a.s.ID, b.s.ID = "LAB-1", "LIVE-1"
	got := captureEnvironmentFor([]scenarioFile{a, b}, "", nsTargets())
	if got == nil || got.Captured || got.Namespace != "" || got.Reason == "" {
		t.Fatalf("capture = %+v, want non-nil, not captured, no namespace, a reason", got)
	}
}

// a target declared `outside_cluster: true` has nothing in the cluster to read.

func outsideTargets() testtargets.List {
	return append(nsTargets(),
		testtargets.Target{Name: "site", OutsideCluster: true, Match: testtargets.Match{ScenarioPrefixes: []string{"SITE-"}}},
		testtargets.Target{Name: "cdn", OutsideCluster: true, Match: testtargets.Match{ScenarioPrefixes: []string{"CDN-"}}},
	)
}

func TestChooseCaptureNamespace_AllChecksOutsideCluster_NotApplicable_EnvVarNotAFallback(t *testing.T) {
	got, reason := chooseCaptureNamespace(outsideTargets(), []loadCheckRef{{ID: "SITE-1"}, {ID: "CDN-1"}}, "msgbus")
	if got != "" {
		t.Fatalf("namespace = %q, want none: ARGUS_SUT_NAMESPACE must not be a fallback for outside-cluster targets", got)
	}
	for _, want := range []string{"not applicable", "declared outside the cluster", "cdn", "site"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}

func TestChooseCaptureNamespace_OneInClusterPlusOutside_ReadsTheInClusterNamespace(t *testing.T) {
	got, reason := chooseCaptureNamespace(outsideTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "SITE-1"}}, "msgbus")
	if got != "msgbus-lab" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus-lab, \"\")", got, reason)
	}
}

func TestChooseCaptureNamespace_TwoInClusterPlusOutside_StillSeveral(t *testing.T) {
	got, reason := chooseCaptureNamespace(outsideTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "LIVE-1"}, {ID: "SITE-1"}}, "")
	if got != "" || !strings.Contains(reason, "more than one") {
		t.Fatalf("got (%q, %q), want the several-namespaces refusal", got, reason)
	}
}

func TestChooseCaptureNamespace_OutsideAndUndeclaredCheck_UndeclaredKeepsTodaysRule(t *testing.T) {
	got, reason := chooseCaptureNamespace(outsideTargets(), []loadCheckRef{{ID: "SITE-1"}, {ID: "OTHER-1"}}, "msgbus")
	if got != "msgbus" || reason != "" {
		t.Fatalf("got (%q, %q), want (msgbus, \"\")", got, reason)
	}
}

func TestCaptureEnvironment_AllOutsideCluster_NoKubernetesAsked_ReasonSaysSo(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	old := envCaptureRead
	t.Cleanup(func() { envCaptureRead = old })
	envCaptureRead = func(ns string) *envcapture.Capture {
		t.Errorf("Kubernetes was asked for namespace %q for a run whose every load check is outside the cluster", ns)
		return &envcapture.Capture{}
	}
	a := loadScenarioFile(t)
	a.s.ID = "SITE-1"
	got := captureEnvironmentFor([]scenarioFile{a}, "", outsideTargets())
	if got == nil || got.Captured || got.Namespace != "" {
		t.Fatalf("capture = %+v, want non-nil, not captured, no namespace", got)
	}
	if !strings.HasPrefix(got.Reason, "not applicable: the target of these load checks is declared outside the cluster") {
		t.Errorf("reason = %q", got.Reason)
	}
	if strings.Contains(got.Reason, "forbidden") || strings.Contains(got.Reason, "credentials") {
		t.Errorf("reason %q reads like a Kubernetes refusal, but nothing was asked of Kubernetes", got.Reason)
	}
}

func TestCaptureEnvironment_OneInClusterPlusOutside_NamespaceReadAndWarningNamesTheOutsideTarget(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "msgbus")
	old := envCaptureRead
	t.Cleanup(func() { envCaptureRead = old })
	var asked []string
	envCaptureRead = func(ns string) *envcapture.Capture {
		asked = append(asked, ns)
		return &envcapture.Capture{Captured: true, Namespace: ns}
	}
	a, b := loadScenarioFile(t), loadScenarioFile(t)
	a.s.ID, b.s.ID = "LAB-1", "SITE-1"
	got := captureEnvironmentFor([]scenarioFile{a, b}, "", outsideTargets())
	if got == nil || !got.Captured || got.Namespace != "msgbus-lab" || len(asked) != 1 || asked[0] != "msgbus-lab" {
		t.Fatalf("capture = %+v, asked %q, want exactly the in-cluster namespace msgbus-lab read", got, asked)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "site") || !strings.Contains(got.Warnings[0], "outside the cluster") {
		t.Errorf("warnings = %q, want one naming the outside-cluster target", got.Warnings)
	}
}

func TestOutsideClusterNote_NamesTheTargetsNotRead(t *testing.T) {
	got := outsideClusterNotes(outsideTargets(), []loadCheckRef{{ID: "LAB-1"}, {ID: "SITE-1"}, {ID: "CDN-1"}, {ID: "SITE-2"}})
	if len(got) != 1 || !strings.Contains(got[0], "cdn, site") || !strings.Contains(got[0], "outside the cluster") {
		t.Fatalf("notes = %q, want one note naming cdn and site", got)
	}
	if n := outsideClusterNotes(outsideTargets(), []loadCheckRef{{ID: "LAB-1"}}); len(n) != 0 {
		t.Errorf("notes = %q, want none when no check belongs to an outside-cluster target", n)
	}
}

func TestCaptureEnvironment_NonLoadRunUntouchedByTargets(t *testing.T) {
	if got := captureEnvironmentFor([]scenarioFile{plainScenarioFile(t)}, "", nsTargets()); got != nil {
		t.Fatalf("capture = %+v, want nil for a non-load run", got)
	}
}
