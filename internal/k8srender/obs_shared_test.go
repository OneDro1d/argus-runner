package k8srender

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// T3.3 (E3 shared ingest): one Loki per environment, per-instance tenancy by Loki's own
// X-Scope-OrgID (tenant = instance id). These tests pin the render side: what an instance renders
// under --obs=shared, what the ONE shared Loki renders, and what 20 instances cost in Loki
// requests/limits before and after.

const testSharedURL = "http://loki.argus-obs.svc.cluster.local:3100"

func sharedInstance() Instance {
	in := sampleInstance()
	in.ObsMode = "shared"
	in.ObsSharedURL = testSharedURL
	return in
}

func TestInstanceValidate_sharedAccepted(t *testing.T) {
	if _, err := RenderExecutor(sharedInstance()); err != nil {
		t.Fatalf("--obs=shared with a shared URL must render, got: %v", err)
	}
}

// No URL = no place to push or read. Refused, never a silent fallback to the bundled loki:3100
// that shared mode does not deploy.
func TestInstanceValidate_sharedWithoutURLRefused(t *testing.T) {
	in := sharedInstance()
	in.ObsSharedURL = ""
	_, err := RenderExecutor(in)
	if err == nil || !strings.Contains(err.Error(), "--obs=shared") || !strings.Contains(err.Error(), "--obs-shared-url") {
		t.Fatalf("--obs=shared without a shared URL must be refused naming the flag, got: %v", err)
	}
}

// Compose has no cluster to host argus-obs in. The message names the mode AND the tier.
func TestInstanceValidate_sharedOnComposeRefused(t *testing.T) {
	in := sharedInstance()
	in.Tier = "compose"
	_, err := RenderObs(in, cfgJSON(t))
	if err == nil || !strings.Contains(err.Error(), "--obs=shared") || !strings.Contains(err.Error(), "compose") {
		t.Fatalf("--obs=shared on --tier compose must be refused naming mode and tier, got: %v", err)
	}
}

// The tenant IS the instance id. An empty one would make every request unscoped, so it is refused
// here as well as at executor start (cmd/argus), whatever else validate() checks first.
func TestSharedTenant_emptyRefused(t *testing.T) {
	if err := sharedTenantCheck(""); err == nil || !strings.Contains(err.Error(), "unscoped") {
		t.Fatalf("an empty shared tenant must be refused as unscoped, got: %v", err)
	}
	if err := sharedTenantCheck("memstore-k3d"); err != nil {
		t.Fatalf("a non-empty tenant must pass, got: %v", err)
	}
}

// Under shared: NO per-instance Loki (Deployment, Service, loki-config ConfigMap), a promtail that
// pushes to <shared>/loki/api/v1/push with tenant_id: <instance-id>, and the pushgateway kept.
func TestRenderObs_shared_noInstanceLoki_promtailTenant(t *testing.T) {
	got, err := RenderObs(sharedInstance(), cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(shared): %v", err)
	}
	for _, d := range docs(t, got) {
		name := nameOf(d)
		if name == "loki" || name == "loki-config" {
			t.Errorf("shared mode rendered per-instance %v %q — the Loki is the environment's, not the instance's", d["kind"], name)
		}
	}
	if !strings.Contains(got, "- url: "+testSharedURL+"/loki/api/v1/push\n") {
		t.Errorf("promtail must push to the shared Loki's push API, got:\n%s", got)
	}
	if !strings.Contains(got, "tenant_id: memstore-k3d\n") {
		t.Errorf("promtail must write with tenant_id: <instance-id>, got:\n%s", got)
	}
	if !strings.Contains(got, "name: pushgateway") {
		t.Error("the pushgateway stays per-instance under shared (out of scope)")
	}
}

// The scrape side (labels + pipeline) must be byte-identical to bundled, or the same SUT reads
// differently on the dashboard depending on the obs mode.
func TestRenderObs_shared_pipelineMatchesBundled(t *testing.T) {
	bundled, err := RenderObs(sampleInstance(), cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := RenderObs(sharedInstance(), cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	cut := func(s string) string {
		i := strings.Index(s, "    scrape_configs:")
		j := strings.Index(s, "              event_type:\n")
		if i < 0 || j < 0 {
			return ""
		}
		return s[i:j]
	}
	b, s := cut(bundled), cut(shared)
	if b == "" || s == "" {
		t.Fatalf("could not locate the scrape_configs block (bundled=%d shared=%d bytes)", len(b), len(s))
	}
	// Bundled's block carries explanatory comments; compare the non-comment lines.
	strip := func(x string) string {
		var out []string
		for _, l := range strings.Split(x, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	if strip(b) != strip(s) {
		t.Errorf("shared promtail pipeline differs from bundled:\n--- bundled\n%s\n--- shared\n%s", strip(b), strip(s))
	}
}

// The executor is given the shared URL and the tenant (and the mode, so it can refuse an empty
// tenant at start). The instance namespace is labelled obs-mode=shared: the shared Loki's
// NetworkPolicy admits exactly those namespaces, and teardown reads it.
func TestRenderExecutor_shared_argsAndNamespaceLabel(t *testing.T) {
	got, err := RenderExecutor(sharedInstance())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- --obs\n            - shared\n",
		"- --loki\n            - \"" + testSharedURL + "\"\n",
		"- --loki-tenant\n            - \"memstore-k3d\"\n",
		"- --pushgateway\n            - http://pushgateway:9091\n",
		"argus.onedroid.ai/obs-mode: shared\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("shared executor render is missing %q", want)
		}
	}
}

// Bundled must see exactly today's render: no tenant, no mode label.
func TestRenderExecutor_bundled_noTenantNoModeLabel(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"--loki-tenant", "obs-mode", "X-Scope-OrgID", "tenant_id"} {
		if strings.Contains(got, never) {
			t.Errorf("bundled executor render must not carry %q", never)
		}
	}
}

// ── the ONE shared Loki ────────────────────────────────────────────────────────────────────

func TestRenderSharedLoki_shape(t *testing.T) {
	for _, tier := range []string{"k3d", "aks"} {
		got, err := RenderSharedLoki(SharedLoki{Tier: tier})
		if err != nil {
			t.Fatalf("%s: RenderSharedLoki: %v", tier, err)
		}
		ds := docs(t, got)
		kinds := kindsOf(ds)
		if kinds["Namespace"] != 1 || kinds["Deployment"] != 1 || kinds["Service"] != 1 || kinds["NetworkPolicy"] < 1 {
			t.Errorf("%s: want 1 Namespace, 1 Deployment, 1 Service, >=1 NetworkPolicy, got %v", tier, kinds)
		}
		for _, d := range ds {
			if k := d["kind"]; k != "Namespace" && nsOf(d) != "argus-obs" {
				t.Errorf("%s: %v %q is not in namespace argus-obs", tier, k, nameOf(d))
			}
		}
		if !strings.Contains(got, "auth_enabled: true\n") {
			t.Errorf("%s: the shared Loki must run auth_enabled: true (tenancy fails CLOSED)", tier)
		}
		if strings.Contains(got, "auth_enabled: false") {
			t.Errorf("%s: auth_enabled: false must not appear in the shared Loki", tier)
		}
		if !regexp.MustCompile(`image: grafana/loki:[0-9.]+@sha256:[0-9a-f]{64}\n`).MatchString(got) {
			t.Errorf("%s: the shared Loki image must be tag@digest pinned", tier)
		}
		if !strings.Contains(got, "retention_period: ") || !strings.Contains(got, "retention_enabled: true") {
			t.Errorf("%s: the shared Loki must set a retention period", tier)
		}
	}
}

func TestRenderSharedLoki_serviceTypePerTier(t *testing.T) {
	k3d, _ := RenderSharedLoki(SharedLoki{Tier: "k3d"})
	aks, _ := RenderSharedLoki(SharedLoki{Tier: "aks"})
	if !strings.Contains(k3d, "type: NodePort") {
		t.Error("local tier: the shared Loki Service must be a NodePort (the Grafana is off-cluster)")
	}
	if strings.Contains(aks, "NodePort") {
		t.Error("managed tier: the shared Loki must stay ClusterIP")
	}
}

// The NetworkPolicy admits ONLY instance namespaces running shared mode and the shared Grafana.
// Nothing with an empty `from`, and never a bare `- {}` peer.
func TestRenderSharedLoki_networkPolicyAdmitsOnlyInstancesAndGrafana(t *testing.T) {
	for _, tier := range []string{"k3d", "aks"} {
		got, _ := RenderSharedLoki(SharedLoki{Tier: tier})
		var pol map[string]any
		for _, d := range docs(t, got) {
			if d["kind"] == "NetworkPolicy" && nameOf(d) == "shared-loki-ingress" {
				pol = d
			}
		}
		if pol == nil {
			t.Fatalf("%s: no shared-loki-ingress NetworkPolicy", tier)
		}
		spec := pol["spec"].(map[string]any)
		ingress, _ := spec["ingress"].([]any)
		if len(ingress) == 0 {
			t.Fatalf("%s: shared-loki-ingress has no ingress rules", tier)
		}
		var sawInstances, sawGrafana bool
		for _, r := range ingress {
			rule := r.(map[string]any)
			from, _ := rule["from"].([]any)
			if len(from) == 0 {
				t.Errorf("%s: an ingress rule with no `from` admits every source", tier)
			}
			for _, p := range from {
				peer := p.(map[string]any)
				if len(peer) == 0 {
					t.Errorf("%s: an empty peer admits every source", tier)
				}
				if ns, ok := peer["namespaceSelector"].(map[string]any); ok {
					ml, _ := ns["matchLabels"].(map[string]any)
					if ml["argus.onedroid.ai/obs-mode"] == "shared" {
						sawInstances = true
					}
					if ml["kubernetes.io/metadata.name"] == "argus-dev" {
						sawGrafana = true
					}
				}
				if _, ok := peer["ipBlock"]; ok && tier == "k3d" {
					sawGrafana = true // the off-cluster Grafana on a local tier
				}
			}
		}
		if !sawInstances || !sawGrafana {
			t.Errorf("%s: want a peer for shared instance namespaces (%v) and one for the shared Grafana (%v)", tier, sawInstances, sawGrafana)
		}
		if tier == "aks" && strings.Contains(got, "ipBlock") {
			t.Error("aks: the managed Grafana is in-cluster; no ipBlock peer")
		}
	}
}

func TestRenderSharedLoki_deterministic(t *testing.T) {
	a, _ := RenderSharedLoki(SharedLoki{Tier: "k3d"})
	b, _ := RenderSharedLoki(SharedLoki{Tier: "k3d"})
	if a == "" || a != b {
		t.Error("RenderSharedLoki must be deterministic (applying it twice changes nothing)")
	}
}

// ── the measurement: 20 instances, bundled vs shared ──────────────────────────────────────

type lokiCost struct {
	lokis int
	// millicores and MiB — the only units the Loki renders use; anything else fails the parse.
	reqCPU, reqMem, limCPU, limMem int64
}

// milli parses a k8s CPU quantity ("50m", "1", "0.5") into millicores.
func milli(t *testing.T, s string) int64 {
	t.Helper()
	if v, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("cpu %q: %v", s, err)
		}
		return n
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("cpu %q: %v", s, err)
	}
	return int64(f * 1000)
}

// mebi parses a k8s memory quantity in Mi or Gi into MiB.
func mebi(t *testing.T, s string) int64 {
	t.Helper()
	mult := int64(1)
	v, ok := strings.CutSuffix(s, "Mi")
	if !ok {
		if v, ok = strings.CutSuffix(s, "Gi"); !ok {
			t.Fatalf("memory %q: only Mi/Gi are parsed here", s)
		}
		mult = 1024
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("memory %q: %v", s, err)
	}
	return n * mult
}

// sumLoki adds up the requests/limits of every container named "loki" in the rendered manifest.
// It reads the RENDERED YAML, not a constant: a change to the Loki resources moves the number.
func sumLoki(t *testing.T, manifest string, acc *lokiCost) {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] != "Deployment" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		pspec, _ := tpl["spec"].(map[string]any)
		cs, _ := pspec["containers"].([]any)
		for _, c := range cs {
			cm := c.(map[string]any)
			if cm["name"] != "loki" {
				continue
			}
			acc.lokis++
			res, _ := cm["resources"].(map[string]any)
			get := func(sect, key string) string {
				m, _ := res[sect].(map[string]any)
				s, _ := m[key].(string)
				if s == "" {
					t.Fatalf("loki container has no %s.%s", sect, key)
				}
				return s
			}
			acc.reqCPU += milli(t, get("requests", "cpu"))
			acc.reqMem += mebi(t, get("requests", "memory"))
			acc.limCPU += milli(t, get("limits", "cpu"))
			acc.limMem += mebi(t, get("limits", "memory"))
		}
	}
}

func TestSharedIngest_20Instances_LokiCost(t *testing.T) {
	const n = 20
	c := cfgJSON(t)
	var before, after lokiCost
	for i := 0; i < n; i++ {
		b := sampleInstance()
		b.ID = "inst-" + string(rune('a'+i))
		obs, err := RenderObs(b, c)
		if err != nil {
			t.Fatal(err)
		}
		sumLoki(t, obs, &before)

		s := sharedInstance()
		s.ID = b.ID
		obs, err = RenderObs(s, c)
		if err != nil {
			t.Fatal(err)
		}
		sumLoki(t, obs, &after)
	}
	one, err := RenderSharedLoki(SharedLoki{Tier: "k3d"})
	if err != nil {
		t.Fatal(err)
	}
	sumLoki(t, one, &after)

	if before.lokis != n {
		t.Errorf("bundled: want %d Lokis for %d instances, got %d", n, n, before.lokis)
	}
	if after.lokis != 1 {
		t.Errorf("shared: want 1 Loki for %d instances, got %d", n, after.lokis)
	}
	t.Logf("RENDERED Loki requests/limits for %d instances (not measured live usage):", n)
	t.Logf("  bundled: %2d Lokis  requests cpu=%dm mem=%dMi  limits cpu=%dm mem=%dMi",
		before.lokis, before.reqCPU, before.reqMem, before.limCPU, before.limMem)
	t.Logf("  shared : %2d Loki   requests cpu=%dm mem=%dMi  limits cpu=%dm mem=%dMi",
		after.lokis, after.reqCPU, after.reqMem, after.limCPU, after.limMem)
}
