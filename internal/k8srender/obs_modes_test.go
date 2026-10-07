package k8srender

import (
	"strings"
	"testing"
)

// ── T3.1: one switch, three values, default bundled, bundled == today's behaviour ─────────────

// TestRenderExecutor_bundledIsByteIdenticalToNoObsMode pins the T3.1 promise directly: an
// Instance that never sets ObsMode at all (every caller before this ticket) and one that sets it
// to "bundled" explicitly must render EXACTLY the same bytes.
func TestRenderExecutor_bundledIsByteIdenticalToNoObsMode(t *testing.T) {
	noMode := sampleInstance()
	bundled := sampleInstance()
	bundled.ObsMode = "bundled"

	gotNoMode, err := RenderExecutor(noMode)
	if err != nil {
		t.Fatalf("RenderExecutor(no ObsMode): %v", err)
	}
	gotBundled, err := RenderExecutor(bundled)
	if err != nil {
		t.Fatalf("RenderExecutor(bundled): %v", err)
	}
	if gotNoMode != gotBundled {
		t.Fatalf("--obs bundled must render byte-identically to no --obs flag at all; they differ")
	}
	// And the bundled args are the ORIGINAL hardcoded bundled endpoints, unchanged.
	for _, want := range []string{"            - --loki\n            - http://loki:3100\n",
		"            - --pushgateway\n            - http://pushgateway:9091\n"} {
		if !strings.Contains(gotBundled, want) {
			t.Errorf("bundled executor manifest missing the original hardcoded line %q", want)
		}
	}
}

// TestRenderObs_bundledIsByteIdenticalToNoObsMode is RenderObs's half of the same promise.
func TestRenderObs_bundledIsByteIdenticalToNoObsMode(t *testing.T) {
	noMode := sampleInstance()
	bundled := sampleInstance()
	bundled.ObsMode = "bundled"

	gotNoMode, err := RenderObs(noMode, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(no ObsMode): %v", err)
	}
	gotBundled, err := RenderObs(bundled, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(bundled): %v", err)
	}
	if gotNoMode != gotBundled {
		t.Fatal("--obs bundled RenderObs output must be byte-identical to no --obs flag at all")
	}
	if gotBundled == "" {
		t.Fatal("bundled RenderObs must still render the obs plane (Loki/pushgateway/promtail) — got empty")
	}
}

// ── T3.2: adopt deploys nothing of its own, and points the executor at the operator's own ──────

// TestRenderExecutor_adoptUsesConfiguredLokiAndPushgateway pins the executor arg shape in adopt
// mode: both endpoints come from the Instance (which cmd/argus populates from the SUT's own
// argus-config.yaml), never the bundled defaults.
func TestRenderExecutor_adoptUsesConfiguredLokiAndPushgateway(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "adopt"
	in.ObsLokiURL = "http://operator-loki.example:3100"
	in.ObsPushgatewayURL = "http://operator-pushgateway.example:9091"

	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(adopt): %v", err)
	}
	if strings.Contains(got, "http://loki:3100") || strings.Contains(got, "http://pushgateway:9091") {
		t.Errorf("adopt executor manifest must not carry the bundled endpoints:\n%s", got)
	}
	if !strings.Contains(got, "- --loki\n            - \"http://operator-loki.example:3100\"\n") {
		t.Errorf("adopt executor manifest missing --loki pointed at the operator's Loki:\n%s", got)
	}
	if !strings.Contains(got, "- --pushgateway\n            - \"http://operator-pushgateway.example:9091\"\n") {
		t.Errorf("adopt executor manifest missing --pushgateway pointed at the operator's Pushgateway:\n%s", got)
	}
}

// TestRenderExecutor_adoptNoPushgatewayIsExplicitEmpty pins the "least invasive correct
// behaviour" this ticket chose for an undeclared observability.pushgateway.url: an EXPLICIT
// `--pushgateway ""`, which obsquery.PushMetrics already treats as "metrics push off" (see
// push_test.go's TestPushMetrics_EmptyURLIsANoOp) — never an OMITTED flag, which would fall
// through to the CLI's own bundled-looking default and push into a Pushgateway adopt never
// deploys, failing on every single run.
func TestRenderExecutor_adoptNoPushgatewayIsExplicitEmpty(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "adopt"
	in.ObsLokiURL = "http://operator-loki.example:3100"
	in.ObsPushgatewayURL = ""

	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(adopt, no pushgateway): %v", err)
	}
	if !strings.Contains(got, "- --pushgateway\n            - \"\"\n") {
		t.Errorf("adopt executor manifest with no configured pushgateway must carry an EXPLICIT --pushgateway \"\", got:\n%s", got)
	}
	if strings.Contains(got, "http://pushgateway:9091") || strings.Contains(got, "http://localhost:9091") {
		t.Errorf("adopt executor manifest must never fall back to a bundled/CLI-default pushgateway URL:\n%s", got)
	}
}

// obsKindsRenderedByT3 are every kind T3.2 says adopt mode must render ZERO of.
var obsKindsRenderedByT3 = []string{"PodMonitor"}

// TestRenderObs_adoptRendersNothing pins the T3.2 promise directly: no Loki, no pushgateway, no
// promtail, no obs PodMonitor, no obs ingress NetworkPolicy, no RBAC for any of them — RenderObs
// returns EMPTY.
func TestRenderObs_adoptRendersNothing(t *testing.T) {
	for _, tier := range []string{"k3d", "aks"} {
		in := sampleInstance()
		in.Tier = tier
		in.ObsMode = "adopt"
		in.ObsLokiURL = "http://operator-loki.example:3100"

		got, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatalf("RenderObs(adopt, tier=%s): %v", tier, err)
		}
		if got != "" {
			t.Fatalf("RenderObs(adopt, tier=%s) must be EMPTY, got %d bytes:\n%s", tier, len(got), got)
		}
	}
}

// TestRenderObs_adoptCombinedWithExecutorRendersNoObsKinds is the belt-and-braces version: render
// BOTH manifests for an adopt instance exactly as cmd/argus's cmdRenderK8s does (concatenated),
// and assert none of the obs-plane kinds appear anywhere — not even smuggled into the executor
// manifest.
func TestRenderObs_adoptCombinedWithExecutorRendersNoObsKinds(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "adopt"
	in.ObsLokiURL = "http://operator-loki.example:3100"

	exec, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(adopt): %v", err)
	}
	obs, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(adopt): %v", err)
	}
	all := docs(t, exec+"\n---\n"+obs)
	kinds := kindsOf(all)
	for _, k := range []string{"PodMonitor"} {
		if kinds[k] != 0 {
			t.Errorf("adopt rendered a %s — adopt must deploy no observability of its own", k)
		}
	}
	for _, d := range all {
		name := nameOf(d)
		if name == "loki" || name == "pushgateway" || name == "promtail" {
			t.Errorf("adopt rendered object named %q (kind %v) — adopt must deploy no observability of its own", name, d["kind"])
		}
	}
	_ = obsKindsRenderedByT3
}

// ── validate(): refuse before rendering ANYTHING ────────────────────────────────────────────────

func TestInstanceValidate_adoptRequiresLokiURL(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "adopt"
	in.ObsLokiURL = "" // not declared in the SUT's argus-config.yaml

	if _, err := RenderExecutor(in); err == nil {
		t.Fatal("RenderExecutor(adopt, no loki url) must refuse, got nil error")
	} else if !strings.Contains(err.Error(), "observability.loki.url") {
		t.Errorf("error must name observability.loki.url, got: %v", err)
	}
	if _, err := RenderObs(in, cfgJSON(t)); err == nil {
		t.Fatal("RenderObs(adopt, no loki url) must refuse, got nil error")
	} else if !strings.Contains(err.Error(), "observability.loki.url") {
		t.Errorf("error must name observability.loki.url, got: %v", err)
	}
}

// TestInstanceValidate_exportWithNoBackendRefused (T3.1): export IS built now, but it still
// refuses an instance that declares neither accepted shape — naming BOTH shapes in one message,
// as the design requires. This replaces the pre-T3.1 "not built yet" test: export's refusal
// message changed because export itself changed, from "recognised and refused" to "built, and
// refused only when misconfigured".
func TestInstanceValidate_exportWithNoBackendRefused(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export" // no ObsLokiURL/ObsLokiPushURL/ObsCredentialVarName, no ObsUseBetterStack

	_, err := RenderExecutor(in)
	if err == nil {
		t.Fatal("RenderExecutor(export, no backend declared) must refuse, got nil error")
	}
	for _, want := range []string{"observability.loki.url", "push_url", "credential", "observability.betterstack", "bundled", "adopt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("export-with-no-backend refusal must mention %q, got: %v", want, err)
		}
	}
	if _, err := RenderObs(in, cfgJSON(t)); err == nil {
		t.Fatal("RenderObs(export, no backend declared) must refuse too, got nil error")
	}
}

// TestInstanceValidate_exportHostedLokiIncomplete_Refused pins each of the three hosted-Loki
// fields as independently REQUIRED: dropping any one of url/push_url/credential must still refuse
// (no partial-credit combination silently renders).
func TestInstanceValidate_exportHostedLokiIncomplete_Refused(t *testing.T) {
	full := func() Instance {
		in := sampleInstance()
		in.ObsMode = "export"
		in.ObsLokiURL = "https://logs.example.grafana.net"
		in.ObsLokiPushURL = "https://logs.example.grafana.net/loki/api/v1/push"
		in.ObsCredentialVarName = "LOKI_CREDENTIAL"
		return in
	}
	cases := []struct {
		name   string
		break_ func(*Instance)
	}{
		{"no url", func(in *Instance) { in.ObsLokiURL = "" }},
		{"no push_url", func(in *Instance) { in.ObsLokiPushURL = "" }},
		{"no credential var", func(in *Instance) { in.ObsCredentialVarName = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := full()
			c.break_(&in)
			if _, err := RenderExecutor(in); err == nil {
				t.Fatalf("incomplete hosted-Loki triple (%s) must refuse, got nil error", c.name)
			}
		})
	}
}

// TestInstanceValidate_exportHostedLokiComplete_Accepted is the positive mirror: all three fields
// set (and no BetterStack) must NOT refuse.
func TestInstanceValidate_exportHostedLokiComplete_Accepted(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "https://logs.example.grafana.net"
	in.ObsLokiPushURL = "https://logs.example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"

	if _, err := RenderExecutor(in); err != nil {
		t.Fatalf("RenderExecutor(export, complete hosted-Loki triple) must not refuse: %v", err)
	}
	if _, err := RenderObs(in, cfgJSON(t)); err != nil {
		t.Fatalf("RenderObs(export, complete hosted-Loki triple) must not refuse: %v", err)
	}
}

// TestInstanceValidate_exportBetterStackAccepted: ObsUseBetterStack alone (no Loki fields at all)
// is a complete, accepted export target.
func TestInstanceValidate_exportBetterStackAccepted(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsUseBetterStack = true

	if _, err := RenderExecutor(in); err != nil {
		t.Fatalf("RenderExecutor(export, betterstack) must not refuse: %v", err)
	}
	if _, err := RenderObs(in, cfgJSON(t)); err != nil {
		t.Fatalf("RenderObs(export, betterstack) must not refuse: %v", err)
	}
}

func TestInstanceValidate_unknownObsModeRefused(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "sideways"

	_, err := RenderExecutor(in)
	if err == nil {
		t.Fatal("RenderExecutor(unknown obs mode) must refuse, got nil error")
	}
	for _, want := range []string{"bundled", "adopt", "export"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-value refusal must list %q as a valid option, got: %v", want, err)
		}
	}
}

// TestInstanceValidate_writesNothingBeforeRefusing: same "write nothing before refusing" rule
// every other pre-render check in this package already follows (see e.g. render_tier_test.go at
// the cmd/argus layer) — RenderExecutor must return an error with an EMPTY string, never a
// partially-built manifest, for every refused obs mode.
func TestInstanceValidate_writesNothingBeforeRefusing(t *testing.T) {
	for _, mode := range []string{"export", "sideways", "adopt"} {
		in := sampleInstance()
		in.ObsMode = mode // adopt here has no ObsLokiURL set, so it also refuses
		got, err := RenderExecutor(in)
		if err == nil {
			t.Fatalf("mode %q: expected a refusal", mode)
		}
		if got != "" {
			t.Errorf("mode %q: RenderExecutor returned %d bytes alongside its error; want empty", mode, len(got))
		}
	}
}
