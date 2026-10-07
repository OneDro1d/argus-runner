package k8srender

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsconfig"
)

// sampleInstance is a SECOND instance, deliberately sharing NO substring with the
// hand-filled `orderservice-k3d` template U1 replaces — so any leaked hardcoding shows up.
func sampleInstance() Instance {
	return Instance{
		ID:           "memstore-k3d",
		SUTNamespace: "memstore-ns",
		Image:        "ghcr.io/onedro1d/argus-runner:m3-dev",
		CPURL:        "http://argus-control.argus-system.svc.cluster.local:8080",
		WorkspaceID:  "ws_abc123",
		SUTName:      "memstore",
		Tier:         "k3d",
		Cluster:      "k3d",
		Version:      "0.1.0",
		RunnerToken:  "runner-tok",
		AuthorToken:  "author-tok",
		ArgusConfig:  "project:\n  name: memstore\n",
		Replicas:     1,
		// CollectSUTLogs: true reproduces the pre-flag default every existing test in this package
		// was written against (promtail always rendered in bundled/export-hosted-Loki/shared mode).
		// Tests for the OFF behaviour build their own Instance (or copy this one and flip it back).
		CollectSUTLogs: true,
	}
}

// docs splits a rendered multi-doc manifest and unmarshals each doc.
func docs(t *testing.T, manifest string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err != nil {
			break
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}

func kindsOf(ds []map[string]any) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		if k, _ := d["kind"].(string); k != "" {
			out[k]++
		}
	}
	return out
}

// nsOf returns metadata.namespace (empty for cluster-scoped).
func nsOf(d map[string]any) string {
	md, _ := d["metadata"].(map[string]any)
	s, _ := md["namespace"].(string)
	return s
}

func nameOf(d map[string]any) string {
	md, _ := d["metadata"].(map[string]any)
	s, _ := md["name"].(string)
	return s
}

// ── The core U1 promise: NOTHING of the old hand-filled instance survives ─────────────

func TestRenderExecutor_noHardcodedInstance(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for _, leak := range []string{"orderservice-k3d", "order-k3d", "order-api", "order-service", "argus-suite:stageII0", "ws_live", "runner-dev-token", "author-dev-token"} {
		if strings.Contains(got, leak) {
			t.Errorf("rendered executor manifest leaks hardcoded %q from the old template", leak)
		}
	}
}

func TestRenderObs_noHardcodedInstance(t *testing.T) {
	got, err := RenderObs(sampleInstance(), cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs: %v", err)
	}
	for _, leak := range []string{"orderservice-k3d", "order-k3d"} {
		if strings.Contains(got, leak) {
			t.Errorf("rendered obs manifest leaks hardcoded %q from the old template", leak)
		}
	}
}

// ── The exit criterion: two instances stand up side by side, zero manual edits ────────

func TestRender_twoInstancesNoCollision(t *testing.T) {
	a := sampleInstance()
	b := sampleInstance()
	b.ID = "social-k3d"
	b.SUTNamespace = "social-ns"
	b.SUTName = "social"

	var aDocs, bDocs []map[string]any
	for _, r := range []struct {
		in  Instance
		dst *[]map[string]any
	}{{a, &aDocs}, {b, &bDocs}} {
		ex, err := RenderExecutor(r.in)
		if err != nil {
			t.Fatalf("RenderExecutor(%s): %v", r.in.ID, err)
		}
		ob, err := RenderObs(r.in, cfgJSON(t))
		if err != nil {
			t.Fatalf("RenderObs(%s): %v", r.in.ID, err)
		}
		*r.dst = docs(t, ex+"\n---\n"+ob)
	}

	if a.Namespace() == b.Namespace() {
		t.Fatalf("both instances rendered into namespace %q", a.Namespace())
	}

	// Every CLUSTER-SCOPED object (no namespace) must have a distinct name across instances —
	// this is what actually breaks when two instances share a cluster.
	clusterScoped := func(ds []map[string]any) map[string]string {
		out := map[string]string{}
		for _, d := range ds {
			if nsOf(d) == "" {
				k, _ := d["kind"].(string)
				if k == "Namespace" {
					continue // the Namespace object itself is named per-instance by construction
				}
				out[k+"/"+nameOf(d)] = k
			}
		}
		return out
	}
	aCS, bCS := clusterScoped(aDocs), clusterScoped(bDocs)
	if len(aCS) == 0 {
		t.Fatal("no cluster-scoped objects rendered — expected at least the promtail ClusterRole/Binding")
	}
	for key := range aCS {
		if _, dup := bCS[key]; dup {
			t.Errorf("cluster-scoped object %q collides between instances %s and %s", key, a.ID, b.ID)
		}
	}

	// Every namespaced object must sit in ITS OWN instance namespace.
	for _, d := range aDocs {
		if ns := nsOf(d); ns != "" && ns != a.Namespace() {
			t.Errorf("instance %s rendered %s/%s into foreign namespace %q", a.ID, d["kind"], nameOf(d), ns)
		}
	}
}

// ── Shape: the plan names the exact objects U1 must emit ─────────────────────────────

func TestRenderExecutor_emitsRequiredObjects(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	kinds := kindsOf(docs(t, got))
	// plan §U1: "executor Deployment, ClusterIP Service, Secret, results PVC, NetworkPolicy, ResourceQuota"
	for _, want := range []string{"Namespace", "PersistentVolumeClaim", "Secret", "ConfigMap", "Deployment", "Service", "NetworkPolicy", "ResourceQuota"} {
		if kinds[want] == 0 {
			t.Errorf("rendered executor manifest is missing a %s (have: %v)", want, kinds)
		}
	}
}

func TestRenderExecutor_serviceIsClusterIP(t *testing.T) {
	got, _ := RenderExecutor(sampleInstance())
	for _, d := range docs(t, got) {
		if d["kind"] != "Service" || nameOf(d) != "executor" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		// S6: the executor is NEVER exposed via NodePort/LoadBalancer/Ingress.
		if got := spec["type"]; got != "ClusterIP" {
			t.Fatalf("executor Service type = %v, want ClusterIP (S6 forbids exposing the executor)", got)
		}
		return
	}
	t.Fatal("no executor Service rendered")
}

func TestRenderExecutor_neverExposesExecutor(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ds := docs(t, got)
	if len(ds) == 0 {
		t.Fatal("nothing rendered — this test would otherwise pass vacuously")
	}
	for _, d := range ds {
		if k, _ := d["kind"].(string); k == "Ingress" {
			t.Error("S6 violation: rendered an Ingress for the executor")
		}
		if d["kind"] == "Service" {
			spec, _ := d["spec"].(map[string]any)
			if ty, _ := spec["type"].(string); ty == "NodePort" || ty == "LoadBalancer" {
				t.Errorf("S6 violation: Service %s has type %s", nameOf(d), ty)
			}
			ports, _ := spec["ports"].([]any)
			for _, p := range ports {
				pm, _ := p.(map[string]any)
				if _, has := pm["nodePort"]; has {
					t.Errorf("S6 violation: Service %s pins a nodePort", nameOf(d))
				}
			}
		}
	}
}

// ── Secret hygiene: tokens live ONLY in the Secret, never in args or plain env ────────

func TestRenderExecutor_tokensOnlyInSecret(t *testing.T) {
	in := sampleInstance()
	in.RunnerToken = "SECRET-RUNNER-VALUE"
	in.AuthorToken = "SECRET-AUTHOR-VALUE"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for _, d := range docs(t, got) {
		if d["kind"] == "Secret" {
			continue // the one legitimate home
		}
		b, _ := yaml.Marshal(d)
		for _, tok := range []string{in.RunnerToken, in.AuthorToken} {
			if strings.Contains(string(b), tok) {
				t.Errorf("token value leaked into a %s (must live only in the Secret)", d["kind"])
			}
		}
	}
}

// ── The obs plane must honour the SUT's declared log-field translation table ──────────

func cfgFrom(t *testing.T, y string) *config.Config {
	t.Helper()
	var c config.Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal argus-config: %v", err)
	}
	return &c
}

func cfgJSON(t *testing.T) *config.Config {
	t.Helper()
	return cfgFrom(t, `
project: {name: memstore}
observability:
  loki:
    url: http://loki:3100
    correlation_field: correlation_id
    level_field: level
    saga_event_field: event_type
    saga_event_value: saga
`)
}

func TestRenderObs_honoursLogFormat(t *testing.T) {
	jsonCfg := cfgJSON(t)
	logfmtCfg := cfgFrom(t, `
project: {name: memstore}
observability:
  loki:
    url: http://loki:3100
    log_format: logfmt
    correlation_field: request_id
    level_field: severity
    saga_event_field: evt
    saga_event_value: saga
`)

	jsonOut, err := RenderObs(sampleInstance(), jsonCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut, "- json:") {
		t.Error("a json-logging SUT must get a promtail `json:` parse stage")
	}

	lfOut, err := RenderObs(sampleInstance(), logfmtCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lfOut, "- logfmt:") {
		t.Error("a logfmt SUT must get a promtail `logfmt:` parse stage (GAP-2: a json stage silently extracts NOTHING)")
	}
	if strings.Contains(lfOut, "- json:") {
		t.Error("a logfmt SUT must NOT also get a json parse stage")
	}
	// The declared field NAMES must be threaded through, not the the operator defaults.
	for _, want := range []string{"request_id", "severity", "evt"} {
		if !strings.Contains(lfOut, want) {
			t.Errorf("promtail config does not carry the SUT's declared field %q", want)
		}
	}
}

// AC-D32 (issue #173): only an ARGUS id may become the correlation_id stream label — the SAME gate the
// compose tier renders (obsconfig.CorrelationLabelStage), because hub-dev, where a SUT's own
// per-request ids filled Loki's 5,000-stream cap in ~90 s, was an AKS instance rendered HERE. The gate
// must run before the labels stage that promotes correlation_id, or it gates nothing. What the template
// DOES is asserted in internal/obsconfig; this asserts the k8s pipeline carries it, in the right place.
func TestRenderObs_onlyArgusIDsBecomeTheCorrelationLabel(t *testing.T) {
	var want []struct {
		Template struct {
			Source   string `yaml:"source"`
			Template string `yaml:"template"`
		} `yaml:"template"`
	}
	if err := yaml.Unmarshal([]byte(obsconfig.CorrelationLabelStage), &want); err != nil || len(want) != 1 {
		t.Fatalf("obsconfig.CorrelationLabelStage is not one YAML stage: %v", err)
	}

	for _, c := range []*config.Config{cfgJSON(t), cfgFrom(t, "project: {name: memstore}\nobservability:\n  loki: {log_format: logfmt, correlation_field: request_id}\n")} {
		got, err := RenderObs(sampleInstance(), c)
		if err != nil {
			t.Fatal(err)
		}
		var promtailYAML string
		for _, d := range docs(t, got) {
			if d["kind"] == "ConfigMap" && nameOf(d) == "promtail-config" {
				data, _ := d["data"].(map[string]any)
				promtailYAML, _ = data["config.yml"].(string)
			}
		}
		if promtailYAML == "" {
			t.Fatal("no promtail-config ConfigMap with a config.yml in the rendered obs plane")
		}
		var cfg struct {
			ScrapeConfigs []struct {
				PipelineStages []struct {
					Template *struct {
						Source   string `yaml:"source"`
						Template string `yaml:"template"`
					} `yaml:"template"`
					Labels map[string]any `yaml:"labels"`
				} `yaml:"pipeline_stages"`
			} `yaml:"scrape_configs"`
		}
		if err := yaml.Unmarshal([]byte(promtailYAML), &cfg); err != nil || len(cfg.ScrapeConfigs) != 1 {
			t.Fatalf("promtail config.yml does not parse to one scrape config: %v\n%s", err, promtailYAML)
		}
		gate, promote := -1, -1
		for i, s := range cfg.ScrapeConfigs[0].PipelineStages {
			if s.Template != nil && s.Template.Source == "correlation_id" {
				if s.Template.Template != want[0].Template.Template {
					t.Errorf("k8s correlation_id gate = %q, want the compose tier's %q", s.Template.Template, want[0].Template.Template)
				}
				gate = i
			}
			if _, ok := s.Labels["correlation_id"]; ok && promote < 0 {
				promote = i
			}
		}
		switch {
		case promote < 0:
			t.Error("no labels stage promotes correlation_id — the dashboard's log panels select on that label")
		case gate < 0:
			t.Error("no template stage gates correlation_id: every distinct value of the SUT's correlation field " +
				"would be a Loki stream (AC-D32)")
		case gate > promote:
			t.Errorf("the correlation_id gate (stage %d) runs AFTER the labels stage (stage %d) — it gates nothing", gate, promote)
		}
	}
}

func TestRenderObs_normalizesEverySagaValue(t *testing.T) {
	multi := cfgFrom(t, `
project: {name: social}
observability:
  loki:
    url: http://loki:3100
    saga_event_field: event
    saga_event_values: [tool_dispatch, ayrshare_dispatch, ghost_dispatch]
`)
	got, err := RenderObs(sampleInstance(), multi)
	if err != nil {
		t.Fatal(err)
	}
	// PROB-2: normalizing only the FIRST value leaves the saga panel partial.
	for _, v := range []string{"tool_dispatch", "ayrshare_dispatch", "ghost_dispatch"} {
		if !strings.Contains(got, v) {
			t.Errorf("promtail saga normalization drops declared marker value %q", v)
		}
	}
}

func TestRenderObs_logGlobScopedToSUTNamespace(t *testing.T) {
	in := sampleInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "/var/log/pods/" + in.SUTNamespace + "_*/*/*.log"
	if !strings.Contains(got, want) {
		t.Errorf("promtail __path__ glob is not scoped to the SUT namespace; want %q", want)
	}
}

// ── Validation: a bad id must be refused, not silently rendered into a broken manifest ─

func TestRenderExecutor_rejectsInvalidID(t *testing.T) {
	for _, bad := range []string{"", "Order_Service", "UPPER", "-leading", "trailing-", strings.Repeat("x", 60)} {
		in := sampleInstance()
		in.ID = bad
		if _, err := RenderExecutor(in); err == nil {
			t.Errorf("RenderExecutor accepted invalid instance id %q", bad)
		}
	}
}

func TestRenderExecutor_rejectsMissingRequired(t *testing.T) {
	base := sampleInstance()
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
	}{
		{"no image", func(i *Instance) { i.Image = "" }},
		{"no SUT namespace", func(i *Instance) { i.SUTNamespace = "" }},
		{"no CP URL", func(i *Instance) { i.CPURL = "" }},
		{"no argus config", func(i *Instance) { i.ArgusConfig = "" }},
	} {
		in := base
		tc.mutate(&in)
		if _, err := RenderExecutor(in); err == nil {
			t.Errorf("RenderExecutor accepted an instance with %s", tc.name)
		}
	}
}

// ── Everything rendered must be parseable and well-formed ────────────────────────────

func TestRender_allDocsWellFormed(t *testing.T) {
	ex, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	ob, err := RenderObs(sampleInstance(), cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	all := docs(t, ex+"\n---\n"+ob)
	if len(all) < 10 {
		t.Fatalf("expected a full instance manifest set, got %d docs", len(all))
	}
	for i, d := range all {
		if _, ok := d["apiVersion"]; !ok {
			t.Errorf("doc %d has no apiVersion", i)
		}
		if _, ok := d["kind"]; !ok {
			t.Errorf("doc %d has no kind", i)
		}
		if nameOf(d) == "" {
			t.Errorf("doc %d (%v) has no metadata.name", i, d["kind"])
		}
	}
}

// The argus-config the operator actually wrote must be embedded VERBATIM — the old template
// synthesized OrderService's config inline, which cannot serve a BYO SUT.
func TestRenderExecutor_embedsOperatorArgusConfigVerbatim(t *testing.T) {
	in := sampleInstance()
	in.ArgusConfig = "project:\n  name: my-weird-sut\ntargets:\n  http:\n    base_url: http://x.y.z:1234\n"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, got) {
		if d["kind"] != "ConfigMap" {
			continue
		}
		data, _ := d["data"].(map[string]any)
		if s, _ := data["argus-config.yaml"].(string); s != in.ArgusConfig {
			t.Errorf("argus-config.yaml was not embedded verbatim:\n got: %q\nwant: %q", s, in.ArgusConfig)
		}
		return
	}
	t.Fatal("no argus-config ConfigMap rendered")
}

func TestRenderExecutor_replicasHonoured(t *testing.T) {
	in := sampleInstance()
	in.Replicas = 3
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	var seen bool
	for _, d := range docs(t, got) {
		if d["kind"] != "Deployment" {
			continue
		}
		seen = true
		spec, _ := d["spec"].(map[string]any)
		if r, _ := spec["replicas"].(int); r != 3 {
			t.Errorf("Deployment replicas = %v, want 3 (min-3 axiom must be expressible)", spec["replicas"])
		}
	}
	if !seen {
		t.Fatal("no Deployment rendered — this test would otherwise pass vacuously")
	}
}

// A Deployment's spec.selector is IMMUTABLE. Folding the instance id into it welds the
// Deployment to that id forever and makes an in-place update impossible — a real server-side
// dry-run rejected exactly that with "spec.selector: field is immutable". Isolation comes from
// the per-instance NAMESPACE, so the selector must stay minimal.
func TestRenderExecutor_deploymentSelectorIsMinimalAndStable(t *testing.T) {
	in := sampleInstance()
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	var checked bool
	for _, d := range docs(t, got) {
		if d["kind"] != "Deployment" {
			continue
		}
		checked = true
		spec, _ := d["spec"].(map[string]any)
		sel, _ := spec["selector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if len(ml) != 1 {
			t.Errorf("Deployment selector has %d labels, want exactly 1 (it is immutable — keep it minimal): %v", len(ml), ml)
		}
		if _, hasInstance := ml["argus.onedroid.ai/instance"]; hasInstance {
			t.Error("Deployment selector includes the instance id; selectors are immutable, so this can never be changed later")
		}
		// The pod template must still CARRY the instance label for cross-namespace queries.
		tpl, _ := spec["template"].(map[string]any)
		md, _ := tpl["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels["argus.onedroid.ai/instance"] != in.ID {
			t.Errorf("pod template is missing argus.onedroid.ai/instance=%s (needed to query pods across instances)", in.ID)
		}
	}
	if !checked {
		t.Fatal("no Deployment rendered — this test would otherwise pass vacuously")
	}
}

// TestLeakCheckHasTeeth is the NEGATIVE CONTROL for TestRenderExecutor_noHardcodedInstance:
// it proves the leak check actually trips on the hand-filled template U1 replaces. Without
// this, a leak check that silently matched nothing would look identical to a clean render.
func TestLeakCheckHasTeeth(t *testing.T) {
	legacy := "  name: argus-inst-orderservice-k3d\n  image: argus-suite:stageII0\n"
	var tripped int
	for _, leak := range []string{"orderservice-k3d", "argus-suite:stageII0"} {
		if strings.Contains(legacy, leak) {
			tripped++
		}
	}
	if tripped != 2 {
		t.Fatalf("the leak check does not detect known-hardcoded values (tripped %d/2) — the clean result on the rendered manifest would be meaningless", tripped)
	}
}

// ── Alias derivation: ONE argus-config must serve both tiers unchanged ────────────────

func TestDeriveAliases_bareCompseHostsGetInClusterFQDNs(t *testing.T) {
	c := cfgFrom(t, `
project: {name: order-service}
targets:
  http: {base_url: "http://order-api:8080"}
  database: {jdbc_url: "jdbc:postgresql://postgres:5432/orders"}
  message_broker:
    url: "amqp://orders:orders@rabbitmq:5672/"
    management_url: "http://rabbitmq:15672"
  external: {webhook_base_url: "http://webhook-mock:8081"}
`)
	got := DeriveAliases(c, "order-k3d")
	want := map[string]string{
		"order-api":    "order-api.order-k3d.svc.cluster.local",
		"postgres":     "postgres.order-k3d.svc.cluster.local",
		"rabbitmq":     "rabbitmq.order-k3d.svc.cluster.local",
		"webhook-mock": "webhook-mock.order-k3d.svc.cluster.local",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("alias %q = %q, want %q (a compose-era bare host must resolve in-cluster)", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("derived %d aliases, want %d: %v", len(got), len(want), got)
	}
}

// Already-resolvable hosts must be LEFT ALONE — aliasing an FQDN or localhost would either
// be a no-op or actively break it.
func TestDeriveAliases_leavesResolvableHostsAlone(t *testing.T) {
	c := cfgFrom(t, `
project: {name: x}
targets:
  http: {base_url: "http://order-api.other-ns.svc.cluster.local:8080"}
  database: {jdbc_url: "jdbc:postgresql://10.0.0.5:5432/orders"}
  mcp: {base_url: "http://localhost:9000"}
`)
	if got := DeriveAliases(c, "order-k3d"); len(got) != 0 {
		t.Errorf("derived aliases for already-resolvable hosts: %v", got)
	}
}

func TestHostOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://order-api:8080", "order-api"},
		{"jdbc:postgresql://postgres:5432/orders", "postgres"},
		{"amqp://orders:orders@rabbitmq:5672/", "rabbitmq"},
		{"http://rabbitmq:15672", "rabbitmq"},
		{"http://host.example.com/x?y=1", "host.example.com"},
		{"not-a-url", ""},
		{"", ""},
	} {
		if got := hostOf(tc.in); got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The derived aliases must actually reach the rendered manifest as ExternalName Services.
func TestRenderExecutor_derivedAliasesBecomeExternalNameServices(t *testing.T) {
	in := sampleInstance()
	in.ExternalAliases = map[string]string{"order-api": "order-api.order-k3d.svc.cluster.local"}
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, got) {
		if d["kind"] != "Service" || nameOf(d) != "order-api" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		if spec["type"] != "ExternalName" {
			t.Fatalf("alias Service type = %v, want ExternalName", spec["type"])
		}
		if spec["externalName"] != "order-api.order-k3d.svc.cluster.local" {
			t.Fatalf("alias externalName = %v", spec["externalName"])
		}
		return
	}
	t.Fatal("no ExternalName Service rendered for the declared alias")
}

// The SUT's ${VAR} values must land in the SECRET, never in the ConfigMap. The config is
// embedded verbatim (still templated), so the pod has to be able to expand it — but a ConfigMap
// is readable by anything with namespace read access, and the entire point of the ${VAR}
// convention is that values never sit next to the config.
func TestRenderExecutor_sutSecretsGoInTheSecretNotTheConfigMap(t *testing.T) {
	in := sampleInstance()
	in.ArgusConfig = "targets:\n  mcp:\n    auth:\n      bearer_token: ${TEST_USER_STATIC_TOKEN}\n"
	in.SecretEnv = map[string]string{"TEST_USER_STATIC_TOKEN": "SUPER-SECRET-VALUE"}
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	var sawSecret bool
	for _, d := range docs(t, got) {
		switch d["kind"] {
		case "Secret":
			data, _ := d["stringData"].(map[string]any)
			if data["TEST_USER_STATIC_TOKEN"] != "SUPER-SECRET-VALUE" {
				t.Errorf("the SUT ${VAR} did not reach the Secret: %v", data["TEST_USER_STATIC_TOKEN"])
			}
			sawSecret = true
		case "ConfigMap":
			data, _ := d["data"].(map[string]any)
			cfg, _ := data["argus-config.yaml"].(string)
			if strings.Contains(cfg, "SUPER-SECRET-VALUE") {
				t.Error("the SECRET VALUE was inlined into the ConfigMap — it must stay templated as ${VAR}")
			}
			if !strings.Contains(cfg, "${TEST_USER_STATIC_TOKEN}") {
				t.Error("the ConfigMap lost the ${VAR} placeholder; the pod could not expand it")
			}
		}
	}
	if !sawSecret {
		t.Fatal("no Secret rendered")
	}
}

// EVERY pod we render must declare cpu+memory LIMITS, because the instance ResourceQuota
// declares limits.cpu/limits.memory — and a quota that sets a limit makes that limit MANDATORY
// for every pod in the namespace. A pod without one is rejected at creation:
//
//	pods "loki-…" is forbidden: failed quota: instance-quota: must specify limits.cpu for: loki
//
// Found LIVE, not by dry-run: a Deployment dry-run never creates a Pod, so the quota is not
// exercised. The executor started (it had limits) while every obs pod was refused, which reads
// as "the obs plane is broken" rather than "the manifest fights its own quota".
func TestRender_everyPodSatisfiesItsOwnQuota(t *testing.T) {
	in := sampleInstance()
	ex, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}

	var quotaSetsLimits bool
	type podspec struct {
		kind, name string
		containers []any
	}
	var pods []podspec

	for _, d := range docs(t, ex+"\n---\n"+ob) {
		kind, _ := d["kind"].(string)
		if kind == "ResourceQuota" {
			spec, _ := d["spec"].(map[string]any)
			hard, _ := spec["hard"].(map[string]any)
			_, hasCPU := hard["limits.cpu"]
			_, hasMem := hard["limits.memory"]
			quotaSetsLimits = hasCPU || hasMem
			continue
		}
		if kind != "Deployment" && kind != "DaemonSet" && kind != "StatefulSet" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		ps, _ := tpl["spec"].(map[string]any)
		cs, _ := ps["containers"].([]any)
		pods = append(pods, podspec{kind, nameOf(d), cs})
	}

	if !quotaSetsLimits {
		t.Skip("the quota no longer declares limits.* — this constraint would not apply")
	}
	if len(pods) < 3 {
		t.Fatalf("expected the executor + the obs workloads, found %d", len(pods))
	}
	for _, p := range pods {
		for _, c := range p.containers {
			cm, _ := c.(map[string]any)
			cname, _ := cm["name"].(string)
			res, _ := cm["resources"].(map[string]any)
			lim, _ := res["limits"].(map[string]any)
			if lim["cpu"] == nil || lim["memory"] == nil {
				t.Errorf("%s/%s container %q has no cpu+memory limits — the ResourceQuota will REJECT this pod at creation",
					p.kind, p.name, cname)
			}
		}
	}
}

// ── U5: the obs plane must be reachable by an OFF-cluster query layer, on local tiers only ──

// svcType returns a Service's spec.type from a rendered manifest.
func svcType(t *testing.T, manifest, name string) string {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] == "Service" && nameOf(d) == name {
			spec, _ := d["spec"].(map[string]any)
			s, _ := spec["type"].(string)
			return s
		}
	}
	t.Fatalf("no Service %q in the rendered manifest", name)
	return ""
}

// On a LOCAL cluster the shared Grafana/Prometheus run OUTSIDE the cluster (a compose project on
// the same host), and a ClusterIP is not routable from there — measured, not assumed: a curl from
// the Grafana container to the Loki ClusterIP returned 000. So the obs plane must be published, or
// every dashboard panel for a k3d instance renders empty while LOOKING configured.
func TestRenderObs_localTierPublishesTheObsPlane(t *testing.T) {
	for _, tier := range []string{"k3d", "kind", "minikube"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		for _, svc := range []string{"loki", "pushgateway"} {
			if ty := svcType(t, got, svc); ty != "NodePort" {
				t.Errorf("tier %s: Service %s type = %q, want NodePort (an off-cluster Grafana cannot reach a ClusterIP)", tier, svc, ty)
			}
		}
	}
}

// On a MANAGED cluster the obs plane must stay ClusterIP. Publishing an unauthenticated log store
// on every node there is a real exposure, not a convenience — Stage III wires the query layer
// differently (in-cluster Grafana, or an authenticated ingress).
func TestRenderObs_managedTierKeepsTheObsPlanePrivate(t *testing.T) {
	for _, tier := range []string{"aks", "eks", "gke", "managed"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		for _, svc := range []string{"loki", "pushgateway"} {
			if ty := svcType(t, got, svc); ty != "ClusterIP" {
				t.Errorf("tier %s: Service %s type = %q, want ClusterIP — publishing an unauthenticated log store off-cluster on a managed tier is an exposure", tier, svc, ty)
			}
		}
	}
}

// S6 is unchanged by any of this: the EXECUTOR stays ClusterIP-only on EVERY tier, including the
// local ones where the obs plane is published. The two must not drift together.
func TestRenderExecutor_stillClusterIPOnLocalTiers(t *testing.T) {
	for _, tier := range []string{"k3d", "kind", "minikube", "aks"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		if ty := svcType(t, got, "executor"); ty != "ClusterIP" {
			t.Errorf("S6 VIOLATION on tier %s: executor Service type = %q, want ClusterIP on every tier", tier, ty)
		}
	}
}

// ── The min-3 axiom must buy node-loss survival, not just a replica count ─────────────

// A node-local RWO class pins every replica to one node: its PV gets a hard node affinity and
// WaitForFirstConsumer binding obliges the scheduler to co-locate. Measured on a 2-node cluster
// before this fix — 3/3 replicas on one node, which satisfies the COUNT and defeats the INTENT.
func TestRenderExecutor_localTierUsesRWXSoReplicasCanSpread(t *testing.T) {
	for _, tier := range []string{"k3d", "kind", "minikube"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		for _, d := range docs(t, got) {
			if d["kind"] != "PersistentVolumeClaim" {
				continue
			}
			spec, _ := d["spec"].(map[string]any)
			modes, _ := spec["accessModes"].([]any)
			if len(modes) == 0 || modes[0] != "ReadWriteMany" {
				t.Errorf("tier %s: PVC accessModes = %v, want ReadWriteMany (RWO pins all replicas to one node)", tier, modes)
			}
			if sc, _ := spec["storageClassName"].(string); sc != "argus-rwx" {
				t.Errorf("tier %s: storageClassName = %q, want argus-rwx", tier, sc)
			}
		}
	}
}

// An explicit caller choice must still win — a cluster with a differently named RWX class, or a
// deliberate single-node RWO setup, must remain expressible.
func TestRenderExecutor_explicitStorageOverridesTheDefault(t *testing.T) {
	in := sampleInstance()
	in.StorageClass = "azurefile-csi"
	in.AccessMode = "ReadWriteMany"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "azurefile-csi") {
		t.Error("an explicitly set StorageClass was overridden by the tier default")
	}
}

// ── III.3a (Stage III): the managed AKS tier ──────────────────────────────────────────

// execPodSpec returns the executor Deployment's pod template spec from a rendered manifest.
func execPodSpec(t *testing.T, manifest string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] != "Deployment" || nameOf(d) != "executor" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		ps, _ := tpl["spec"].(map[string]any)
		return ps
	}
	t.Fatal("no executor Deployment rendered")
	return nil
}

// execPVC returns the exec-results PersistentVolumeClaim spec from a rendered manifest.
func execPVC(t *testing.T, manifest string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] == "PersistentVolumeClaim" && nameOf(d) == "exec-results" {
			spec, _ := d["spec"].(map[string]any)
			return spec
		}
	}
	t.Fatal("no exec-results PVC rendered")
	return nil
}

// On AKS the default node-local disk class (managed/local-path) is RWO, which pins every replica
// to the node that first bound the volume — the same failure the local-tier RWX fix prevents, but
// on the managed cluster where node loss is routine. `--tier aks` alone (no extra flags) must yield
// an azurefile RWX class + ReadWriteMany so the shared identity volume lets the replicas spread.
func TestRenderExecutor_aksDefaultsToAzurefileRWX(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	in.StorageClass = "" // exercise the tier default, not an explicit override
	in.AccessMode = ""
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	pvc := execPVC(t, got)
	if sc, _ := pvc["storageClassName"].(string); sc != "azurefile-csi" {
		t.Errorf("aks PVC storageClassName = %q, want azurefile-csi (the AKS RWX class)", sc)
	}
	modes, _ := pvc["accessModes"].([]any)
	if len(modes) == 0 || modes[0] != "ReadWriteMany" {
		t.Errorf("aks PVC accessModes = %v, want ReadWriteMany (RWO pins all replicas to one node)", modes)
	}
}

// AKS nodes cannot import the private suite image from host docker (there is no host); they must
// PULL it from GHCR, which is private (an anonymous pull 401s). So the managed-tier executor must
// carry an imagePullSecret. The cluster convention (used by the CP and the managed Social) is a
// dockerconfig secret named `ghcr-pull`; onboard.sh copies it into the instance namespace.
func TestRenderExecutor_aksSetsGhcrPullImagePullSecret(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ps := execPodSpec(t, got)
	ips, _ := ps["imagePullSecrets"].([]any)
	if len(ips) == 0 {
		t.Fatal("aks executor pod has no imagePullSecrets — it cannot pull the private GHCR suite image")
	}
	first, _ := ips[0].(map[string]any)
	if first["name"] != "ghcr-pull" {
		t.Errorf("aks imagePullSecrets[0].name = %v, want ghcr-pull (the cluster convention)", first["name"])
	}
}

// The local tiers (k3d et al.) import the image into the node containerd store and run it
// IfNotPresent, so they need no pull secret to come up and must not DEFAULT to one: a reference to a
// secret nothing created is a spurious Warning event on every executor pod — and, since VR5-U2, far
// worse than cosmetic. internal/runner/updateguard.go decides whether this cluster can obtain an
// image by whether the pod spec references a pull secret, so a phantom reference flips the update
// guard from "refuse, import first" to "allow" and lets the button strand the executor (V19-005).
//
// The DEFAULT stays managed-tier-only. An EXPLICIT one is now honoured on a local tier —
// TestRenderExecutor_localTierHonoursAnExplicitImagePullSecret, below.
func TestRenderExecutor_localTiersHaveNoImagePullSecret(t *testing.T) {
	for _, tier := range []string{"k3d", "kind", "minikube"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		ps := execPodSpec(t, got)
		if ips, has := ps["imagePullSecrets"]; has && ips != nil {
			if arr, _ := ips.([]any); len(arr) > 0 {
				t.Errorf("tier %s: executor pod has imagePullSecrets %v — a local tier imports the image, it does not pull from a private registry", tier, arr)
			}
		}
	}
}

// VR5-U2 (V19-005): a local tier that WAS given a pull secret must reference it.
//
// The Environments page's "⟳ Update this executor" button names an image onboarding never imported.
// On k3d nothing can fetch it, and the Deployment is maxSurge:0 at replicas:1 — the running pod is
// terminated first, so an unobtainable image leaves no executor and nothing able to roll back.
// Measured 2026-08-15: the button took a k3d executor down. Onboarding now resolves the operator's
// registry credential, creates the Secret, and passes its name in; this pins that the render carries
// it through. Without this the Secret exists, the pod ignores it, and the guard still refuses —
// the fix would be invisible.
//
// Additive under IfNotPresent: an imported image is still served from the node store.
func TestRenderExecutor_localTierHonoursAnExplicitImagePullSecret(t *testing.T) {
	in := sampleInstance()
	in.Tier = "k3d"
	in.ImagePullSecret = "ghcr-pull"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ps := execPodSpec(t, got)
	ips, _ := ps["imagePullSecrets"].([]any)
	if len(ips) != 1 {
		t.Fatalf("k3d with an explicit ImagePullSecret rendered %d imagePullSecrets, want 1.\n"+
			"  Onboarding created the Secret and the pod does not reference it, so nothing changed:\n"+
			"  guardImageObtainable still sees no pull secret and still refuses the update button.", len(ips))
	}
	first, _ := ips[0].(map[string]any)
	if first["name"] != "ghcr-pull" {
		t.Errorf("imagePullSecrets[0].name = %v, want ghcr-pull", first["name"])
	}
	// The pull POLICY must not move with it. IfNotPresent is what keeps the imported image
	// authoritative; flipping to Always would make every pod start depend on a registry round-trip
	// and would break a locally-built tag that was never pushed.
	cs, _ := ps["containers"].([]any)
	if len(cs) == 0 {
		t.Fatal("no containers rendered")
	}
	c, _ := cs[0].(map[string]any)
	if p, _ := c["imagePullPolicy"].(string); p != "IfNotPresent" {
		t.Errorf("imagePullPolicy = %q on k3d, want IfNotPresent — adding a pull secret must stay ADDITIVE", p)
	}
}

// STALENESS. A managed cluster PULLS the image from a registry, and our suite tags MOVE (:m3-dev is
// rebuilt in place). Under IfNotPresent a node that already cached that tag keeps serving the OLD
// binary forever, and nothing reports it — the exact failure measured on k3d, where two nodes held
// two different builds under the single tag :m3-dev and a Deployment's replicas ran two different
// binaries depending on where they landed. k3d solves it by digest-comparing on import; a managed
// tier has no import step, so it must instead always re-pull. Local tiers must STAY IfNotPresent:
// the image is imported into the node store and is not pullable from there at all.
func TestRenderExecutor_managedTierAlwaysRepullsSoAMovingTagCannotGoStale(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ps := execPodSpec(t, got)
	cs, _ := ps["containers"].([]any)
	if len(cs) == 0 {
		t.Fatal("no containers rendered")
	}
	c0, _ := cs[0].(map[string]any)
	if pp := c0["imagePullPolicy"]; pp != "Always" {
		t.Errorf("aks executor imagePullPolicy = %v, want Always (a moving tag would otherwise serve a stale cached build)", pp)
	}

	for _, tier := range []string{"k3d", "kind", "minimal-not-a-tier", "minikube"} {
		li := sampleInstance()
		li.Tier = tier
		lgot, err := RenderExecutor(li)
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		lps := execPodSpec(t, lgot)
		lcs, _ := lps["containers"].([]any)
		lc0, _ := lcs[0].(map[string]any)
		if tier == "minimal-not-a-tier" {
			continue // unknown tiers are not the subject here; only assert the LOCAL ones
		}
		if pp := lc0["imagePullPolicy"]; pp != "IfNotPresent" {
			t.Errorf("tier %s: imagePullPolicy = %v, want IfNotPresent (the image is imported into the node store, not pullable)", tier, pp)
		}
	}
}

// UC138 (III.2): the managed executor must be TOLD its environment's browser-facing Grafana base, or
// runner__get_dashboard_url emits the localhost default and every managed deep link is dead. It rides
// plain env (not the Secret): a public Grafana URL is not a credential.
func TestRenderExecutor_grafanaPublicURLReachesTheExecutorEnv(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	in.GrafanaPublicURL = "https://grafana.example.com"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ps := execPodSpec(t, got)
	cs, _ := ps["containers"].([]any)
	c0, _ := cs[0].(map[string]any)
	env, _ := c0["env"].([]any)
	var found string
	for _, e := range env {
		em, _ := e.(map[string]any)
		if em["name"] == "ARGUS_GRAFANA_PUBLIC_URL" {
			found, _ = em["value"].(string)
		}
	}
	if found != in.GrafanaPublicURL {
		t.Errorf("ARGUS_GRAFANA_PUBLIC_URL = %q, want %q — without it the managed deep link points at localhost", found, in.GrafanaPublicURL)
	}
}

// #215: the executor learns its results volume's access mode from the render, because it has no
// right to read the PVC and an autoscaler that scales a ReadWriteOnce volume to 3 strands 2 pods.
func TestRenderExecutor_resultsAccessModeReachesTheExecutorEnv(t *testing.T) {
	for _, tier := range []string{"aks", "k3d", ""} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %q: RenderExecutor: %v", tier, err)
		}
		_, wantMode := StorageDefaultsFor(tier)
		ps := execPodSpec(t, got)
		cs, _ := ps["containers"].([]any)
		c0, _ := cs[0].(map[string]any)
		env, _ := c0["env"].([]any)
		var found string
		for _, e := range env {
			em, _ := e.(map[string]any)
			if em["name"] == "ARGUS_RESULTS_ACCESS_MODE" {
				found, _ = em["value"].(string)
			}
		}
		if found != wantMode {
			t.Errorf("tier %q: ARGUS_RESULTS_ACCESS_MODE = %q, want %q (the PVC's own access mode)", tier, found, wantMode)
		}
	}
}

// Unset must render NO env entry, so compose/k3d keep the localhost default and their manifests stay
// byte-identical to before.
func TestRenderExecutor_noGrafanaPublicURLRendersNoEnv(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if strings.Contains(got, "ARGUS_GRAFANA_PUBLIC_URL") {
		t.Error("rendered ARGUS_GRAFANA_PUBLIC_URL with no value set")
	}
}

// ── S1 enrollment delivery (III.3) ───────────────────────────────────────────────────

// The CP half of S1 has existed since CP-M3-120 (POST /api/enrollments mints it;
// ARGUS_FED_REGISTER_AUTH=enrollment makes the FIRST /fed/register require it). The missing half
// is DELIVERY: the executor reads ARGUS_ENROLLMENT_TOKEN from its environment, and its env comes
// from `envFrom: secretRef: exec-tokens` — so the credential has to land in that Secret. Anywhere
// else (ConfigMap, plain env, args) would either not reach the pod or would expose it.
func TestRenderExecutor_enrollmentTokenRidesTheSecret(t *testing.T) {
	in := sampleInstance()
	in.EnrollmentToken = "enr_SECRET_VALUE_123"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	var sawInSecret bool
	for _, d := range docs(t, got) {
		switch d["kind"] {
		case "Secret":
			data, _ := d["stringData"].(map[string]any)
			if data["ARGUS_ENROLLMENT_TOKEN"] == in.EnrollmentToken {
				sawInSecret = true
			}
		default:
			b, _ := yaml.Marshal(d)
			if strings.Contains(string(b), in.EnrollmentToken) {
				t.Errorf("enrollment credential leaked into a %s — it must live ONLY in the Secret", d["kind"])
			}
		}
	}
	if !sawInSecret {
		t.Error("ARGUS_ENROLLMENT_TOKEN is not in the exec-tokens Secret; the executor reads it from env via envFrom, so it would never see it")
	}
}

// With no enrollment credential the KEY must be absent entirely, not present-and-empty. The executor
// treats a set-but-empty value as "an enrollment was supplied" and would present an empty credential
// on its first register — a 403 that reads like a CP bug instead of "no enrollment configured".
// Enrollment is also still OPT-IN (the CP only demands it when ARGUS_FED_REGISTER_AUTH=enrollment),
// so every existing compose/k3d onboarding must render exactly as before.
func TestRenderExecutor_noEnrollmentTokenOmitsTheKeyEntirely(t *testing.T) {
	in := sampleInstance() // EnrollmentToken unset
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if strings.Contains(got, "ARGUS_ENROLLMENT_TOKEN") {
		t.Error("rendered a ARGUS_ENROLLMENT_TOKEN key with no credential set — an empty value is not the same as absent")
	}
}

// LEAST PRIVILEGE ON A SHARED CLUSTER. promtail's ClusterRole grants cluster-wide
// nodes/nodes/proxy/services/endpoints/pods — but the rendered promtail config uses static_configs,
// NOT kubernetes_sd (the code says so itself), so the grant is entirely UNUSED. On a laptop k3d that
// is harmless; on a shared managed cluster `nodes/proxy` is a kubelet-API read of every tenant's
// pods, bound cluster-wide, once PER INSTANCE. Managed tiers must render no cluster-scoped RBAC.
func TestRenderObs_managedTierGrantsNoClusterWideRBAC(t *testing.T) {
	for _, tier := range []string{"aks"} {
		in := sampleInstance()
		in.ID = "social-" + tier
		in.Tier = tier
		in.Cluster = ""
		got, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		for _, d := range docs(t, got) {
			k, _ := d["kind"].(string)
			if k == "ClusterRole" || k == "ClusterRoleBinding" {
				t.Errorf("tier %s: rendered a %s (%s) — a managed cluster must not get per-instance cluster-wide RBAC it never uses", tier, k, nameOf(d))
			}
		}
		if strings.Contains(got, "nodes/proxy") {
			t.Errorf("tier %s: manifest still grants nodes/proxy (kubelet-API read of every tenant)", tier)
		}
		// promtail must STILL be rendered — dropping the RBAC must not drop log collection.
		if !strings.Contains(got, "kind: DaemonSet") || !strings.Contains(got, "name: promtail") {
			t.Errorf("tier %s: dropping the ClusterRole also dropped promtail itself", tier)
		}
	}
}

// The LOCAL tiers keep their existing RBAC untouched — k3d is Stage-II-accepted and live-proven, and
// this change must not churn it.
func TestRenderObs_localTierRBACUnchanged(t *testing.T) {
	got, err := RenderObs(sampleInstance(), cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(docs(t, got))
	if kinds["ClusterRole"] == 0 || kinds["ClusterRoleBinding"] == 0 {
		t.Errorf("k3d lost its promtail ClusterRole/Binding (have: %v)", kinds)
	}
}

// REGRESSION GUARD (III.3a): adding the aks storage defaults must not disturb any OTHER tier's
// defaults — including the EMPTY tier string, which normalize() labels k3d only LATER. An empty
// tier historically fell through to the non-local fallback (local-path / RWO); a mid-flight change
// to argus-rwx would bind the PVC to a class that exists only after onboard.sh installs it, so
// on any other cluster the PVC stays Pending and the executor never becomes Ready. This table
// pins EVERY tier's storage defaults so the next tier added cannot silently move them either.
func TestNormalize_storageDefaultsPerTierAreStable(t *testing.T) {
	for _, tc := range []struct{ tier, class, mode string }{
		{"k3d", "argus-rwx", "ReadWriteMany"},
		{"kind", "argus-rwx", "ReadWriteMany"},
		{"minikube", "argus-rwx", "ReadWriteMany"},
		{"aks", "azurefile-csi", "ReadWriteMany"},
		{"", "local-path", "ReadWriteOnce"},    // empty: the historical fallback, NOT k3d's
		{"eks", "local-path", "ReadWriteOnce"}, // unknown tier: unchanged fallback
		{"gke", "local-path", "ReadWriteOnce"},
	} {
		in := sampleInstance()
		in.Tier = tc.tier
		in.Cluster = ""
		in.StorageClass = ""
		in.AccessMode = ""
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %q: %v", tc.tier, err)
		}
		pvc := execPVC(t, got)
		if sc, _ := pvc["storageClassName"].(string); sc != tc.class {
			t.Errorf("tier %q: storageClassName = %q, want %q", tc.tier, sc, tc.class)
		}
		modes, _ := pvc["accessModes"].([]any)
		if len(modes) == 0 || modes[0] != tc.mode {
			t.Errorf("tier %q: accessModes = %v, want [%s]", tc.tier, modes, tc.mode)
		}
	}
}

// SCHEDULING. On AKS the SYSTEM node pool is UNTAINTED by default, so a pod with no node constraint
// can land on it — next to CoreDNS / metrics-server / konnectivity. Our executor runs JMeter (a JVM
// under load), which is exactly the neighbour those control-plane add-ons should not have. The same
// drift was caught for the control plane (a CP replica landed on the system pool) and fixed there
// with a node constraint. Use the AKS-GENERIC `kubernetes.azure.com/mode` label (present on every
// AKS cluster: system|user) rather than this cluster's pool NAMES, so the rule is portable.
func TestRenderExecutor_aksKeepsTheExecutorOffTheSystemNodePool(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ps := execPodSpec(t, got)
	aff, _ := ps["affinity"].(map[string]any)
	na, _ := aff["nodeAffinity"].(map[string]any)
	if na == nil {
		t.Fatal("aks executor has no nodeAffinity — nothing keeps it off the untainted system node pool")
	}
	req, _ := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	if req == nil {
		t.Fatal("aks nodeAffinity is not REQUIRED — a preference would still allow the system pool")
	}
	terms, _ := req["nodeSelectorTerms"].([]any)
	if len(terms) == 0 {
		t.Fatal("aks nodeAffinity has no nodeSelectorTerms")
	}
	blob, _ := yaml.Marshal(req)
	s := string(blob)
	for _, want := range []string{"kubernetes.azure.com/mode", "NotIn", "system"} {
		if !strings.Contains(s, want) {
			t.Errorf("aks nodeAffinity does not exclude the system pool (missing %q):\n%s", want, s)
		}
	}
	// The pod anti-affinity (replica spread) must SURVIVE alongside it — the two live under the
	// same `affinity:` key, so adding one must not silently drop the other.
	if _, ok := aff["podAntiAffinity"]; !ok {
		t.Error("adding nodeAffinity dropped the podAntiAffinity — the replicas would no longer be told to spread")
	}
}

// The node-pool rule is AKS-specific: on a local cluster (k3d/kind/minikube) there is no such label
// and no system pool, and a REQUIRED nodeAffinity nothing satisfies would leave every replica
// Pending forever — an outage dressed as hardening.
func TestRenderExecutor_localTiersGetNoNodeAffinity(t *testing.T) {
	for _, tier := range []string{"k3d", "kind", "minikube"} {
		in := sampleInstance()
		in.Tier = tier
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %s: %v", tier, err)
		}
		ps := execPodSpec(t, got)
		aff, _ := ps["affinity"].(map[string]any)
		if _, has := aff["nodeAffinity"]; has {
			t.Errorf("tier %s: executor has a nodeAffinity — on a single-node local cluster a required rule would leave replicas Pending forever", tier)
		}
		if _, ok := aff["podAntiAffinity"]; !ok {
			t.Errorf("tier %s: lost the podAntiAffinity", tier)
		}
	}
}

// An explicit ImagePullSecret must win over the tier default — a cluster whose GHCR secret is
// named differently, or a public-image setup, must remain expressible.
func TestRenderExecutor_explicitImagePullSecretOverridesTheDefault(t *testing.T) {
	in := sampleInstance()
	in.ID = "social-aks"
	in.Tier = "aks"
	in.Cluster = ""
	in.ImagePullSecret = "my-registry-cred"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ps := execPodSpec(t, got)
	ips, _ := ps["imagePullSecrets"].([]any)
	if len(ips) == 0 {
		t.Fatal("explicit ImagePullSecret produced no imagePullSecrets")
	}
	first, _ := ips[0].(map[string]any)
	if first["name"] != "my-registry-cred" {
		t.Errorf("imagePullSecrets[0].name = %v, want my-registry-cred (explicit override ignored)", first["name"])
	}
}

// The replicas must be TOLD to spread, and only preferentially — a required anti-affinity rule
// would leave replicas Pending forever on a single-node cluster, turning a resilience feature
// into an outage.
func TestRenderExecutor_antiAffinityIsPreferredNotRequired(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	var checked bool
	for _, d := range docs(t, got) {
		if d["kind"] != "Deployment" {
			continue
		}
		checked = true
		spec, _ := d["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		ps, _ := tpl["spec"].(map[string]any)
		aff, _ := ps["affinity"].(map[string]any)
		pa, _ := aff["podAntiAffinity"].(map[string]any)
		if pa == nil {
			t.Fatal("no podAntiAffinity — the replicas have nothing telling them to spread across nodes")
		}
		if _, bad := pa["requiredDuringSchedulingIgnoredDuringExecution"]; bad {
			t.Error("anti-affinity is REQUIRED — on a single-node cluster the extra replicas would stay Pending forever")
		}
		if _, ok := pa["preferredDuringSchedulingIgnoredDuringExecution"]; !ok {
			t.Error("anti-affinity is neither preferred nor required")
		}
	}
	if !checked {
		t.Fatal("no Deployment rendered")
	}
}

// CP-M3-III-34: the exec-tokens Secret must carry ARGUS_TOKEN, not just the two SCOPE tokens.
//
// ARGUS_RUNNER_TOKEN / ARGUS_AUTHOR_TOKEN (renamed ARGUS_EXECUTOR_SECRET; both keys ship this
// release — see TestRenderExecutor_execTokensSecretCarriesBothAuthorTokenKeys) declare which scope
// a presented token carries; the CLI
// reads the PRESENTED credential from --token or ARGUS_TOKEN. Without it, every argus subcommand
// run inside the executor pod dies with
//
//	{"error":"denied: auth: a token is required (supply --token or ARGUS_TOKEN; ...)"}
//
// That is exactly how the SUT-auth preflight silently never ran on ANY k8s tier (its failure was
// swallowed and misreported as "older image without preflight-auth"), and why argus get-report was
// unusable in-pod. (The compose tier meets the same gate differently: with no ARGUS_TOKEN the
// unknown-command arm sat BELOW the gate, so a pre-0.3.29 image exits 3 there — V28-001 / VR10-U2.)
// Nothing covered this, which is why it shipped — so assert the wire fact, not the wording.
func TestRenderExecutor_secretCarriesPresentedToken(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for _, key := range []string{"ARGUS_TOKEN:", "ARGUS_RUNNER_TOKEN:", "ARGUS_AUTHOR_TOKEN:"} {
		if !strings.Contains(got, key) {
			t.Errorf("exec-tokens Secret is missing %s — in-pod argus commands will fail with "+
				"\"a token is required\"", key)
		}
	}
	// ARGUS_TOKEN must be the RUNNER token: commands run in-pod are runner-scope, and handing the
	// author token to the execution plane would widen scope past the C1 holdout boundary.
	in := sampleInstance()
	if in.RunnerToken == "" || in.RunnerToken == in.AuthorToken {
		t.Skip("sampleInstance has no distinct runner token to assert against")
	}
	idx := strings.Index(got, "ARGUS_TOKEN:")
	line := got[idx:]
	if e := strings.IndexByte(line, '\n'); e >= 0 {
		line = line[:e]
	}
	if !strings.Contains(line, in.RunnerToken) {
		t.Errorf("ARGUS_TOKEN should carry the RUNNER token; got %q", line)
	}
	if strings.Contains(line, in.AuthorToken) {
		t.Errorf("ARGUS_TOKEN must NOT carry the AUTHOR token (C1 holdout): %q", line)
	}
}

// yamlScalarValue extracts the value of a "<key>: <value>" line from rendered YAML, for the tests
// below that need to compare two keys' values rather than only check for the key's presence. The
// renderer writes these Secret entries with Go's %q (json/yaml-compatible double-quoted scalar
// syntax), so a single trim of the surrounding quotes recovers the literal string every time — no
// full YAML parse needed for a scalar this simple.
func yamlScalarValue(t *testing.T, doc, key string) string {
	t.Helper()
	prefix := key + ": "
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		raw := strings.TrimPrefix(trimmed, prefix)
		return strings.Trim(raw, `"`)
	}
	t.Fatalf("key %q not found in rendered YAML", key)
	return ""
}

// TestRenderExecutor_execTokensSecretCarriesBothAuthorTokenKeys is the rename's own contract (T1/7):
// the exec-tokens Secret must carry BOTH the old key (ARGUS_AUTHOR_TOKEN) and the new key
// (ARGUS_EXECUTOR_SECRET), with the IDENTICAL value, so that an executor image built before the
// rename (which only ever reads ARGUS_AUTHOR_TOKEN) and one built after (which reads
// ARGUS_EXECUTOR_SECRET via internal/envname, falling back to ARGUS_AUTHOR_TOKEN) both start
// correctly off the SAME rendered manifest — this is what makes a rollback across the rename safe.
// Nothing else about the Secret (ARGUS_RUNNER_TOKEN, ARGUS_TOKEN) should move.
func TestRenderExecutor_execTokensSecretCarriesBothAuthorTokenKeys(t *testing.T) {
	in := sampleInstance()
	if in.AuthorToken == "" {
		t.Fatal("sampleInstance must set AuthorToken for this test to mean anything")
	}
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for _, key := range []string{"ARGUS_AUTHOR_TOKEN:", "ARGUS_EXECUTOR_SECRET:"} {
		if !strings.Contains(got, key) {
			t.Errorf("exec-tokens Secret is missing %s — an executor image on the OTHER side of the "+
				"rename from the one currently deployed would start with no author-scope credential", key)
		}
	}
	oldVal := yamlScalarValue(t, got, "ARGUS_AUTHOR_TOKEN")
	newVal := yamlScalarValue(t, got, "ARGUS_EXECUTOR_SECRET")
	if oldVal != in.AuthorToken {
		t.Errorf("ARGUS_AUTHOR_TOKEN = %q, want the configured AuthorToken %q", oldVal, in.AuthorToken)
	}
	if newVal != in.AuthorToken {
		t.Errorf("ARGUS_EXECUTOR_SECRET = %q, want the configured AuthorToken %q", newVal, in.AuthorToken)
	}
	if oldVal != newVal {
		t.Errorf("ARGUS_AUTHOR_TOKEN (%q) and ARGUS_EXECUTOR_SECRET (%q) must carry the SAME value", oldVal, newVal)
	}
}

// TestRenderObs_everyImageIsPinnedByDigest: the obs plane's third-party images are tag@digest, never a
// moving tag. lokiConfigYAML is written against one Loki's schema; ":latest" let a major bump reach an
// instance on its next pod restart, with nothing in the render changed.
func TestRenderObs_everyImageIsPinnedByDigest(t *testing.T) {
	got, err := RenderObs(sampleInstance(), cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs: %v", err)
	}
	n := 0
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		n++
		ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		if !strings.Contains(ref, "@sha256:") || strings.Contains(ref, ":latest") {
			t.Errorf("obs image %q is not pinned tag@sha256", ref)
		}
	}
	// loki + pushgateway + promtail. Zero would mean the scan matched nothing and passed vacuously.
	if n != 3 {
		t.Errorf("found %d image: lines in the obs render, want 3 (loki, pushgateway, promtail)", n)
	}
}
