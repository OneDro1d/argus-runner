package main

// sut_logs_review_test.go — the independent review of cmd/argus side: the notice and the
// second Role follow ONE function (k8srender.Instance.SUTLogsCollectorRendered), and `argus collect-logs`
// outside a pod says so in one sentence.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
)

const adoptCfg = "observability:\n  loki:\n    url: http://own-loki.invalid:3100\n"

// FIX 1: every tier x obs mode. Whether a collector is rendered is read off the rendered obs.yaml (what the
// renderer really emitted); the notice and the Role file must agree with it, and with the one function.
func TestRenderK8s_SUTLogsViaAPI_NoticeAndRoleFollowTheOneFunction(t *testing.T) {
	t.Setenv("OBS_EXPORT_MARKER_VAR", "u:p")
	modes := []struct {
		name      string
		cfg       string
		args      []string
		rendered  bool   // the specification, by hand
		modeInMsg string // what the one-sentence notice names
	}{
		{"bundled", noObsConfig, nil, true, ""},
		{"shared", noObsConfig, []string{"--obs", "shared", "--loki-tenant", "podmon-obslogs-test"}, true, ""},
		{"export-hosted", exportHostedLokiConfig, []string{"--obs", "export"}, true, ""},
		{"adopt", noObsConfig + adoptCfg, []string{"--obs", "adopt"}, false, "--obs adopt"},
		{"none", noObsConfig, []string{"--obs", "none"}, false, "--obs none"},
		{"export-betterstack", exportBetterStackConfig, []string{"--obs", "export"}, false, "--obs export to BetterStack"},
	}
	for _, tier := range []string{"k3d", "aks"} {
		for _, m := range modes {
			t.Run(tier+"/"+m.name, func(t *testing.T) {
				base := append([]string{"--tier", tier, "--collect-sut-logs", "--emit-sut-access-role"}, m.args...)
				_, _, dNode, _, _ := renderK8sRaw(t, m.cfg, base...)
				rc, out, d, _, stderr := renderK8sRaw(t, m.cfg, append(base, "--sut-logs-via", "api")...)
				if rc != exitOK {
					t.Fatalf("exit %d: %+v", rc, out)
				}
				obs := readOutOrEmpty(d, "obs.yaml")
				renderedByRenderer := strings.Contains(obs, "name: sut-logs\n")
				if renderedByRenderer != m.rendered {
					t.Fatalf("renderer emitted the collector = %v, the spec for %s says %v", renderedByRenderer, m.name, m.rendered)
				}
				role := readOut(t, d, "sut-access-role.yaml")
				hasSecond := strings.Contains(role, "argus-sut-logs-read")
				willRead := strings.Contains(stderr, "will read")
				mustApply := strings.Contains(stderr, "must apply")
				if m.rendered {
					if !hasSecond || !willRead || !mustApply {
						t.Errorf("collector rendered: second Role %v, 'will read' %v, 'must apply' %v:\n%s", hasSecond, willRead, mustApply, stderr)
					}
					return
				}
				if hasSecond || willRead || mustApply {
					t.Errorf("no collector rendered: second Role %v, 'will read' %v, 'must apply' %v:\n%s", hasSecond, willRead, mustApply, stderr)
				}
				if role != readOut(t, dNode, "sut-access-role.yaml") {
					t.Errorf("sut-access-role.yaml differs from the one without --sut-logs-via api")
				}
				if !strings.Contains(stderr, "no log collector is rendered in "+m.modeInMsg) {
					t.Errorf("stderr does not say no collector is rendered in %q:\n%s", m.modeInMsg, stderr)
				}
				if n := strings.Count(stderr, "collect-sut-logs:"); n != 1 {
					t.Errorf("want ONE collect-sut-logs sentence, got %d:\n%s", n, stderr)
				}
			})
		}
	}
}

// The notice is worded from the same function the renderer gates on: a table over Instance values.
func TestCollectSUTLogsNotice_FollowsSUTLogsCollectorRendered(t *testing.T) {
	for _, tc := range []struct {
		mode string
		bs   bool
		want bool
	}{
		{"", false, true}, {"bundled", false, true}, {"shared", false, true}, {"export", false, true},
		{"export", true, false}, {"adopt", false, false}, {"none", false, false},
	} {
		in := k8srender.Instance{SUTNamespace: "ns1", CollectSUTLogs: true, SUTLogsVia: "api", ObsMode: tc.mode, ObsUseBetterStack: tc.bs}
		if got := in.SUTLogsCollectorRendered(); got != tc.want {
			t.Errorf("%q bs=%v: SUTLogsCollectorRendered = %v, want %v", tc.mode, tc.bs, got, tc.want)
		}
		n := collectSUTLogsNotice(in, true)
		if has := strings.Contains(n, "will read"); has != in.SUTLogsCollectorRendered() {
			t.Errorf("%q bs=%v: notice 'will read' = %v:\n%s", tc.mode, tc.bs, has, n)
		}
		if has := strings.Contains(n, "must apply"); has != in.SUTLogsCollectorRendered() {
			t.Errorf("%q bs=%v: notice 'must apply' = %v:\n%s", tc.mode, tc.bs, has, n)
		}
	}
}

// FIX 5: `argus collect-logs` where there is no ServiceAccount directory. The command function runs with a
// directory that does not exist (the real binary is never run here: this machine is itself a pod).
func TestCmdCollectLogs_OutsideAPod_OneSentenceNonZeroNoNetwork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := collectCfg(t, noObsConfig)
	t.Setenv("ARGUS_INSTANCE_ID", "inst-1")
	t.Setenv("ARGUS_SUT_NAMESPACE", "sut-ns")
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1") // a cluster address that would be tried if anything dialled
	old := collectLogsServiceAccountDir
	collectLogsServiceAccountDir = filepath.Join(t.TempDir(), "no-such-serviceaccount-dir")
	defer func() { collectLogsServiceAccountDir = old }()
	var rc int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			rc = cmdCollectLogs(&commonFlags{configPath: cfg, loki: srv.URL})
		})
	})
	if rc == exitOK {
		t.Fatalf("exit code 0 outside a pod; stdout %q", stdout)
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON error: %v\n%q", err, stdout)
	}
	if !strings.Contains(out.Error, "not in a pod with a ServiceAccount") {
		t.Errorf("error = %q, want it to say the process is not in a pod with a ServiceAccount", out.Error)
	}
	if strings.Contains(out.Error, "\n") || strings.Count(out.Error, ". ") > 0 {
		t.Errorf("error is more than one sentence: %q", out.Error)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("stderr is not empty (the one sentence is the JSON error): %q", stderr)
	}
	if hits.Load() != 0 {
		t.Errorf("%d network calls were made to Loki", hits.Load())
	}
}

// readOutOrEmpty is readOut for a file that a mode may legitimately not write.
func readOutOrEmpty(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}
