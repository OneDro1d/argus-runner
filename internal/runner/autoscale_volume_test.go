package runner

import (
	"os"
	"strings"
	"testing"
)

// #215: an executor whose results volume is not ReadWriteMany must not scale past one replica. The
// extra pods land on other nodes and sit in ContainerCreating with a Multi-Attach error, measured
// 2026-09-24 on argus-inst-shop-uni-arb (managed-csi, ReadWriteOnce): run 20260924T131700077 scaled
// 1→3 and reported "1/3 replicas READY" for the whole run.
func TestAC215_ActiveReplicasFollowTheResultsVolume(t *testing.T) {
	cases := []struct {
		mode       string
		want       int
		wantCapped bool
	}{
		{"ReadWriteMany", 3, false},
		{"ReadWriteOnce", 1, true},
		// Instances rendered before the env existed carry none, and some of them ARE ReadWriteMany:
		// unknown keeps today's behaviour rather than silently dropping their min-3.
		{"", 3, false},
		{"ReadWriteOncePod", 1, true},
	}
	for _, c := range cases {
		got, capped := activeReplicasForVolume(c.mode, 3)
		if got != c.want || capped != c.wantCapped {
			t.Errorf("activeReplicasForVolume(%q, 3) = (%d, %v), want (%d, %v)", c.mode, got, capped, c.want, c.wantCapped)
		}
	}
}

// The wiring, not only the rule: newAutoscalerFromEnv is what runner.go calls, so a test of it fails
// if the call site stops applying the cap.
func TestAC215_TheAutoscalerTheExecutorBuildsIsCappedOnRWO(t *testing.T) {
	t.Setenv("ARGUS_RESULTS_ACCESS_MODE", "ReadWriteOnce")
	t.Setenv("ARGUS_SCALE_ACTIVE", "")
	as := newAutoscalerFromEnv("ns", func(string, ...any) {})
	if as.ActiveReplicas != 1 {
		t.Fatalf("ActiveReplicas = %d on a ReadWriteOnce volume, want 1", as.ActiveReplicas)
	}

	t.Setenv("ARGUS_RESULTS_ACCESS_MODE", "ReadWriteMany")
	as = newAutoscalerFromEnv("ns", func(string, ...any) {})
	if as.ActiveReplicas != 0 { // 0 = the package default (3) applies in defaults()
		t.Fatalf("ActiveReplicas = %d on a ReadWriteMany volume, want 0 (default min-3)", as.ActiveReplicas)
	}
}

// The executor's only autoscaler construction goes through newAutoscalerFromEnv. A hand-built
// Autoscaler at the call site would skip the cap while every test above stayed green.
func TestAC215_RunnerBuildsItsAutoscalerThroughTheCappedConstructor(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "s.exec.Autoscale = newAutoscalerFromEnv(") {
		t.Error("runner.go no longer assigns s.exec.Autoscale from newAutoscalerFromEnv — the #215 cap is not wired")
	}
	if strings.Contains(s, "&Autoscaler{") {
		t.Error("runner.go builds an Autoscaler literal directly — it bypasses the #215 cap")
	}
}

// An operator's explicit ARGUS_SCALE_ACTIVE still wins: the cap is a safe default, not a lock.
func TestAC215_AnExplicitScaleActiveOverridesTheCap(t *testing.T) {
	t.Setenv("ARGUS_RESULTS_ACCESS_MODE", "ReadWriteOnce")
	t.Setenv("ARGUS_SCALE_ACTIVE", "2")
	as := newAutoscalerFromEnv("ns", func(string, ...any) {})
	if as.ActiveReplicas != 2 {
		t.Fatalf("ActiveReplicas = %d with ARGUS_SCALE_ACTIVE=2, want 2", as.ActiveReplicas)
	}
}
