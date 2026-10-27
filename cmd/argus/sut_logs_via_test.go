package main

// sut_logs_via_test.go —: `render-k8s --collect-sut-logs --sut-logs-via node|api`, the
// notices, and the `collect-logs` command's own wiring.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
)

func readOut(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestRenderK8s_SUTLogsViaAPI_RendersTheCollectorAndTheSecondRole(t *testing.T) {
	rc, out, dir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--sut-logs-via", "api", "--emit-sut-access-role")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, out)
	}
	obs := readOut(t, dir, "obs.yaml")
	for _, bad := range []string{"hostPath", "DaemonSet", "ClusterRole", "name: promtail", "app: promtail", "/var/log/pods"} {
		if strings.Contains(obs, bad) {
			t.Errorf("obs.yaml contains %q", bad)
		}
	}
	for _, want := range []string{"kind: Deployment\nmetadata:\n  name: sut-logs", "args: [collect-logs, --config, /config/argus-config.yaml, --loki, http://loki:3100]", "name: argus-sut-logs"} {
		if !strings.Contains(obs, want) {
			t.Errorf("obs.yaml lacks %q", want)
		}
	}
	role := readOut(t, dir, "sut-access-role.yaml")
	if strings.Count(role, "kind: Role\n") != 2 || !strings.Contains(role, "resources: [pods/log]") || !strings.Contains(role, "name: argus-sut-logs-read") {
		t.Errorf("sut-access-role.yaml:\n%s", role)
	}
	// the notices: which namespace is read, and that its owner must apply the second Role
	for _, want := range []string{`"sut-app-ns"`, "deployment/sut-logs", "SECOND Role", "argus-sut-logs-read", "collects nothing"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "promtail will read") {
		t.Errorf("stderr still says promtail will read:\n%s", stderr)
	}
}

// Without --emit-sut-access-role the notice says how to get the Role.
func TestRenderK8s_SUTLogsViaAPI_WithoutEmitSaysHowToGetTheRole(t *testing.T) {
	rc, out, _, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--sut-logs-via", "api")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, out)
	}
	if !strings.Contains(stderr, "render it with --emit-sut-access-role") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestRenderK8s_SUTLogsViaAPI_WithoutCollectSUTLogsIsAUsageErrorByName(t *testing.T) {
	rc, out, dir, _, _ := renderK8sRaw(t, noObsConfig, "--sut-logs-via", "api")
	if rc != exitUsage {
		t.Fatalf("exit %d, want exitUsage: %+v", rc, out)
	}
	if !strings.Contains(out.Error, "--sut-logs-via api") || !strings.Contains(out.Error, "--collect-sut-logs") {
		t.Errorf("error = %q", out.Error)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files before refusing", len(entries))
	}
}

func TestRenderK8s_SUTLogsViaOtherValuesAreRefusedByName(t *testing.T) {
	for _, v := range []string{"sideways", "API ", "daemonset", "hostpath"} {
		rc, out, dir, _, _ := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--sut-logs-via", v)
		if v == "API " { // case and surrounding space are tolerated, like --obs
			if rc != exitOK {
				t.Errorf("%q: exit %d %+v", v, rc, out)
			}
			continue
		}
		if rc != exitUsage || !strings.Contains(out.Error, "must be node or api") || !strings.Contains(out.Error, v) {
			t.Errorf("%q: exit %d error %q", v, rc, out.Error)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q: wrote files", v)
		}
	}
}

// `node` is today's render, spelled out or not: the files and the stderr line are the same bytes.
func TestRenderK8s_SUTLogsViaNode_IsByteIdenticalToTheDefault(t *testing.T) {
	_, _, d1, _, e1 := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--emit-sut-access-role")
	_, _, d2, _, e2 := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--sut-logs-via", "node", "--emit-sut-access-role")
	for _, f := range []string{"executor.yaml", "obs.yaml", "sut-access-role.yaml"} {
		if readOut(t, d1, f) != readOut(t, d2, f) {
			t.Errorf("%s differs between the default and --sut-logs-via node", f)
		}
	}
	const old = "collect-sut-logs: ON — promtail will read every pod log under namespace \"sut-app-ns\" (SUT_NAMESPACE)\n"
	if !strings.Contains(e1, old) || !strings.Contains(e2, old) {
		t.Errorf("the node notice changed:\n%s\n%s", e1, e2)
	}
	if !strings.Contains(readOut(t, d1, "obs.yaml"), "kind: DaemonSet") {
		t.Error("node no longer renders promtail")
	}
}

func TestRenderK8s_SUTLogsViaAPI_ExportAndSharedModes(t *testing.T) {
	exportCfg := noObsConfig + "observability:\n  loki:\n    url: https://logs.example.invalid\n    push_url: https://logs.example.invalid/loki/api/v1/push\n    credential: ${LOKI_CREDENTIAL}\n"
	t.Setenv("LOKI_CREDENTIAL", "user:s3cr3t-value")
	rc, out, dir, _, _ := renderK8sRaw(t, exportCfg, "--collect-sut-logs", "--sut-logs-via", "api", "--obs", "export")
	if rc != exitOK {
		t.Fatalf("export: exit %d %+v", rc, out)
	}
	obs := readOut(t, dir, "obs.yaml")
	if strings.Contains(obs, "s3cr3t-value") || !strings.Contains(obs, "secretKeyRef: {name: argus-obs-credential, key: LOKI_CREDENTIAL}") {
		t.Errorf("export credential:\n%s", obs)
	}
	if strings.Contains(obs, "hostPath") || strings.Contains(obs, "DaemonSet") {
		t.Errorf("export api rendered a hostPath or DaemonSet")
	}
	rc, out, dir, _, _ = renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--sut-logs-via", "api", "--obs", "shared", "--loki-tenant", "podmon-obslogs-test")
	if rc != exitOK {
		t.Fatalf("shared: exit %d %+v", rc, out)
	}
	obs = readOut(t, dir, "obs.yaml")
	if !strings.Contains(obs, "--loki-tenant, \"podmon-obslogs-test\"") || strings.Contains(obs, "hostPath") {
		t.Errorf("shared:\n%s", obs)
	}
}

// upgrade says, by name, what it leaves behind.
func TestPromtailLeftoverNotice_NamesTheThreeDeletes(t *testing.T) {
	n := promtailLeftoverNotice("argus-inst-x")
	for _, want := range []string{
		"kubectl -n argus-inst-x delete daemonset/promtail",
		"kubectl -n argus-inst-x delete configmap/promtail-config",
		"kubectl -n argus-inst-x delete serviceaccount/promtail",
		"shipped TWICE",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("notice lacks %q:\n%s", want, n)
		}
	}
}

func TestUpgradeUsageNamesTheFlagAndHelpListsIt(t *testing.T) {
	_, out := runHelp(t, "render-k8s", "--help")
	if !hasFlagToken(out, "sut-logs-via") {
		t.Errorf("render-k8s --help does not list --sut-logs-via:\n%s", out)
	}
	rc, out := runHelp(t, "collect-logs", "--help")
	if rc != exitOK || !hasFlagToken(out, "loki") {
		t.Errorf("collect-logs --help: rc %d\n%s", rc, out)
	}
}

// ── collect-logs wiring ───────────────────────────────────────────────────────────────────────────

func collectCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanCollectLogs_ReadsTheExecutorsLokiAndTheLabelRules(t *testing.T) {
	cfg := collectCfg(t, noObsConfig+"observability:\n  loki:\n    url: https://logs.example.invalid/\n    push_url: https://logs.example.invalid/loki/api/v1/push\n    credential: ${LOKI_CREDENTIAL}\n    log_format: logfmt\n    level_field: severity\n    saga_event_values: [a, b]\n")
	t.Setenv("ARGUS_INSTANCE_ID", "inst-1")
	t.Setenv("ARGUS_SUT_NAMESPACE", "sut-ns")
	t.Setenv("ARGUS_CLUSTER", "aks")
	t.Setenv("POD_NAME", "sut-logs-abc")
	t.Setenv("POD_NAMESPACE", "argus-inst-inst-1")
	t.Setenv("LOKI_CREDENTIAL", "u:p")
	// the SUT's own ${VARs} are NOT given to this process, and must not matter
	cf := &commonFlags{configPath: cfg, loki: "https://logs.example.invalid", obsMode: "export"}
	p, err := planCollectLogs(cf)
	if err != nil {
		t.Fatal(err)
	}
	if p.Loki.Credential != "u:p" || p.Loki.PushURL != "https://logs.example.invalid/loki/api/v1/push" || p.Loki.Tenant != "" {
		t.Errorf("loki = %+v", p.Loki)
	}
	r := p.Rules
	if r.Instance != "inst-1" || r.Cluster != "aks" || r.Project != "obs-modes-test" || r.Format != "logfmt" || r.LevelField != "severity" || strings.Join(r.SagaMarkers, ",") != "a,b" {
		t.Errorf("rules = %+v", r)
	}
	if p.Namespace != "sut-ns" || p.SelfPod != "sut-logs-abc" || p.SelfNS != "argus-inst-inst-1" {
		t.Errorf("plan = %+v", p)
	}
	// a --loki that is NOT the configured Loki never receives the configured credential or push url
	cf2 := &commonFlags{configPath: cfg, loki: "http://loki:3100"}
	p2, err := planCollectLogs(cf2)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Loki.Credential != "" || p2.Loki.PushURL != "" {
		t.Errorf("credential or push url handed to a different Loki: %+v", p2.Loki)
	}
}

func TestPlanCollectLogs_Refusals(t *testing.T) {
	cfg := collectCfg(t, noObsConfig)
	good := func() {
		t.Setenv("ARGUS_INSTANCE_ID", "i")
		t.Setenv("ARGUS_SUT_NAMESPACE", "n")
	}
	t.Setenv("ARGUS_INSTANCE_ID", "")
	t.Setenv("ARGUS_SUT_NAMESPACE", "n")
	if _, err := planCollectLogs(&commonFlags{configPath: cfg, loki: "http://l"}); err == nil || !strings.Contains(err.Error(), "ARGUS_INSTANCE_ID") {
		t.Errorf("no instance id: %v", err)
	}
	t.Setenv("ARGUS_INSTANCE_ID", "i")
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	if _, err := planCollectLogs(&commonFlags{configPath: cfg, loki: "http://l"}); err == nil || !strings.Contains(err.Error(), "ARGUS_SUT_NAMESPACE") {
		t.Errorf("no namespace: %v", err)
	}
	good()
	if _, err := planCollectLogs(&commonFlags{configPath: cfg, loki: "http://l", obsMode: "shared"}); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Errorf("shared with no tenant: %v", err)
	}
	if _, err := planCollectLogs(&commonFlags{configPath: cfg, loki: ""}); err == nil {
		t.Error("an empty --loki was accepted")
	}
	cred := collectCfg(t, noObsConfig+"observability:\n  loki:\n    url: https://h.invalid\n    credential: ${NOT_SET_ANYWHERE}\n")
	t.Setenv("NOT_SET_ANYWHERE", "")
	_, err := planCollectLogs(&commonFlags{configPath: cred, loki: "https://h.invalid"})
	if err == nil || !strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Errorf("unresolvable credential: %v", err)
	}
	if rc := dispatch([]string{"collect-logs"}); rc != exitUsage {
		t.Errorf("collect-logs with no --config: rc %d", rc)
	}
}

func TestCollectSUTLogsNotice_NodeIsTheOldLine(t *testing.T) {
	in := k8srender.Instance{SUTNamespace: "ns1"}
	if got, want := collectSUTLogsNotice(in, false), "collect-sut-logs: ON — promtail will read every pod log under namespace \"ns1\" (SUT_NAMESPACE)\n"; got != want {
		t.Errorf("got %q", got)
	}
}
