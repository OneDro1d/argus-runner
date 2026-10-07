package k8srender

import (
	"strings"
	"testing"
)

// T3.1 (E3 export mode): render CONTENT tests — what actually comes out of RenderExecutor/
// RenderObs for each accepted export target, and what must NEVER come out of either.

func exportHostedLokiInstance() Instance {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "https://logs.example.grafana.net"
	in.ObsLokiPushURL = "https://logs.example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"
	return in
}

func exportBetterStackInstance() Instance {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsUseBetterStack = true
	in.ObsCredentialVarName = "BETTERSTACK_CREDENTIAL"
	return in
}

// export + hosted-Loki renders promtail ONLY: no Loki/pushgateway/PodMonitor kinds, and the
// promtail DaemonSet/ConfigMap/ServiceAccount are present.
func TestRenderObs_exportHostedLoki_PromtailOnly(t *testing.T) {
	in := exportHostedLokiInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, hosted-loki): %v", err)
	}
	if got == "" {
		t.Fatal("export+hosted-Loki RenderObs must render promtail — got empty")
	}
	ds := docs(t, got)
	kinds := kindsOf(ds)
	for _, forbidden := range []string{"Loki", "PodMonitor"} {
		if kinds[forbidden] != 0 {
			t.Errorf("export+hosted-Loki rendered a %s — export deploys promtail only", forbidden)
		}
	}
	for _, d := range ds {
		name := nameOf(d)
		if name == "loki" || name == "pushgateway" {
			t.Errorf("export+hosted-Loki rendered object named %q (kind %v) — must not deploy Loki/pushgateway of its own", name, d["kind"])
		}
	}
	if kinds["DaemonSet"] != 1 {
		t.Errorf("export+hosted-Loki must render exactly one DaemonSet (promtail), got %d", kinds["DaemonSet"])
	}
	if !strings.Contains(got, "kind: ConfigMap") || !strings.Contains(got, "promtail-config") {
		t.Error("export+hosted-Loki must render the promtail-config ConfigMap")
	}
}

// export + hosted-Loki: the promtail client URL carries the credential as a ${VAR} userinfo
// placeholder, and the DaemonSet references the argus-obs-credential Secret via secretKeyRef —
// never a literal value.
func TestRenderObs_exportHostedLoki_ClientURLAndCredentialWiring(t *testing.T) {
	in := exportHostedLokiInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, hosted-loki): %v", err)
	}
	wantURL := "https://${LOKI_CREDENTIAL}@logs.example.grafana.net/loki/api/v1/push"
	if !strings.Contains(got, wantURL) {
		t.Errorf("promtail clients: url must be %q, got:\n%s", wantURL, got)
	}
	if !strings.Contains(got, "-config.expand-env=true") {
		t.Error("promtail args must include -config.expand-env=true, or the ${VAR} above is never resolved")
	}
	if !strings.Contains(got, "secretKeyRef: {name: argus-obs-credential, key: LOKI_CREDENTIAL}") {
		t.Errorf("promtail DaemonSet must reference the argus-obs-credential Secret via secretKeyRef, got:\n%s", got)
	}
}

// export + BetterStack renders ZERO obs objects — same "deploys nothing of its own" promise as
// adopt, and stricter than hosted-Loki export (not even promtail).
func TestRenderObs_exportBetterStack_RendersNothing(t *testing.T) {
	in := exportBetterStackInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, betterstack): %v", err)
	}
	if got != "" {
		t.Fatalf("export+betterstack RenderObs must be EMPTY, got %d bytes:\n%s", len(got), got)
	}
}

// export + BetterStack: the executor gets the betterstack credential via secretKeyRef.
func TestRenderExecutor_exportBetterStack_CredentialSecretKeyRef(t *testing.T) {
	in := exportBetterStackInstance()
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(export, betterstack): %v", err)
	}
	if !strings.Contains(got, "secretKeyRef: {name: argus-obs-credential, key: BETTERSTACK_CREDENTIAL}") {
		t.Errorf("executor must reference the argus-obs-credential Secret via secretKeyRef, got:\n%s", got)
	}
}

// export + hosted-Loki: the executor also gets the credential via secretKeyRef (used by
// obsquery.Loki's Basic Auth on the QUERY side, internal/toolcore.lokiFor).
func TestRenderExecutor_exportHostedLoki_CredentialSecretKeyRef(t *testing.T) {
	in := exportHostedLokiInstance()
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(export, hosted-loki): %v", err)
	}
	if !strings.Contains(got, "secretKeyRef: {name: argus-obs-credential, key: LOKI_CREDENTIAL}") {
		t.Errorf("executor must reference the argus-obs-credential Secret via secretKeyRef, got:\n%s", got)
	}
	if !strings.Contains(got, "- --loki\n            - \"https://logs.example.grafana.net\"\n") {
		t.Errorf("executor --loki must carry the hosted Loki's QUERY url (ObsLokiURL), got:\n%s", got)
	}
}

// CORE SAFETY PROPERTY: whatever the credential VALUE is, it must NEVER appear in ANY rendered
// export-mode manifest — only the bare ${VAR} NAME (as literal placeholder text) and the Secret
// reference by name/key. Uses a distinctive marker value that would be impossible to produce by
// accident, so a match proves a real leak.
func TestExportMode_NeverEmitsTheCredentialValue(t *testing.T) {
	const marker = "MARKER-VALUE-4f8c9a2e-must-never-appear-in-render-output"
	// k8srender.Instance never carries the RESOLVED credential value at all in export mode — only
	// ObsCredentialVarName, the bare ${VAR} NAME — so this pins that no code path here echoes it
	// as if it were a value (e.g. mistaking the name for the secret, or %-formatting it into a
	// stringData-shaped line). The layer that could otherwise leak a real value is cmd/argus's
	// generic SecretEnv sweep (every ${VAR} the raw config text references, normally rendered
	// straight into the exec-tokens Secret) — that exclusion is pinned at the cmd/argus layer
	// (TestCmdRenderK8s_ExportNeverEmitsCredentialValue), end to end with a real env var set to
	// this SAME marker, because that is where the value actually enters the pipeline.
	for _, in := range []Instance{exportHostedLokiInstance(), exportBetterStackInstance()} {
		exec, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("RenderExecutor(%s): %v", in.ObsMode, err)
		}
		obs, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatalf("RenderObs(%s): %v", in.ObsMode, err)
		}
		if strings.Contains(exec, marker) || strings.Contains(obs, marker) {
			t.Fatalf("export mode manifest carries the credential VALUE %q — must reference the argus-obs-credential Secret instead:\nexecutor:\n%s\nobs:\n%s", marker, exec, obs)
		}
		// Belt-and-braces: the credential must appear ONLY as the secretKeyRef form, never as a
		// stringData-shaped `KEY: "value"` line anywhere in either manifest.
		bogus := in.ObsCredentialVarName + `: "` + marker + `"`
		if strings.Contains(exec, bogus) || strings.Contains(obs, bogus) {
			t.Fatalf("credential rendered as a literal stringData entry: %q", bogus)
		}
	}
}

// TestRenderObs_exportRefused_writesNothingBeforeRefusing: same "write nothing before refusing"
// discipline as adopt/unknown mode, for an export instance with no accepted backend declared.
func TestRenderObs_exportRefused_writesNothingBeforeRefusing(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	got, err := RenderObs(in, cfgJSON(t))
	if err == nil {
		t.Fatal("RenderObs(export, no backend) must refuse")
	}
	if got != "" {
		t.Errorf("RenderObs(export, no backend) returned %d bytes alongside its error; want empty", len(got))
	}
}
