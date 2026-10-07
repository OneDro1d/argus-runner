package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

//, EXECUTOR HALF: one version reading per Kubernetes namespace a run touches, taken for every
// run mode before any work on the system, and kept entirely out of the evidence bundle.

// msgbusTargets is the shape the tester reported: `live` in msgbus, `lab` and `lab-load` BOTH in msgbus-lab.
func msgbusTargets() testtargets.List {
	return testtargets.List{
		{Name: "live", Namespace: "msgbus", Match: testtargets.Match{ScenarioPrefixes: []string{"LIVE-"}}},
		{Name: "lab", Namespace: "msgbus-lab", Match: testtargets.Match{ScenarioPrefixes: []string{"LAB-"}}},
		{Name: "lab-load", Namespace: "msgbus-lab", Match: testtargets.Match{ScenarioPrefixes: []string{"LOAD-"}}},
		{Name: "bare", Match: testtargets.Match{ScenarioPrefixes: []string{"BARE-"}}},
	}
}

func refs(ids ...string) []scenarioRef {
	var out []scenarioRef
	for _, id := range ids {
		out = append(out, scenarioRef{ID: id})
	}
	return out
}

func TestVersionNamespaces_TwoTargetsOfOneNamespaceAreOneNamespace(t *testing.T) {
	got := versionNamespaces(msgbusTargets(), refs("LAB-1", "LOAD-1"), "msgbus")
	if len(got) != 1 || got[0].Namespace != "msgbus-lab" {
		t.Fatalf("got %+v, want exactly msgbus-lab: lab and lab-load share a namespace", got)
	}
	if !reflect.DeepEqual(got[0].Targets, []string{"lab", "lab-load"}) {
		t.Errorf("targets %v, want [lab lab-load]", got[0].Targets)
	}
}

func TestVersionNamespaces_ARunOverSeveralTargetsReadsEachNamespaceOnce(t *testing.T) {
	got := versionNamespaces(msgbusTargets(), refs("LIVE-1", "LAB-1", "LAB-2", "LOAD-1"), "msgbus")
	var names []string
	for _, g := range got {
		names = append(names, g.Namespace)
	}
	if !reflect.DeepEqual(names, []string{"msgbus", "msgbus-lab"}) {
		t.Fatalf("namespaces %v, want [msgbus msgbus-lab] (sorted, unique)", names)
	}
}

func TestVersionNamespaces_ATargetWithNoNamespaceUsesTheEnvironmentOne(t *testing.T) {
	got := versionNamespaces(msgbusTargets(), refs("BARE-1", "UNKNOWN-1"), "sut-env")
	if len(got) != 1 || got[0].Namespace != "sut-env" {
		t.Fatalf("got %+v, want the ARGUS_SUT_NAMESPACE namespace for a target that declares none and for an unassigned check", got)
	}
	if !reflect.DeepEqual(got[0].Targets, []string{"bare", "unassigned"}) {
		t.Errorf("targets %v", got[0].Targets)
	}
}

func TestVersionNamespaces_NoTestTargetsIsTheEnvironmentNamespaceAndNamesNoTarget(t *testing.T) {
	got := versionNamespaces(nil, refs("A-1", "B-1"), "sut-env")
	if len(got) != 1 || got[0].Namespace != "sut-env" || len(got[0].Targets) != 0 {
		t.Fatalf("got %+v, want [sut-env] with no target names", got)
	}
}

func TestVersionNamespaces_NothingToNameIsNothing(t *testing.T) {
	if got := versionNamespaces(nil, refs("A-1"), ""); len(got) != 0 {
		t.Fatalf("got %+v: with no declared namespace anywhere there is nothing to read", got)
	}
}

func TestVersionNamespaces_AtMostEightAreRead(t *testing.T) {
	var list testtargets.List
	var ids []string
	for _, c := range "abcdefghijkl" {
		n := "t" + string(c)
		list = append(list, testtargets.Target{Name: n, Namespace: "ns-" + string(c), Match: testtargets.Match{ScenarioPrefixes: []string{strings.ToUpper(string(c)) + "-"}}})
		ids = append(ids, strings.ToUpper(string(c))+"-1")
	}
	if got := versionNamespaces(list, refs(ids...), ""); len(got) != maxVersionReadings {
		t.Fatalf("%d namespaces, want the cap %d", len(got), maxVersionReadings)
	}
}

// fakeNS answers per namespace, counting every call.
type fakeNS struct {
	mu    sync.Mutex
	calls []string
	by    map[string]artifactmeasure.Reading
}

func (f *fakeNS) read(_ context.Context, ns string) artifactmeasure.Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ns)
	return f.by[ns]
}

func TestTakeVersionReadings_KeyIsTheDigestSetAndTheReadingHoldsNothingElse(t *testing.T) {
	f := &fakeNS{by: map[string]artifactmeasure.Reading{
		"msgbus-lab": {Source: "k8s", Digests: []string{mdB, mdA, mdA, mdA}, Unresolved: 1}, // three replicas of A
	}}
	got := takeVersionReadings(context.Background(), ExecConfig{Tier: "k3d", ReadNamespaceVersion: f.read}, msgbusTargets(), refs("LAB-1"), "")
	if len(got) != 1 {
		t.Fatalf("readings %+v", got)
	}
	r := got[0]
	if r.Namespace != "msgbus-lab" || r.Key != artifactmeasure.VersionKey([]string{mdA, mdB}) || r.Digests != 2 || r.Unresolved != 1 || r.Reason != "" {
		t.Fatalf("reading %+v, want key over the 2 distinct digests, 1 unresolved", r)
	}
	// the type has no field for a pod, an image or an environment: pin the wire's key set
	raw, _ := json.Marshal(r)
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"digests", "key", "namespace", "targets", "unresolved"}) {
		t.Errorf("reading carries keys %v", names)
	}
}

func TestTakeVersionReadings_UnreadableNamespaceHasNoKeyAndNamesItself(t *testing.T) {
	f := &fakeNS{by: map[string]artifactmeasure.Reading{
		"msgbus":     {Source: "k8s", Reason: "forbidden: this executor may not list pods in namespace msgbus"},
		"msgbus-lab": {Source: "k8s", Reason: "no in-cluster Kubernetes credentials: token missing"}, // names no namespace itself
	}}
	got := takeVersionReadings(context.Background(), ExecConfig{Tier: "k3d", ReadNamespaceVersion: f.read}, msgbusTargets(), refs("LIVE-1", "LAB-1"), "")
	if len(got) != 2 {
		t.Fatalf("readings %+v", got)
	}
	for _, r := range got {
		if r.Key != "" || r.Digests != 0 {
			t.Errorf("%s: key %q digests %d for an unreadable namespace", r.Namespace, r.Key, r.Digests)
		}
		if !strings.Contains(r.Reason, r.Namespace) {
			t.Errorf("%s: reason %q does not name the namespace", r.Namespace, r.Reason)
		}
	}
}

func TestTakeVersionReadings_NoDigestAnywhereIsNoKeyWithAReason(t *testing.T) {
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"msgbus": {Source: "k8s", Unresolved: 3}}}
	got := takeVersionReadings(context.Background(), ExecConfig{Tier: "k3d", ReadNamespaceVersion: f.read}, msgbusTargets(), refs("LIVE-1"), "")
	if len(got) != 1 || got[0].Key != "" || got[0].Unresolved != 3 || !strings.Contains(got[0].Reason, "msgbus") {
		t.Fatalf("got %+v, want no key, 3 unresolved and a reason naming the namespace (tag-only images name no version)", got)
	}
}

func TestTakeVersionReadings_ComposeIsOneReadingWithNoNamespaceAndNoKubernetesCall(t *testing.T) {
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"": {Source: "compose", Digests: []string{mdC}}}}
	// even with namespaced targets declared, the compose tier makes ONE empty-namespace reading
	got := takeVersionReadings(context.Background(), ExecConfig{Tier: "compose", ReadNamespaceVersion: f.read}, msgbusTargets(), refs("LIVE-1", "LAB-1"), "msgbus")
	if len(got) != 1 || got[0].Namespace != "" || got[0].Key != artifactmeasure.VersionKey([]string{mdC}) {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(f.calls, []string{""}) {
		t.Errorf("the reader was called for %q, want one call with no namespace", f.calls)
	}
}

func TestTakeVersionReadings_NoReaderIsNoReadingsAndTheRunIsNotTouched(t *testing.T) {
	if got := takeVersionReadings(context.Background(), ExecConfig{Tier: "k3d"}, msgbusTargets(), refs("LIVE-1"), ""); got != nil {
		t.Fatalf("got %+v with no reader wired", got)
	}
}

// A reader that never answers must not hold the run: the whole set of readings has its own deadline.
func TestTakeVersionReadings_ASlowReaderIsBoundedAndNeverFailsTheRun(t *testing.T) {
	prev := versionReadBudget
	versionReadBudget = 50 * time.Millisecond
	t.Cleanup(func() { versionReadBudget = prev })
	slow := func(ctx context.Context, ns string) artifactmeasure.Reading {
		<-ctx.Done()
		return artifactmeasure.Reading{Source: "k8s", Reason: "unreachable: " + ctx.Err().Error()}
	}
	start := time.Now()
	got := takeVersionReadings(context.Background(), ExecConfig{Tier: "k3d", ReadNamespaceVersion: slow}, msgbusTargets(), refs("LIVE-1", "LAB-1"), "")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	for _, r := range got {
		if r.Key != "" || r.Reason == "" || !strings.Contains(r.Reason, r.Namespace) {
			t.Errorf("reading %+v", r)
		}
	}
	if len(got) != 2 {
		t.Errorf("got %d readings, want one per namespace even when the budget ran out", len(got))
	}
}

// ---- the run path: NewRunFunc -----------------------------------------------------------------------

func ttConfigFile(t *testing.T, extra string) string {
	return ttCfg(t, extra)
}

const msgbusYAML = "test_targets:\n" +
	"  - {name: live, namespace: msgbus, match: {scenario_prefixes: [LIVE-]}}\n" +
	"  - {name: lab, namespace: msgbus-lab, match: {scenario_prefixes: [LAB-]}}\n" +
	"  - {name: lab-load, namespace: msgbus-lab, match: {scenario_prefixes: [LOAD-]}}\n"

func scenarioBody(id string) federation.ScenarioPayload {
	return federation.ScenarioPayload{Path: "http/" + id + ".md", Body: "# t\n\n## Metadata\n- **ID**: " + id + "\n- **Layers**: http\n"}
}

func runWith(t *testing.T, cfg ExecConfig, a *federation.RunAssignment) (federation.ResultsPush, error) {
	t.Helper()
	cfg.ResultsRoot = t.TempDir()
	return NewRunFunc(cfg)(context.Background(), a)
}

func TestRun_EveryModeCarriesTheReadingsOfTheNamespacesItsScenariosBelongTo(t *testing.T) {
	stubRunCore(t)
	k := newKey(t)
	for _, mode := range []string{"build", "", "scheduled", "final", "rehearsal"} {
		f := &fakeNS{by: map[string]artifactmeasure.Reading{
			"msgbus":     {Source: "k8s", Digests: []string{mdA}},
			"msgbus-lab": {Source: "k8s", Digests: []string{mdB}},
		}}
		a := &federation.RunAssignment{Mode: mode, RunID: "run-v-" + mode, ArtifactDigest: mdA}
		if mode == "final" || mode == "scheduled" || mode == "rehearsal" {
			a.Scenarios = []federation.ScenarioPayload{sealed(t, k, a.RunID, "http/LAB-1.md", "# t\n\n## Metadata\n- **ID**: LAB-1\n"),
				sealed(t, k, a.RunID, "http/LIVE-1.md", "# t\n\n## Metadata\n- **ID**: LIVE-1\n")}
		} else {
			a.Scenarios = []federation.ScenarioPayload{scenarioBody("LAB-1"), scenarioBody("LIVE-1")}
		}
		push, err := runWith(t, ExecConfig{Tier: "k3d", X25519Priv: k, ConfigPath: ttConfigFile(t, msgbusYAML), ReadNamespaceVersion: f.read,
			MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)}, a)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		var got []federation.VersionReading
		if err := json.Unmarshal(push.VersionReadings, &got); err != nil || len(got) != 2 {
			t.Fatalf("mode %q: version_readings %s (%v), want one per namespace", mode, push.VersionReadings, err)
		}
		if got[0].Namespace != "msgbus" || got[1].Namespace != "msgbus-lab" || got[0].Key == got[1].Key || got[0].Key == "" {
			t.Errorf("mode %q: readings %+v", mode, got)
		}
		if !reflect.DeepEqual(got[1].Targets, []string{"lab"}) {
			t.Errorf("mode %q: targets %v, want [lab]", mode, got[1].Targets)
		}
	}
}

// ⛔ A build run's bundle hash must stay byte-identical to today's, and a certifying run's measurement must
// not change by a byte: the readings are NOT evidence.
func TestRun_TheReadingsAreNotEvidence_BundleHashAndMeasurementAreUnchanged(t *testing.T) {
	stubRunCore(t)
	k := newKey(t)
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"msgbus-lab": {Source: "k8s", Digests: []string{mdC}}}}
	for _, mode := range []string{"build", "final"} {
		mk := func() *federation.RunAssignment {
			a := &federation.RunAssignment{Mode: mode, RunID: "run-ev", ArtifactDigest: mdA}
			if mode == "final" {
				a.Scenarios = []federation.ScenarioPayload{sealed(t, k, a.RunID, "http/LAB-1.md", "# t\n\n## Metadata\n- **ID**: LAB-1\n")}
			} else {
				a.Scenarios = []federation.ScenarioPayload{scenarioBody("LAB-1")}
			}
			return a
		}
		cfg := ExecConfig{Tier: "k3d", X25519Priv: k, ConfigPath: ttConfigFile(t, msgbusYAML),
			MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA, mdB}}, nil)}
		without, err := runWith(t, cfg, mk())
		if err != nil {
			t.Fatal(err)
		}
		cfg.ReadNamespaceVersion = f.read
		with, err := runWith(t, cfg, mk())
		if err != nil {
			t.Fatal(err)
		}
		if len(without.VersionReadings) != 0 || len(with.VersionReadings) == 0 {
			t.Fatalf("mode %s: readings without=%s with=%s", mode, without.VersionReadings, with.VersionReadings)
		}
		if with.EvidenceBundleHash != without.EvidenceBundleHash || with.ScenarioEvidenceRoot != without.ScenarioEvidenceRoot ||
			string(with.ArtifactMeasurement) != string(without.ArtifactMeasurement) {
			t.Errorf("mode %s: taking readings moved the evidence: bundle %q vs %q, root %q vs %q, measurement %s vs %s", mode,
				with.EvidenceBundleHash, without.EvidenceBundleHash, with.ScenarioEvidenceRoot, without.ScenarioEvidenceRoot,
				with.ArtifactMeasurement, without.ArtifactMeasurement)
		}
		if mode == "build" && with.EvidenceBundleHash != evidenceBundleHash(with.Scenarios) {
			t.Errorf("a build run's bundle hash is not the scenario-only hash")
		}
	}
}

// RefuseIfMismatch belongs to the artifact measurement alone: readings that disagree with the declared digest
// refuse nothing, and the measurement's own refusal is unchanged.
func TestRun_ReadingsNeverRefuseARun(t *testing.T) {
	stubRunCore(t)
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"msgbus-lab": {Source: "k8s", Digests: []string{mdC}}}} // not the declared digest
	push, err := runWith(t, ExecConfig{Tier: "k3d", ConfigPath: ttConfigFile(t, msgbusYAML), ReadNamespaceVersion: f.read},
		&federation.RunAssignment{Mode: "build", RunID: "run-nr", ArtifactDigest: mdA, Scenarios: []federation.ScenarioPayload{scenarioBody("LAB-1")}})
	if err != nil || len(push.VersionReadings) == 0 {
		t.Fatalf("err %v readings %s", err, push.VersionReadings)
	}
}

func TestRun_ReadingsAreTakenBeforeAnyScenarioRuns(t *testing.T) {
	core := stubRunCore(t)
	rd := func(context.Context, string) artifactmeasure.Reading {
		if *core != 0 {
			t.Error("the namespace was read AFTER a scenario had already run")
		}
		return artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}
	}
	if _, err := runWith(t, ExecConfig{Tier: "k3d", ConfigPath: ttConfigFile(t, msgbusYAML), ReadNamespaceVersion: rd},
		&federation.RunAssignment{Mode: "build", RunID: "run-ord", Scenarios: []federation.ScenarioPayload{scenarioBody("LAB-1")}}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_AnUnreadableNamespaceIsARecordedReasonNotARunFailure(t *testing.T) {
	stubRunCore(t)
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"msgbus-lab": {Source: "k8s", Reason: "forbidden: this executor may not list pods in namespace msgbus-lab"}}}
	push, err := runWith(t, ExecConfig{Tier: "k3d", ConfigPath: ttConfigFile(t, msgbusYAML), ReadNamespaceVersion: f.read},
		&federation.RunAssignment{Mode: "build", RunID: "run-fb", Scenarios: []federation.ScenarioPayload{scenarioBody("LAB-1")}})
	if err != nil || push.Outcome != "passed" {
		t.Fatalf("err %v outcome %q: an unreadable namespace must not fail the run", err, push.Outcome)
	}
	var got []federation.VersionReading
	_ = json.Unmarshal(push.VersionReadings, &got)
	if len(got) != 1 || got[0].Key != "" || !strings.Contains(got[0].Reason, "forbidden") {
		t.Fatalf("readings %s", push.VersionReadings)
	}
}

func TestRun_NoTestTargetsReadsTheEnvironmentNamespace(t *testing.T) {
	stubRunCore(t)
	t.Setenv("ARGUS_SUT_NAMESPACE", "sut-env")
	f := &fakeNS{by: map[string]artifactmeasure.Reading{"sut-env": {Source: "k8s", Digests: []string{mdA}}}}
	push, err := runWith(t, ExecConfig{Tier: "k3d", ConfigPath: ttConfigFile(t, ""), ReadNamespaceVersion: f.read},
		&federation.RunAssignment{Mode: "build", RunID: "run-env", Scenarios: []federation.ScenarioPayload{scenarioBody("ANY-1")}})
	if err != nil {
		t.Fatal(err)
	}
	var got []federation.VersionReading
	_ = json.Unmarshal(push.VersionReadings, &got)
	if len(got) != 1 || got[0].Namespace != "sut-env" || len(got[0].Targets) != 0 || got[0].Key == "" {
		t.Fatalf("readings %s", push.VersionReadings)
	}
}

func TestRun_AnExecutorWithNoReaderSendsNothingAndTheFieldIsOmitted(t *testing.T) {
	stubRunCore(t)
	push, err := runWith(t, ExecConfig{Tier: "k3d"}, &federation.RunAssignment{Mode: "build", RunID: "run-nr2"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(push)
	if len(push.VersionReadings) != 0 || strings.Contains(string(raw), "version_readings") {
		t.Fatalf("push %s", raw)
	}
}

// 🚩 Wiring: a correct reader that Bootstrap never installs ships dead. The field is read by NewRunFunc, which
// takes ExecConfig BY VALUE, so it must be set before the executor literal (the SutFingerprint trap).
func TestBootstrapInstallsTheNamespaceVersionReader(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "cfg.Fed.Exec.ReadNamespaceVersion = ")
	j := strings.Index(s, "Run:    NewRunFunc(cfg.Fed.Exec)")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("Bootstrap must assign cfg.Fed.Exec.ReadNamespaceVersion BEFORE NewRunFunc(cfg.Fed.Exec) copies the struct (assignment at %d, NewRunFunc at %d)", i, j)
	}
}

func TestNamespaceVersionReader_TierComesFromTheExecutorsOwnConfig(t *testing.T) {
	r := namespaceVersionReader(ExecConfig{Tier: ""})(context.Background(), "x")
	if !strings.Contains(r.Reason, "tier") {
		t.Fatalf("reading %+v", r)
	}
}

func TestVersionReadingsFile_IsNotUnderTheEvidencePath(t *testing.T) {
	// the readings are assigned after bindMeasurement's inputs are fixed; this reads the source so a refactor that
	// feeds them into the bundle shows up as a failing test, not a silent hash change.
	src, err := os.ReadFile(filepath.Join(".", "artifact_measure.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "VersionReadings") || strings.Contains(string(src), "versionReadings") {
		t.Fatal("artifact_measure.go (the bundle binding) mentions the version readings")
	}
}
