package k8srender

// sutlogs_api_test.go —: render-k8s --collect-sut-logs --sut-logs-via api.
//
// PROMISE: with `api` the obs plane carries NO hostPath, NO DaemonSet and nothing cluster-scoped, a one
// replica `sut-logs` Deployment with an `argus collect-logs` command in its place, in every mode promtail
// renders in; `node` (the default) is byte-for-byte what it was before the flag existed.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func apiInstance() Instance {
	in := sampleInstance()
	in.CollectSUTLogs = true
	in.SUTLogsVia = "api"
	return in
}

func renderObsFor(t *testing.T, in Instance) string {
	t.Helper()
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs: %v", err)
	}
	return out
}

func apiExportInstance() Instance {
	in := apiInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "https://logs-example.grafana.net"
	in.ObsLokiPushURL = "https://logs-example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"
	return in
}

func apiSharedInstance() Instance {
	in := apiInstance()
	in.ObsMode = "shared"
	in.ObsSharedURL = "http://loki.argus-obs.svc.cluster.local:3100"
	return in
}

// every mode promtail renders in, each with its tier of interest
func apiModes() map[string]Instance {
	return map[string]Instance{"bundled": apiInstance(), "export": apiExportInstance(), "shared": apiSharedInstance()}
}

func findDoc(t *testing.T, ds []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range ds {
		if d["kind"] == kind && nameOf(d) == name {
			return d
		}
	}
	return nil
}

func collectorContainer(t *testing.T, ds []map[string]any) map[string]any {
	t.Helper()
	dep := findDoc(t, ds, "Deployment", "sut-logs")
	if dep == nil {
		t.Fatal("no Deployment sut-logs rendered")
	}
	spec := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	cs := spec["containers"].([]any)
	if len(cs) != 1 {
		t.Fatalf("containers = %d, want 1", len(cs))
	}
	return cs[0].(map[string]any)
}

func envOf(c map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// api: no hostPath, no DaemonSet, nothing cluster-scoped, no promtail — on a LOCAL tier too (where `node`
// binds a ClusterRole), in every mode.
func TestSUTLogsAPI_RendersNoHostPathNoDaemonSetNothingClusterScoped(t *testing.T) {
	for name, in := range apiModes() {
		for _, tier := range []string{"k3d", "aks"} {
			in := in
			in.Tier = tier
			in.Cluster = tier
			out := renderObsFor(t, in)
			if strings.Contains(out, "hostPath") {
				t.Errorf("%s/%s: api rendered a hostPath:\n%s", name, tier, out)
			}
			// (a header comment may still SAY "promtail"; no object is one, and nothing reads the node's logs)
			if strings.Contains(out, "DaemonSet") || strings.Contains(out, "/var/log/pods") || strings.Contains(out, "name: promtail") || strings.Contains(out, "app: promtail") {
				t.Errorf("%s/%s: api rendered promtail or a DaemonSet", name, tier)
			}
			for _, k := range []string{"ClusterRole", "ClusterRoleBinding"} {
				if strings.Contains(out, "kind: "+k) {
					t.Errorf("%s/%s: api rendered a %s", name, tier, k)
				}
			}
			if strings.Contains(out, "hostNetwork") {
				t.Errorf("%s/%s: hostNetwork", name, tier)
			}
			ds := docs(t, out)
			for _, d := range ds {
				switch d["kind"] {
				case "Namespace", "Node", "PersistentVolume":
					t.Errorf("%s/%s: cluster-scoped %v", name, tier, d["kind"])
				}
				if nsOf(d) == "" {
					t.Errorf("%s/%s: %v %s has no namespace", name, tier, d["kind"], nameOf(d))
				}
			}
			if findDoc(t, ds, "ServiceAccount", "argus-sut-logs") == nil {
				t.Errorf("%s/%s: no ServiceAccount argus-sut-logs", name, tier)
			}
			if findDoc(t, ds, "Deployment", "sut-logs") == nil {
				t.Errorf("%s/%s: no Deployment sut-logs", name, tier)
			}
		}
	}
}

func TestSUTLogsAPI_DeploymentShape(t *testing.T) {
	for name, in := range apiModes() {
		ds := docs(t, renderObsFor(t, in))
		dep := findDoc(t, ds, "Deployment", "sut-logs")
		if dep == nil {
			t.Fatalf("%s: no Deployment", name)
		}
		spec := dep["spec"].(map[string]any)
		if spec["replicas"] != 1 {
			t.Errorf("%s: replicas = %v, want 1", name, spec["replicas"])
		}
		if st := spec["strategy"].(map[string]any); st["type"] != "Recreate" {
			t.Errorf("%s: strategy = %v, want Recreate", name, st)
		}
		pod := spec["template"].(map[string]any)["spec"].(map[string]any)
		if pod["serviceAccountName"] != "argus-sut-logs" {
			t.Errorf("%s: serviceAccountName = %v", name, pod["serviceAccountName"])
		}
		c := collectorContainer(t, ds)
		if c["image"] != in.Image {
			t.Errorf("%s: image = %v, want the executor's %q", name, c["image"], in.Image)
		}
		if _, hasCmd := c["command"]; hasCmd {
			t.Errorf("%s: command: would replace the ENTRYPOINT; use args", name)
		}
		args := c["args"].([]any)
		if args[0] != "collect-logs" || args[1] != "--config" || args[2] != "/config/argus-config.yaml" {
			t.Errorf("%s: args = %v", name, args)
		}
		sc := c["securityContext"].(map[string]any)
		if sc["allowPrivilegeEscalation"] != false {
			t.Errorf("%s: allowPrivilegeEscalation = %v", name, sc["allowPrivilegeEscalation"])
		}
		if drop := sc["capabilities"].(map[string]any)["drop"].([]any); len(drop) != 1 || drop[0] != "ALL" {
			t.Errorf("%s: capabilities.drop = %v", name, drop)
		}
		if _, set := sc["runAsNonRoot"]; set {
			t.Errorf("%s: runAsNonRoot is set; the image runs as root and that is a separate change", name)
		}
		res := c["resources"].(map[string]any)
		req, lim := res["requests"].(map[string]any), res["limits"].(map[string]any)
		if req["cpu"] != "25m" || req["memory"] != "64Mi" || lim["cpu"] != "200m" || lim["memory"] != "256Mi" {
			t.Errorf("%s: resources = %v", name, res)
		}
		env := envOf(c)
		for k, want := range map[string]string{"ARGUS_INSTANCE_ID": in.ID, "ARGUS_SUT_NAMESPACE": in.SUTNamespace, "ARGUS_CLUSTER": in.Cluster} {
			if env[k]["value"] != want {
				t.Errorf("%s: env %s = %v, want %q", name, k, env[k]["value"], want)
			}
		}
		// the config is mounted read-only; no other volume is a host path
		vols := pod["volumes"].([]any)
		if len(vols) != 1 || vols[0].(map[string]any)["name"] != "config" {
			t.Errorf("%s: volumes = %v", name, vols)
		}
		mounts := c["volumeMounts"].([]any)
		if m := mounts[0].(map[string]any); m["mountPath"] != "/config" || m["readOnly"] != true {
			t.Errorf("%s: mount = %v", name, m)
		}
	}
}

func TestSUTLogsAPI_ArgsAndEnvPerMode(t *testing.T) {
	// bundled: the instance Loki
	ds := docs(t, renderObsFor(t, apiInstance()))
	args := collectorContainer(t, ds)["args"].([]any)
	if got := asStrings(args[3:]); strings.Join(got, " ") != "--loki http://loki:3100" {
		t.Errorf("bundled args tail = %v", got)
	}

	// shared: the environment's Loki as tenant = the instance id
	sh := apiSharedInstance()
	ds = docs(t, renderObsFor(t, sh))
	args = collectorContainer(t, ds)["args"].([]any)
	if got := strings.Join(asStrings(args[3:]), " "); got != "--obs shared --loki "+sh.ObsSharedURL+" --loki-tenant "+sh.ID {
		t.Errorf("shared args tail = %q", got)
	}

	// export: the operator's Loki; the credential only by secretKeyRef
	ex := apiExportInstance()
	out := renderObsFor(t, ex)
	ds = docs(t, out)
	c := collectorContainer(t, ds)
	if got := strings.Join(asStrings(c["args"].([]any)[3:]), " "); got != "--loki "+ex.ObsLokiURL {
		t.Errorf("export args tail = %q", got)
	}
	cred := envOf(c)["LOKI_CREDENTIAL"]
	ref := cred["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
	if ref["name"] != "argus-obs-credential" || ref["key"] != "LOKI_CREDENTIAL" {
		t.Errorf("credential env = %v", cred)
	}
	if _, literal := cred["value"]; literal {
		t.Error("the credential is rendered as a literal value")
	}
	// bundled and shared carry no credential env
	for _, in := range []Instance{apiInstance(), sh} {
		if _, has := envOf(collectorContainer(t, docs(t, renderObsFor(t, in))))["LOKI_CREDENTIAL"]; has {
			t.Errorf("%s: credential env rendered without an export credential", in.ObsMode)
		}
	}
	// export renders the collector ONLY: no Loki, no pushgateway (as promtail-only does)
	if findDoc(t, ds, "Deployment", "loki") != nil || findDoc(t, ds, "Deployment", "pushgateway") != nil {
		t.Error("export rendered a Loki or pushgateway")
	}
}

func asStrings(a []any) []string {
	var out []string
	for _, v := range a {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// The same pull-secret handling as the executor.
func TestSUTLogsAPI_ImagePullPolicyAndSecretFollowTheExecutor(t *testing.T) {
	in := apiInstance()
	in.Tier, in.Cluster = "aks", "aks" // normalize gives ghcr-pull, Always
	ds := docs(t, renderObsFor(t, in))
	dep := findDoc(t, ds, "Deployment", "sut-logs")
	pod := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	sec := pod["imagePullSecrets"].([]any)
	if len(sec) != 1 || sec[0].(map[string]any)["name"] != "ghcr-pull" {
		t.Errorf("imagePullSecrets = %v", sec)
	}
	if got := collectorContainer(t, ds)["imagePullPolicy"]; got != "Always" {
		t.Errorf("imagePullPolicy = %v", got)
	}
}

// api needs --collect-sut-logs; adopt / none / BetterStack still render nothing, exactly as promtail does.
func TestSUTLogsAPI_GatesLikePromtail(t *testing.T) {
	in := sampleInstance()
	in.CollectSUTLogs = false
	in.SUTLogsVia = "api"
	if _, err := RenderObs(in, cfgJSON(t)); err == nil || !strings.Contains(err.Error(), "--collect-sut-logs") {
		t.Errorf("api without collect-sut-logs: err = %v", err)
	}
	in.SUTLogsVia = "sideways"
	if _, err := RenderObs(in, cfgJSON(t)); err == nil || !strings.Contains(err.Error(), "node or api") {
		t.Errorf("unknown value: err = %v", err)
	}
	for _, mode := range []string{"adopt", "none"} {
		in := apiInstance()
		in.ObsMode = mode
		in.ObsLokiURL = "http://own-loki:3100"
		if out := renderObsFor(t, in); strings.TrimSpace(out) != "" {
			t.Errorf("%s with api rendered %d bytes", mode, len(out))
		}
	}
	bs := apiExportInstance()
	bs.ObsLokiURL, bs.ObsLokiPushURL, bs.ObsCredentialVarName, bs.ObsUseBetterStack = "", "", "", true
	if out := renderObsFor(t, bs); strings.TrimSpace(out) != "" {
		t.Errorf("BetterStack export with api rendered %d bytes", len(out))
	}
}

// The executor Deployment/Namespace/quota are untouched by the flag.
func TestSUTLogsAPI_ExecutorRenderIsUnchanged(t *testing.T) {
	a, err := RenderExecutor(apiInstance())
	if err != nil {
		t.Fatal(err)
	}
	n := apiInstance()
	n.SUTLogsVia = ""
	b, err := RenderExecutor(n)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("executor.yaml differs between api and node")
	}
}

// Quota: the formula is unchanged and still holds everything obs.yaml emits with the collector (one pod,
// where promtail took one per node).
func TestSUTLogsAPI_QuotaStillFitsEverythingTheRendererEmits(t *testing.T) {
	const (
		executorCPU, lokiCPU, pushgatewayCPU, collectorCPU, oneShotCPU = 1000, 500, 200, 200, 500
		executorMem, lokiMem, pushgatewayMem, collectorMem             = 1536, 512, 256, 256
	)
	for _, replicas := range []int{1, 3, 5} {
		in := apiInstance()
		in.Replicas = replicas
		exec, err := RenderExecutor(in)
		if err != nil {
			t.Fatal(err)
		}
		obs := renderObsFor(t, in)
		// the collector's limits, read from what was rendered, not restated
		c := collectorContainer(t, docs(t, obs))
		lim := c["resources"].(map[string]any)["limits"].(map[string]any)
		if lim["cpu"] != "200m" || lim["memory"] != "256Mi" {
			t.Fatalf("collector limits = %v", lim)
		}
		needCPU := replicas*executorCPU + lokiCPU + pushgatewayCPU + collectorCPU + oneShotCPU
		needMem := replicas*executorMem + lokiMem + pushgatewayMem + collectorMem
		haveCPU := 1000 * atoi(t, quotaLine(t, exec, "limits.cpu"))
		haveMem := atoi(t, strings.TrimSuffix(quotaLine(t, exec, "limits.memory"), "Mi"))
		if haveCPU < needCPU || haveMem < needMem {
			t.Errorf("replicas=%d: quota cpu %dm mem %dMi cannot fit %dm / %dMi", replicas, haveCPU, haveMem, needCPU, needMem)
		}
		// pods: executors + loki + pushgateway + collector + one-shots
		if pods := atoi(t, quotaLine(t, exec, "pods")); pods < quotaReplicas(replicas)+3+2 {
			t.Errorf("replicas=%d: pods quota %d", replicas, pods)
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a whole number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ── the access-role file ──────────────────────────────────────────────────────────────────────────

func TestSUTAccessRole_WithAPI_AddsASecondRoleAfterTheFirst(t *testing.T) {
	in := apiInstance()
	out := SUTAccessRoleManifest(in)
	ds := docs(t, out)
	if len(ds) != 4 {
		t.Fatalf("docs = %d, want 4 (Role, RoleBinding, Role, RoleBinding)", len(ds))
	}
	if nameOf(ds[0]) != "argus-load-environment-read" || nameOf(ds[1]) != "argus-load-environment-read" {
		t.Errorf("the first two docs are no longer the existing grant: %s, %s", nameOf(ds[0]), nameOf(ds[1]))
	}
	role, binding := ds[2], ds[3]
	if role["kind"] != "Role" || nameOf(role) != "argus-sut-logs-read" || nsOf(role) != in.SUTNamespace {
		t.Errorf("second Role = %v %s in %q", role["kind"], nameOf(role), nsOf(role))
	}
	rules := role["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("rules = %v, want exactly pods get,list and pods/log get", rules)
	}
	want := []struct {
		res   string
		verbs string
	}{{"pods", "get,list"}, {"pods/log", "get"}}
	for i, w := range want {
		r := rules[i].(map[string]any)
		res := strings.Join(asStrings(r["resources"].([]any)), ",")
		verbs := strings.Join(asStrings(r["verbs"].([]any)), ",")
		groups := asStrings(r["apiGroups"].([]any))
		if res != w.res || verbs != w.verbs || len(groups) != 1 || groups[0] != "" {
			t.Errorf("rule %d = %v", i, r)
		}
	}
	if binding["kind"] != "RoleBinding" || nsOf(binding) != in.SUTNamespace {
		t.Errorf("binding = %v in %q", binding["kind"], nsOf(binding))
	}
	if ref := binding["roleRef"].(map[string]any); ref["kind"] != "Role" || ref["name"] != "argus-sut-logs-read" {
		t.Errorf("roleRef = %v", ref)
	}
	subj := binding["subjects"].([]any)[0].(map[string]any)
	if subj["kind"] != "ServiceAccount" || subj["name"] != "argus-sut-logs" || subj["namespace"] != in.Namespace() {
		t.Errorf("subject = %v", subj)
	}
	// the header says in plain words what the second grant lets Argus read
	if !strings.Contains(out, "EVERY container's log lines of EVERY pod in") {
		t.Error("the second grant does not say what it lets Argus read")
	}
	// the existing Role still does not grant pods/log
	first := out[:strings.Index(out, "SECOND GRANT")]
	if strings.Contains(first, "pods/log") {
		t.Error("the existing Role grants pods/log")
	}
}

func TestSUTAccessRole_WithoutAPI_IsTheFileItAlwaysWas(t *testing.T) {
	plain := sampleInstance()
	viaNode := sampleInstance()
	viaNode.SUTLogsVia = "node"
	api := apiInstance()
	if SUTAccessRoleManifest(plain) != SUTAccessRoleManifest(viaNode) {
		t.Error("node changed the access-role file")
	}
	if !strings.HasPrefix(SUTAccessRoleManifest(api), SUTAccessRoleManifest(plain)) {
		t.Error("api changed the first grant")
	}
	// and the node file is pinned to the base by hash (testdata/node-golden.sha256, taken at the base commit)
	assertGolden(t, "access-role", SUTAccessRoleManifest(plain))
}

// ── `node` is byte-identical to the base ──────────────────────────────────────────────────────────

func TestSUTLogsNode_RendersByteIdenticalToTheBase(t *testing.T) {
	cases := map[string]Instance{"bundled": sampleInstance(), "export": apiExportInstance(), "shared": apiSharedInstance()}
	for name, in := range cases {
		in.SUTLogsVia = "" // today's default
		out := renderObsFor(t, in)
		assertGolden(t, "obs-"+name, out)
		in.SUTLogsVia = "node" // spelled out: the same bytes
		if again := renderObsFor(t, in); again != out {
			t.Errorf("%s: --sut-logs-via node differs from the default", name)
		}
		exec, err := RenderExecutor(in)
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "executor-"+name, exec)
	}
}

// assertGolden compares sha256(got) with testdata/node-golden/<name>.sha256, taken by rendering the same
// Instance with the BASE commit's renderer (2dec4e02), before this change existed.
//
// the three executor-* goldens were RE-TAKEN when the executor Role gained the precondition
// probe's pods/networkpolicies rights and allow-same-namespace gained its probe-listener exclusion
// (preconditions_rbac_test.go pins both). The obs-* and access-role goldens are the base's, untouched.
//
// AC-D70: the three executor-* goldens were RE-TAKEN once more when two YAML comments in the executor manifest named
// the runner tools with one underscore (`runner_*`); measured before re-taking: the new render with the old spelling
// put back hashed to the previous golden for all three, so those two comment lines are the only change.
//
// In this repository ALL SEVEN goldens were re-taken once more: the rendered manifests carry a few YAML comments, and
// the comments in this tree name no internal record ids and no internal project names, so every render hashes
// differently from the private build's. From here the goldens pin `node` against this tree's own previous output.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "node-golden", name+".sha256"))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	sum := sha256.Sum256([]byte(got))
	if have := hex.EncodeToString(sum[:]); have != strings.TrimSpace(string(b)) {
		t.Errorf("%s: sha256 %s, the base rendered %s: `node` is no longer byte-identical", name, have, strings.TrimSpace(string(b)))
	}
}
