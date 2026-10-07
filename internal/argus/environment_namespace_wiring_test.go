package argus

import (
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// ARGUS-TA-5 wiring: the pure rule is tested in environment_namespace_test.go; these go
// through the real entry points (RunAll and the per-step capture of an AMQP Load check) so that a call site
// that stops handing over the test targets turns them red.

const taTargetBlock = "test_targets:\n  - name: lab-load\n    namespace: other-ns\n    match: { scenario_prefixes: [AL-] }\n"

// A load run outside a cluster still reports the namespace it chose: NewInClusterClient fails and the
// Capture is {Namespace: ns, Reason: "no in-cluster Kubernetes credentials: ..."}.
func TestTA5_RunAll_EnvironmentNamespaceIsTheLoadChecksTargetNamespace(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "env-ns")
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	cfg := loadConfig(t, allowLab) + taTargetBlock
	rr, err := RunAll(loadCfg(t, cfg), filepath.Join(e.dir, "scenarios"), e.results, "lab", "", "", "", "20261002T120000000",
		&loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }})
	if err != nil {
		t.Fatal(err)
	}
	env := rr.Report.Environment
	if env == nil {
		t.Fatal("a load run carries no environment")
	}
	if env.Namespace != "other-ns" {
		t.Fatalf("Environment.Namespace = %q (reason %q), want other-ns, the namespace of the check's test target, not ARGUS_SUT_NAMESPACE", env.Namespace, env.Reason)
	}
}

func TestTA5_AMQPLoadPerStepCaptureReadsTheChecksTargetNamespace(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "env-ns")
	e := newLoadEnv(t)
	var got []string
	amqpLoadEnvCapture = func(ns string) *envcapture.Capture { got = append(got, ns); return nil }
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	e.run(t, loadConfig(t, allowLab)+taTargetBlock, f)
	if len(got) != 4 {
		t.Fatalf("capture calls = %d (%v), want 2 per step x 2 steps", len(got), got)
	}
	for i, ns := range got {
		if ns != "other-ns" {
			t.Errorf("call %d read namespace %q, want other-ns", i, ns)
		}
	}
}

func TestTA5_AMQPLoadPerStepCaptureWithoutTargetNamespaceReadsEnvVar(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "env-ns")
	e := newLoadEnv(t)
	var got []string
	amqpLoadEnvCapture = func(ns string) *envcapture.Capture { got = append(got, ns); return nil }
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	e.run(t, loadConfig(t, allowLab), &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }})
	if len(got) != 2 || got[0] != "env-ns" || got[1] != "env-ns" {
		t.Fatalf("namespaces read = %v, want env-ns twice", got)
	}
}

func TestTA5_DefaultAMQPLoadEnvCapture_EmptyNamespaceReadsNothing(t *testing.T) {
	if c := defaultAMQPLoadEnvCapture(""); c != nil {
		t.Fatalf("capture = %+v, want nil", c)
	}
}
