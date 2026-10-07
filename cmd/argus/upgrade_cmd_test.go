package main

// upgrade_cmd_test.go — `argus upgrade --instance <id>` (onboarding review items 11 and 22).
//
// No live cluster, no database. `kubectl` is a stub on PATH: a shell shim that re-executes THIS test
// binary (TestKubectlStub) as a small fake API server over a directory of JSON files — `get` reads
// them, `patch` merges into them, every other verb is only recorded. Because the fake keeps state, the
// "second run reports nothing" promise is tested against what the first run actually wrote.
//
// The "running instance" is produced by the REAL render-k8s from older inputs, then given the fields a
// server adds (replicas scaled by the executor, clusterIP, canonical quantities, status).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8supgrade"
)

const (
	upInst      = "upg1"
	upNS        = "argus-inst-upg1"
	upOldDigest = "registry.invalid/argus@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// ---- the fake kubectl ------------------------------------------------------------------------------

func TestKubectlStub(t *testing.T) {
	if os.Getenv("ARGUS_KUBECTL_STUB") != "1" {
		t.Skip("helper process for upgrade_cmd_test.go, not a test")
	}
	dir := os.Getenv("STUBDIR")
	args := stubArgs()
	stdin, _ := ioReadAll(os.Stdin)
	appendFile(filepath.Join(dir, "calls.txt"), strings.Join(args, " ")+"\n")
	rest := args
	for len(rest) >= 2 && (rest[0] == "--kubeconfig" || rest[0] == "--context") {
		rest = rest[2:]
	}
	if len(rest) == 0 {
		os.Exit(0)
	}
	switch rest[0] {
	case "config":
		if e := os.Getenv("STUB_CONFIG_ERROR"); e != "" {
			os.Stderr.WriteString(e + "\n")
			os.Exit(1)
		}
		ctx, server := "stub-ctx", "https://stub.example.invalid:6443"
		for i, a := range args {
			if a == "--context" && i+1 < len(args) {
				ctx = args[i+1]
			}
		}
		if sv := os.Getenv("STUB_SERVER"); sv != "" {
			server = sv
		}
		out, _ := json.Marshal(map[string]any{
			"current-context": ctx,
			"contexts":        []any{map[string]any{"name": ctx, "context": map[string]any{"cluster": "stub-cluster"}}},
			"clusters":        []any{map[string]any{"name": "stub-cluster", "cluster": map[string]any{"server": server}}},
		})
		os.Stdout.Write(out)
	case "get":
		if e := os.Getenv("STUB_GET_ERROR"); e != "" {
			os.Stderr.WriteString(e + "\n")
			os.Exit(1)
		}
		if nk := os.Getenv("STUB_NOKIND"); nk != "" && rest[1] == nk {
			os.Stderr.WriteString("error: the server doesn't have a resource type \"" + strings.SplitN(nk, ".", 2)[0] + "\"\n")
			os.Exit(1)
		}
		file := filepath.Join(dir, "live", rest[1]+"__"+rest[2]+".json")
		if rest[1] == "secret" {
			// The ONLY way a Secret may be asked about: existence, by name. Anything else is a read.
			if len(rest) != 7 || rest[3] != "-n" || rest[5] != "-o" || rest[6] != "name" {
				os.Stderr.WriteString("STUB: a Secret was read other than by `-o name`\n")
				os.Exit(1)
			}
			if _, err := os.Stat(file); err != nil {
				os.Stderr.WriteString("Error from server (NotFound): secrets \"" + rest[2] + "\" not found\n")
				os.Exit(1)
			}
			os.Stdout.WriteString("secret/" + rest[2] + "\n")
			os.Exit(0)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			os.Stderr.WriteString("Error from server (NotFound): " + rest[1] + " \"" + rest[2] + "\" not found\n")
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case "create":
		if len(rest) < 3 || rest[1] != "-f" || rest[2] != "-" {
			os.Stderr.WriteString("STUB: create must be `create -f -`\n")
			os.Exit(1)
		}
		var doc map[string]any
		if err := json.Unmarshal(stdin, &doc); err != nil {
			os.Stderr.WriteString("bad manifest: " + err.Error() + "\n")
			os.Exit(1)
		}
		kind, _ := doc["kind"].(string)
		res := strings.ToLower(kind)
		if av, _ := doc["apiVersion"].(string); strings.Contains(av, "/") {
			res += "." + strings.SplitN(av, "/", 2)[0]
		}
		name, _ := doc["metadata"].(map[string]any)["name"].(string)
		if stubFails("create", res+"/"+name) {
			os.Stderr.WriteString("Error from server (Forbidden): " + res + " \"" + name + "\" is forbidden\n")
			os.Exit(1)
		}
		file := filepath.Join(dir, "live", res+"__"+name+".json")
		if _, err := os.Stat(file); err == nil {
			os.Stderr.WriteString("Error from server (AlreadyExists): " + res + " \"" + name + "\" already exists\n")
			os.Exit(1)
		}
		out, _ := json.Marshal(stubStrip(doc))
		_ = os.WriteFile(file, out, 0o644)
		appendFile(filepath.Join(dir, "creates.txt"), res+"/"+name+" "+string(stdin)+"\n")
	case "patch":
		if stubFails("patch", rest[1]+"/"+rest[2]) {
			os.Stderr.WriteString("Error from server (Forbidden): " + rest[1] + " \"" + rest[2] + "\" is forbidden\n")
			os.Exit(1)
		}
		file := filepath.Join(dir, "live", rest[1]+"__"+rest[2]+".json")
		b, err := os.ReadFile(file)
		if err != nil {
			os.Stderr.WriteString("Error from server (NotFound)\n")
			os.Exit(1)
		}
		var live, patch map[string]any
		_ = json.Unmarshal(b, &live)
		if err := json.Unmarshal(stdin, &patch); err != nil {
			os.Stderr.WriteString("bad patch: " + err.Error() + "\n")
			os.Exit(1)
		}
		var merged map[string]any
		if stubPatchType(rest) == "merge" {
			merged = stubJSONMerge(live, patch)
		} else {
			merged = stubMerge(live, patch, "")
		}
		// The API server validates the object a patch produces: a Service whose ports repeat a name is
		// refused (a strategic patch that keys ports by number and finds a NEW number ADDS a second
		// entry that still carries the old name).
		if msg := stubServicePortNames(merged); msg != "" {
			os.Stderr.WriteString("The Service \"" + rest[2] + "\" is invalid: " + msg + "\n")
			os.Exit(1)
		}
		out, _ := json.Marshal(merged)
		_ = os.WriteFile(file, out, 0o644)
		appendFile(filepath.Join(dir, "patches.txt"), rest[1]+"/"+rest[2]+" "+string(stdin)+"\n")
	}
	os.Exit(0)
}

// stubFails is true when STUB_FAIL ("verb:resource/name,verb:resource/name") names this call.
func stubFails(verb, target string) bool {
	for _, f := range strings.Split(os.Getenv("STUB_FAIL"), ",") {
		if f != "" && f == verb+":"+target {
			return true
		}
	}
	return false
}

// stubPatchType is the value of `--type=` in a patch call (kubectl's default is strategic).
func stubPatchType(rest []string) string {
	for _, a := range rest {
		if strings.HasPrefix(a, "--type=") {
			return strings.TrimPrefix(a, "--type=")
		}
	}
	return "strategic"
}

// stubJSONMerge is RFC 7386: objects merge key by key, null deletes, every other value (arrays
// included) REPLACES what was there.
func stubJSONMerge(live, patch map[string]any) map[string]any {
	for k, pv := range patch {
		if pv == nil {
			delete(live, k)
			continue
		}
		if pm, ok := pv.(map[string]any); ok {
			if lm, ok := live[k].(map[string]any); ok {
				live[k] = stubJSONMerge(lm, pm)
				continue
			}
		}
		live[k] = pv
	}
	return live
}

// stubServicePortNames is the API server's "duplicate port name" refusal for a Service: "" when the
// object is fine (or is not a Service).
func stubServicePortNames(obj map[string]any) string {
	if obj["kind"] != "Service" {
		return ""
	}
	spec, _ := obj["spec"].(map[string]any)
	ports, _ := spec["ports"].([]any)
	seen := map[string]bool{}
	for i, p := range ports {
		pm, _ := p.(map[string]any)
		name, _ := pm["name"].(string)
		if name == "" {
			continue
		}
		if seen[name] {
			return fmt.Sprintf("spec.ports[%d].name: Duplicate value: %q", i, name)
		}
		seen[name] = true
	}
	return ""
}

// stubMerge is strategic-merge-ish, with the API server's own list merge keys: volumeMounts by
// mountPath, container ports by containerPort, Service ports by port, every other list of named
// maps by name; any other list is replaced. `parent` is the key of the map being merged.
func stubMerge(live, patch map[string]any, parent string) map[string]any {
	for k, pv := range patch {
		if k == "$retainKeys" {
			continue
		}
		lv, ok := live[k]
		if !ok {
			live[k] = stubStrip(pv)
			continue
		}
		switch p := pv.(type) {
		case map[string]any:
			if lm, ok := lv.(map[string]any); ok {
				if rk, has := p["$retainKeys"].([]any); has {
					keep := map[string]bool{}
					for _, x := range rk {
						keep[x.(string)] = true
					}
					for lk := range lm {
						if !keep[lk] {
							delete(lm, lk)
						}
					}
				}
				live[k] = stubMerge(lm, p, k)
			} else {
				live[k] = stubStrip(pv)
			}
		case []any:
			ll, ok := lv.([]any)
			if !ok || len(p) == 0 {
				live[k] = stubStrip(pv)
				continue
			}
			key := stubListKey(k, parent, p)
			if key == "" {
				live[k] = stubStrip(pv)
				continue
			}
			for _, pe := range p {
				pm := pe.(map[string]any)
				found := false
				for i, le := range ll {
					if lm, ok := le.(map[string]any); ok && fmt.Sprint(lm[key]) == fmt.Sprint(pm[key]) {
						ll[i] = stubMerge(lm, pm, k)
						found = true
					}
				}
				if !found {
					ll = append(ll, stubStrip(pe))
				}
			}
			live[k] = ll
		default:
			live[k] = pv
		}
	}
	return live
}

func stubListKey(field, parent string, p []any) string {
	first, isMap := p[0].(map[string]any)
	if !isMap {
		return ""
	}
	switch {
	case field == "volumeMounts":
		return "mountPath"
	case field == "ports" && first["containerPort"] != nil:
		return "containerPort"
	case field == "ports" && parent == "spec": // a Service's ports
		return "port"
	case field == "ports":
		return "" // e.g. NetworkPolicy ports: an atomic list
	case first["name"] != nil:
		return "name"
	}
	return ""
}

func stubStrip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "$retainKeys")
		for k, x := range t {
			t[k] = stubStrip(x)
		}
	case []any:
		for i, x := range t {
			t[i] = stubStrip(x)
		}
	}
	return v
}

// ---- harness ---------------------------------------------------------------------------------------

type upCluster struct {
	dir string
	cfg string
}

func stubArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func ioReadAll(f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil || (st.Mode()&os.ModeCharDevice) != 0 {
		return nil, err
	}
	var b []byte
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			return b, nil
		}
	}
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

const upConfig = "project:\n  name: upgrade-test\ntargets:\n  http:\n    base_url: https://example.invalid\n"

// The three token values are BUILT AT RUNTIME. A literal in this file was once rewritten to
// "[REDACTED]" by a commit-time redactor, after which the leak test searched for needles that occurred
// nowhere and could not fail. A value assembled here cannot be matched in source; the leak test uses
// these same variables as its needles.
var (
	upRunnerTok = strings.Join([]string{"zq", "runner", "marker", "one"}, "-")
	upAuthorTok = strings.Join([]string{"zq", "author", "marker", "two"}, "-")
	upEnrollTok = strings.Join([]string{"zq", "enroll", "marker", "three"}, "-")
	// The two identity keys (base64, as ARGUS_IDENTITY_KEY_B64 carries them) are built the same way: a
	// base64 literal is exactly what a redactor rewrites.
	upIdentOldB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join([]string{"old", "fake", "key"}, "-")))
	upIdentNewB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join([]string{"new", "fake", "key"}, "-")))
)

func upEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"ARGUS_RUNNER_TOKEN":       upRunnerTok,
		"ARGUS_AUTHOR_TOKEN":       upAuthorTok,
		"ARGUS_EXECUTOR_SECRET":    "",
		"ARGUS_ENROLLMENT_TOKEN":   upEnrollTok,
		"ARGUS_CP_URL":             "https://cp.example.invalid",
		"ARGUS_WORKSPACE_ID":       "ws_test",
		"ARGUS_GRAFANA_PUBLIC_URL": "",
		"ARGUS_IMAGE_PULL_SECRET":  "",
		"ARGUS_IDENTITY_KEY_B64":   "",
		"ARGUS_KUBE_CONTEXT_HOST":  "",
		"ARGUS_KUBECONFIG_HOST":    "",
		"ARGUS_OBS_PROM_LABEL":     "",
		"ARGUS_VERSION":            "",
		"ARGUS_INSTANCE_ID":        "",
		"ARGUS_TIER":               "",
		"KUBECONFIG":               "",
	} {
		t.Setenv(k, v)
	}
}

// serverize adds what an API server (and the executor's own autoscaler) puts on a live object.
func serverize(o k8supgrade.Object) map[string]any {
	d := o.Doc
	meta, _ := d["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		d["metadata"] = meta
	}
	meta["uid"] = "00000000-fake"
	meta["resourceVersion"] = "1234"
	d["status"] = map[string]any{}
	spec, _ := d["spec"].(map[string]any)
	switch o.Kind {
	case "Deployment":
		if o.Name == "executor" {
			spec["replicas"] = float64(3) // scaled by the executor's own autoscaler
		}
		tpl := spec["template"].(map[string]any)["spec"].(map[string]any)
		for _, c := range tpl["containers"].([]any) {
			c.(map[string]any)["terminationMessagePath"] = "/dev/termination-log"
		}
	case "Service":
		if spec["type"] != "ExternalName" {
			spec["clusterIP"] = "10.43.0.7"
		}
	case "ResourceQuota":
		if h, ok := spec["hard"].(map[string]any); ok {
			h["limits.memory"] = "9Gi" // the server canonicalises 9216Mi
		}
	}
	return d
}

// newCluster renders the instance with the REAL render-k8s from `oldArgs` and installs it in the fake
// cluster, minus Secrets (upgrade must never need them to exist). The stub shim goes on PATH.
func newCluster(t *testing.T, oldEnv map[string]string, oldArgs ...string) *upCluster {
	t.Helper()
	upEnv(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte(upConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range oldEnv {
		t.Setenv(k, v)
	}
	out := filepath.Join(dir, "rendered")
	base := []string{"render-k8s", "--config", cfg, "--instance-id", upInst, "--sut-namespace", "sut",
		"--image", upOldDigest, "--tier", "k3d", "--replicas", "1", "--podmonitor", "off", "--out", out}
	rc := exitOK
	stdout := captureStdout(t, func() {
		captureStderr(t, func() { rc = dispatch(append(base, oldArgs...)) })
	})
	if rc != exitOK {
		t.Fatalf("old render failed (%d): %s", rc, stdout)
	}
	for k := range oldEnv { // the old onboarding's environment does not follow us into the upgrade run
		t.Setenv(k, "")
	}
	upEnv(t)

	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, f := range []string{"executor.yaml", "obs.yaml"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(b))
	}
	objs, err := k8supgrade.Parse(texts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.IsSecret() {
			// A Secret exists in the fixture only as a marker with no data: upgrade may ask whether it
			// exists (`get secret <name> -o name`) and the stub refuses any other way of asking.
			if err := os.WriteFile(filepath.Join(live, "secret__"+o.Name+".json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, _ := json.Marshal(serverize(o))
		if err := os.WriteFile(filepath.Join(live, o.Resource()+"__"+o.Name+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec \"" + self + "\" -test.run='^TestKubectlStub$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_KUBECTL_STUB", "1")
	t.Setenv("STUBDIR", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &upCluster{dir: dir, cfg: cfg}
}

func (c *upCluster) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, "calls.txt"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (c *upCluster) resetCalls() { _ = os.Remove(filepath.Join(c.dir, "calls.txt")) }

func (c *upCluster) patches() string {
	b, _ := os.ReadFile(filepath.Join(c.dir, "patches.txt"))
	return string(b)
}

func (c *upCluster) live(t *testing.T, resource, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, "live", resource+"__"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (c *upCluster) remove(resource, name string) {
	_ = os.Remove(filepath.Join(c.dir, "live", resource+"__"+name+".json"))
}

func (c *upCluster) setLive(t *testing.T, resource, name string, mutate func(map[string]any)) {
	t.Helper()
	m := c.live(t, resource, name)
	mutate(m)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(c.dir, "live", resource+"__"+name+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// upgradeArgs are the flags onboarding passed to render-k8s, minus the ones that only matter to a
// render (--out, --image), plus the upgrade's own.
func (c *upCluster) upgradeArgs(extra ...string) []string {
	return append([]string{"upgrade", "--instance", upInst, "--config", c.cfg, "--sut-namespace", "sut",
		"--tier", "k3d", "--replicas", "1", "--podmonitor", "off"}, extra...)
}

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { code = dispatch(args) })
	})
	return
}

// newerKit changes what the "newer kit" renders: a pull secret (imagePullSecrets + the registries
// annotation) and a Grafana link env on the Deployment, and a new config on the ConfigMap.
func newerKit(t *testing.T, c *upCluster) {
	t.Helper()
	t.Setenv("ARGUS_IMAGE_PULL_SECRET", "ghcr-pull")
	t.Setenv("ARGUS_GRAFANA_PUBLIC_URL", "https://grafana.example.invalid")
	if err := os.WriteFile(c.cfg, []byte(upConfig+"# newer kit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.addSecret(t, "ghcr-pull") // the user created the pull Secret the newer kit's Deployment names
}

// addSecret puts an (empty) Secret marker in the fake cluster.
func (c *upCluster) addSecret(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, "live", "secret__"+name+".json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func onlyGets(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		f := strings.Fields(call)
		for len(f) >= 2 && (f[0] == "--kubeconfig" || f[0] == "--context") {
			f = f[2:]
		}
		readOnly := len(f) > 0 && (f[0] == "get" || (len(f) >= 3 && f[0] == "config" && f[1] == "view"))
		if !readOnly {
			t.Errorf("a dry run made a non-read kubectl call: %q", call)
		}
	}
}

// ---- tests -----------------------------------------------------------------------------------------

func TestUpgrade_DryRunIsTheDefaultAndWritesNothing(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "no changes were made") {
		t.Errorf("the output must say it is a dry run and that nothing was written:\n%s", out)
	}
	if !strings.Contains(out, "--apply") {
		t.Errorf("the output must name the flag that applies:\n%s", out)
	}
	calls := c.calls(t)
	if len(calls) == 0 {
		t.Fatal("a dry run still READS the cluster; no kubectl call recorded")
	}
	onlyGets(t, calls)
	if c.patches() != "" {
		t.Errorf("patches recorded in a dry run:\n%s", c.patches())
	}
}

func TestUpgrade_ShowsTheDifferencePerObject(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	out, _, _ := run(t, c.upgradeArgs()...)
	for _, want := range []string{
		"ConfigMap/argus-config", "# newer kit",
		"Deployment/executor", "ARGUS_GRAFANA_PUBLIC_URL", "https://grafana.example.invalid",
		"argus.onedroid.ai/pull-secret-registries", "imagePullSecrets",
		"Service/executor", "unchanged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ResourceQuota/instance-quota  DIFFERS") || strings.Contains(out, "instance-quota: differs") {
		t.Errorf("the quota only differs by canonical form (9216Mi vs 9Gi); it must read unchanged:\n%s", out)
	}
}

func TestUpgrade_ApplyPatchesOnlyTheObjectsThatDiffer(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var patched []string
	for _, call := range c.calls(t) {
		if verbOf(call) == "patch" {
			patched = append(patched, call)
		}
		for _, verb := range []string{"apply", "create", "replace", "delete", "edit"} {
			if verbOf(call) == verb {
				t.Errorf("unexpected write verb: %q", call)
			}
		}
	}
	if len(patched) != 2 {
		t.Fatalf("want exactly 2 patches (argus-config ConfigMap, executor Deployment), got %d: %v", len(patched), patched)
	}
	joined := strings.Join(patched, "\n")
	if !strings.Contains(joined, "patch configmap argus-config -n "+upNS) ||
		!strings.Contains(joined, "patch deployment.apps executor -n "+upNS) {
		t.Errorf("patched the wrong objects:\n%s", joined)
	}
	if !strings.Contains(joined, "--type=strategic") || !strings.Contains(joined, "--patch-file=/dev/stdin") {
		t.Errorf("patches must be strategic merge patches with the body on stdin:\n%s", joined)
	}
	for _, want := range []string{"patched", "ConfigMap/argus-config", "Deployment/executor", "left alone", "Service/executor"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report must name what it changed and what it left alone (%q):\n%s", want, out)
		}
	}
	// What the cluster now holds.
	dep := c.live(t, "deployment.apps", "executor")
	if !strings.Contains(toJSON(dep), "https://grafana.example.invalid") || !strings.Contains(toJSON(dep), "pull-secret-registries") {
		t.Errorf("the Deployment was not updated")
	}
}

func TestUpgrade_SecondRunAfterApplyReportsNoDifferences(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	if _, _, code := run(t, c.upgradeArgs("--apply")...); code != exitOK {
		t.Fatalf("first apply exit %d", code)
	}
	c.resetCalls()
	_ = os.Remove(filepath.Join(c.dir, "patches.txt"))
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitOK {
		t.Fatalf("second run exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "no differences") {
		t.Errorf("second run must report no differences:\n%s", out)
	}
	if c.patches() != "" {
		t.Errorf("second run patched something:\n%s", c.patches())
	}
	for _, call := range c.calls(t) {
		if verbOf(call) == "patch" {
			t.Errorf("second run called patch: %q", call)
		}
	}
}

func TestUpgrade_NeverTouchesAnySecret(t *testing.T) {
	c := newCluster(t, map[string]string{"ARGUS_IDENTITY_KEY_B64": upIdentOldB64})
	t.Setenv("ARGUS_IDENTITY_KEY_B64", upIdentNewB64)
	newerKit(t, c)
	for _, mode := range [][]string{nil, {"--apply"}} {
		c.resetCalls()
		out, errOut, _ := run(t, c.upgradeArgs(mode...)...)
		for _, call := range c.calls(t) {
			l := strings.ToLower(call)
			if strings.Contains(l, "secret") && !secretExistsCall.MatchString(call) {
				t.Errorf("kubectl was asked about a Secret other than by existence (`get secret <name> -n <ns> -o name`): %q", call)
			}
			if verbOf(call) == "apply" {
				t.Errorf("kubectl apply: %q", call)
			}
		}
		if !strings.Contains(out, "Secret/exec-tokens") || !strings.Contains(out, "Secret/exec-identity") ||
			!strings.Contains(strings.ToLower(out), "skipped") {
			t.Errorf("the output must say the Secrets are skipped:\n%s", out)
		}
		for _, leak := range []string{upRunnerTok, upAuthorTok, upEnrollTok, upIdentNewB64, upIdentOldB64} {
			if strings.Contains(out+errOut+c.patches(), leak) {
				t.Errorf("secret value %q reached the output or a patch", leak)
			}
		}
	}
}

func TestUpgrade_IdentityMountIsNotTornDownByAMissingKeyInTheEnvironment(t *testing.T) {
	c := newCluster(t, map[string]string{"ARGUS_IDENTITY_KEY_B64": upIdentOldB64})
	// The upgrade run has NO ARGUS_IDENTITY_KEY_B64. A naive render would point ARGUS_IDENTITY_PATH back at
	// the results volume, and the pods would mint a NEW machine identity.
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK || !strings.Contains(out, "no differences") {
		t.Fatalf("exit %d; an unchanged instance must show no differences:\n%s", code, out)
	}
	if strings.Contains(out, "ARGUS_IDENTITY_PATH") || strings.Contains(out, "/results/identity.key") {
		t.Errorf("identity wiring must not differ:\n%s", out)
	}
}

func TestUpgrade_LeavesTheExecutorImageAndReplicasAlone(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	out, _, code := run(t, c.upgradeArgs("--image", "registry.invalid/argus:newer", "--apply")...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "registry.invalid/argus:newer") || !strings.Contains(strings.ToLower(out), "image") ||
		!strings.Contains(out, "left alone") {
		t.Errorf("the output must say the render's image differs and was left alone:\n%s", out)
	}
	if strings.Contains(c.patches(), "argus:newer") {
		t.Errorf("the patch carries the render's image:\n%s", c.patches())
	}
	dep := c.live(t, "deployment.apps", "executor")
	if !strings.Contains(toJSON(dep), upOldDigest) || strings.Contains(toJSON(dep), "argus:newer") {
		t.Errorf("the running image changed")
	}
	if r := dep["spec"].(map[string]any)["replicas"]; r != float64(3) {
		t.Errorf("replicas = %v; the executor's own scale (3) must survive an upgrade", r)
	}
}

func TestUpgrade_RefusesWhenNamespaceOrExecutorIsMissing(t *testing.T) {
	t.Run("namespace", func(t *testing.T) {
		c := newCluster(t, nil)
		newerKit(t, c)
		c.remove("namespace", upNS)
		out, _, code := run(t, c.upgradeArgs("--apply")...)
		if code != exitDenied || !strings.Contains(out, upNS) || !strings.Contains(out, "not found") {
			t.Fatalf("exit %d, want %d with a one-line reason naming the namespace:\n%s", code, exitDenied, out)
		}
		if c.patches() != "" {
			t.Error("a refusal must change nothing")
		}
	})
	t.Run("executor", func(t *testing.T) {
		c := newCluster(t, nil)
		newerKit(t, c)
		c.remove("deployment.apps", "executor")
		out, _, code := run(t, c.upgradeArgs("--apply")...)
		if code != exitDenied || !strings.Contains(out, "executor") || !strings.Contains(out, "not found") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if c.patches() != "" {
			t.Error("a refusal must change nothing")
		}
	})
}

func TestUpgrade_RefusesWhenTheClusterHoldsAnotherInstance(t *testing.T) {
	t.Run("namespace label", func(t *testing.T) {
		c := newCluster(t, nil)
		newerKit(t, c)
		c.setLive(t, "namespace", upNS, func(m map[string]any) {
			m["metadata"].(map[string]any)["labels"].(map[string]any)["argus.onedroid.ai/instance"] = "someone-else"
		})
		out, _, code := run(t, c.upgradeArgs("--apply")...)
		if code != exitDenied || !strings.Contains(out, "someone-else") || !strings.Contains(out, upInst) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if c.patches() != "" {
			t.Error("a refusal must change nothing")
		}
	})
	t.Run("executor env", func(t *testing.T) {
		c := newCluster(t, nil)
		newerKit(t, c)
		c.setLive(t, "deployment.apps", "executor", func(m map[string]any) {
			cs := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			for _, e := range cs[0].(map[string]any)["env"].([]any) {
				if e.(map[string]any)["name"] == "ARGUS_INSTANCE_ID" {
					e.(map[string]any)["value"] = "someone-else"
				}
			}
		})
		out, _, code := run(t, c.upgradeArgs("--apply")...)
		if code != exitDenied || !strings.Contains(out, "someone-else") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if c.patches() != "" {
			t.Error("a refusal must change nothing")
		}
	})
}

func TestUpgrade_RefusesWhenTheRenderFails(t *testing.T) {
	c := newCluster(t, nil)
	out, _, code := run(t, append(c.upgradeArgs("--apply"), "--tier", "no-such-tier")...)
	if code != exitDenied {
		t.Fatalf("exit %d, want %d:\n%s", code, exitDenied, out)
	}
	if c.patches() != "" {
		t.Error("a refusal must change nothing")
	}
	onlyGets(t, c.calls(t))

	// And a config that does not load (same refusal, different render failure).
	if err := os.WriteFile(c.cfg, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.resetCalls()
	_, _, code = run(t, c.upgradeArgs("--apply")...)
	if code != exitDenied || c.patches() != "" {
		t.Fatalf("a config that fails to load must refuse (exit %d) and change nothing", code)
	}
}

func TestUpgrade_UsageErrors(t *testing.T) {
	c := newCluster(t, nil)
	if _, _, code := run(t, "upgrade", "--config", c.cfg); code != exitUsage {
		t.Errorf("no --instance: exit %d, want %d", code, exitUsage)
	}
	if _, _, code := run(t, "upgrade", "--instance", upInst); code != exitUsage {
		t.Errorf("no --config: exit %d, want %d", code, exitUsage)
	}
	if _, _, code := run(t, "upgrade", "--instance", upInst, "--instance-id", "other", "--config", c.cfg); code != exitUsage {
		t.Errorf("--instance and --instance-id disagree: exit %d, want %d", code, exitUsage)
	}
	if _, _, code := run(t, "upgrade", "--instance", upInst, "--config", c.cfg, "--no-such-flag"); code != exitUsage {
		t.Errorf("unknown flag: exit %d, want %d", code, exitUsage)
	}
}

func TestUpgrade_KubeconfigAndContextReachEveryKubectlCall(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	// The last two runs use `--podmonitor auto`, which probes the cluster for the PodMonitor CRD: that
	// probe is a kubectl call like any other and must carry both flags, in front of the verb.
	for _, mode := range [][]string{nil, {"--apply"}, {"--podmonitor", "auto"}, {"--apply", "--podmonitor", "auto"}} {
		c.resetCalls()
		if _, _, code := run(t, c.upgradeArgs(append(mode, "--kubeconfig", kubeconfigPath, "--kube-context", "in-cluster")...)...); code != exitOK {
			t.Fatalf("exit %d", code)
		}
		calls := c.calls(t)
		if len(calls) < 3 {
			t.Fatalf("too few kubectl calls: %v", calls)
		}
		for _, call := range calls {
			if !strings.HasPrefix(call, "--kubeconfig "+kubeconfigPath+" --context in-cluster ") {
				t.Errorf("kubectl call without --kubeconfig/--context: %q", call)
			}
		}
		probe := "--kubeconfig " + kubeconfigPath + " --context in-cluster get crd " + podMonitorCRDName + " -o name"
		probed := false
		for _, call := range calls {
			probed = probed || call == probe
		}
		if want := len(mode) > 0 && mode[len(mode)-1] == "auto"; probed != want {
			t.Errorf("mode %v: PodMonitor CRD probe made = %v, want %v; calls %v", mode, probed, want, calls)
		}
	}
}

// secretExistsCall is the ONE kubectl call upgrade may make that names a Secret: existence by name.
var secretExistsCall = regexp.MustCompile(`^(--kubeconfig \S+ )?(--context \S+ )?get secret \S+ -n \S+ -o name$`)

func TestUpgrade_ImmutableDifferenceIsLeftAloneAndReported(t *testing.T) {
	c := newCluster(t, nil)
	// A tester who upgrades with another storage class than the instance was onboarded with.
	out, _, code := run(t, c.upgradeArgs("--apply", "--storage-class", "other-class")...)
	if !strings.Contains(out, "PersistentVolumeClaim/exec-results") || !strings.Contains(out, "immutable") {
		t.Errorf("the PVC difference must be reported as immutable:\n%s", out)
	}
	if code != exitFailed {
		t.Errorf("exit %d: an apply that left a difference it cannot make must not exit 0 (want %d)", code, exitFailed)
	}
	for _, call := range c.calls(t) {
		if strings.Contains(call, "patch persistentvolumeclaim") {
			t.Errorf("patched an immutable PVC: %q", call)
		}
	}
}

func TestUpgradeUsage_NamesTheFlagsAndTheGuarantees(t *testing.T) {
	rc, out := runHelp(t, "upgrade", "--help")
	if rc != exitOK {
		t.Fatalf("exit %d", rc)
	}
	for _, want := range []string{"--instance", "--apply", "--kubeconfig", "dry run", "exec-tokens", "image"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("upgrade --help lacks %q:\n%s", want, out)
		}
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// verbOf is the kubectl verb of a recorded call, after the global --kubeconfig/--context flags.
func verbOf(call string) string {
	f := strings.Fields(call)
	for len(f) >= 2 && (f[0] == "--kubeconfig" || f[0] == "--context") {
		f = f[2:]
	}
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
