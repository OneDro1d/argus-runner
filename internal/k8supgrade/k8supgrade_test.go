package k8supgrade

import (
	"encoding/json"
	"strings"
	"testing"
)

const twoDocs = `# leading comment
apiVersion: v1
kind: ConfigMap
metadata: {name: argus-config, namespace: argus-inst-x}
data:
  a: "1"
---
apiVersion: v1
kind: Secret
metadata: {name: exec-tokens, namespace: argus-inst-x}
stringData: {ARGUS_RUNNER_TOKEN: "fake-not-real"}
---
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata: {name: pg, namespace: argus-inst-x}
spec: {}
`

func mustParse(t *testing.T, y string) []Object {
	t.Helper()
	objs, err := Parse(y)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return objs
}

func obj(t *testing.T, y string) Object {
	t.Helper()
	o := mustParse(t, y)
	if len(o) != 1 {
		t.Fatalf("want 1 object, got %d", len(o))
	}
	return o[0]
}

func live(t *testing.T, j string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParse_SplitsDocumentsAndNamesObjects(t *testing.T) {
	objs := mustParse(t, twoDocs)
	if len(objs) != 3 {
		t.Fatalf("want 3 objects (empty documents skipped), got %d", len(objs))
	}
	if objs[0].Ref() != "ConfigMap/argus-config" || objs[0].Namespace != "argus-inst-x" {
		t.Errorf("object 0 = %s ns=%s", objs[0].Ref(), objs[0].Namespace)
	}
	if !objs[1].IsSecret() || objs[0].IsSecret() {
		t.Errorf("IsSecret wrong: secret=%v configmap=%v", objs[1].IsSecret(), objs[0].IsSecret())
	}
	for i, want := range []string{"configmap", "secret", "podmonitor.monitoring.coreos.com"} {
		if got := objs[i].Resource(); got != want {
			t.Errorf("Resource(%d) = %q, want %q", i, got, want)
		}
	}
	if got := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\n").Resource(); got != "deployment.apps" {
		t.Errorf("Resource = %q", got)
	}
}

func TestDiff_SubsetIgnoresLiveOnlyFields(t *testing.T) {
	o := obj(t, "apiVersion: v1\nkind: Service\nmetadata: {name: s, labels: {a: b}}\nspec: {type: ClusterIP}\n")
	l := live(t, `{"metadata":{"name":"s","uid":"u","labels":{"a":"b","extra":"x"}},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.1"},"status":{}}`)
	if cs := Diff(o, Prepare(o), l); len(cs) != 0 {
		t.Fatalf("live-only fields must not be differences: %+v", cs)
	}
}

func TestDiff_ReportsChangedAndMissing(t *testing.T) {
	o := obj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, annotations: {k: v}}\ndata: {a: new, b: same}\n")
	l := live(t, `{"metadata":{"name":"c"},"data":{"a":"old","b":"same"}}`)
	cs := Diff(o, Prepare(o), l)
	if len(cs) != 2 {
		t.Fatalf("want 2 changes, got %+v", cs)
	}
	var sawChanged, sawMissing bool
	for _, c := range cs {
		if strings.HasSuffix(c.Path, "data.a") && !c.Missing && c.Live == "old" && c.Desired == "new" {
			sawChanged = true
		}
		if strings.Contains(c.Path, "annotations") && c.Missing {
			sawMissing = true
		}
	}
	if !sawChanged || !sawMissing {
		t.Errorf("changes = %+v", cs)
	}
}

func TestDiff_ListsMatchByName(t *testing.T) {
	o := obj(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: executor}
spec:
  template:
    spec:
      containers:
        - name: executor
          env:
            - {name: A, value: "1"}
            - {name: B, value: "2"}
`)
	l := live(t, `{"spec":{"template":{"spec":{"containers":[{"name":"sidecar"},{"name":"executor","env":[{"name":"B","value":"2"},{"name":"Z","value":"9"},{"name":"A","value":"0"}]}]}}}}`)
	cs := Diff(o, Prepare(o), l)
	if len(cs) != 1 || !strings.Contains(cs[0].Path, "containers[executor]") || !strings.Contains(cs[0].Path, "env[A]") {
		t.Fatalf("want exactly the env A change, addressed by name: %+v", cs)
	}
}

func TestDiff_QuantitiesCompareByValue(t *testing.T) {
	o := obj(t, "apiVersion: v1\nkind: ResourceQuota\nmetadata: {name: q}\nspec:\n  hard: {limits.memory: \"9216Mi\", limits.cpu: \"1000m\", pods: \"14\"}\n")
	l := live(t, `{"spec":{"hard":{"limits.memory":"9Gi","limits.cpu":"1","pods":"14"}}}`)
	if cs := Diff(o, Prepare(o), l); len(cs) != 0 {
		t.Fatalf("9216Mi == 9Gi and 1000m == 1: %+v", cs)
	}
	l2 := live(t, `{"spec":{"hard":{"limits.memory":"8Gi","limits.cpu":"1","pods":"14"}}}`)
	if cs := Diff(o, Prepare(o), l2); len(cs) != 1 {
		t.Fatalf("a real quantity change must show: %+v", cs)
	}
}

func TestDiff_EmptyDesiredEqualsAbsentLive(t *testing.T) {
	o := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec:\n  template:\n    spec:\n      containers:\n        - name: c\n          env: [{name: ARGUS_WORKSPACE_ID, value: \"\"}]\n")
	l := live(t, `{"spec":{"template":{"spec":{"containers":[{"name":"c","env":[{"name":"ARGUS_WORKSPACE_ID"}]}]}}}}`)
	if cs := Diff(o, Prepare(o), l); len(cs) != 0 {
		t.Fatalf("value \"\" renders as an absent value on the server: %+v", cs)
	}
}

func TestDiff_ImmutableFieldsAreFlagged(t *testing.T) {
	o := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec:\n  selector: {matchLabels: {a: new}}\n")
	l := live(t, `{"spec":{"selector":{"matchLabels":{"a":"old"}}}}`)
	cs := Diff(o, Prepare(o), l)
	if len(cs) != 1 || !cs[0].Immutable {
		t.Fatalf("selector change must be flagged immutable: %+v", cs)
	}
	p := obj(t, "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: p}\nspec:\n  storageClassName: new\n  resources: {requests: {storage: 3Gi}}\n")
	lp := live(t, `{"spec":{"storageClassName":"old","resources":{"requests":{"storage":"2Gi"}}}}`)
	cs = Diff(p, Prepare(p), lp)
	var imm, mut int
	for _, c := range cs {
		if c.Immutable {
			imm++
		} else {
			mut++
		}
	}
	if imm != 1 || mut != 1 {
		t.Fatalf("storageClassName immutable, storage request mutable: %+v", cs)
	}
}

func TestPrepare_DropsRuntimeOwnedReplicasFromExecutorOnly(t *testing.T) {
	ex := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: executor}\nspec: {replicas: 1}\n")
	spec := Prepare(ex)["spec"].(map[string]any)
	if _, has := spec["replicas"]; has {
		t.Error("executor spec.replicas is owned by the executor's own autoscaler and must not be compared or patched")
	}
	lk := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: loki}\nspec: {replicas: 1}\n")
	if _, has := Prepare(lk)["spec"].(map[string]any)["replicas"]; !has {
		t.Error("only the executor Deployment is exempt")
	}
	// Prepare must not mutate the parsed object.
	if _, has := ex.Doc["spec"].(map[string]any)["replicas"]; !has {
		t.Error("Prepare mutated the source object")
	}
}

func TestFormat_MasksSensitiveEnvValuesAndShowsSecretRefsByNameOnly(t *testing.T) {
	o := obj(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  template:
    spec:
      containers:
        - name: c
          env:
            - {name: ARGUS_API_TOKEN, value: "fake-new-value"}
            - {name: PLAIN, value: "visible-new"}
            - name: FROMSECRET
              valueFrom: {secretKeyRef: {name: exec-tokens, key: K}}
`)
	l := live(t, `{"spec":{"template":{"spec":{"containers":[{"name":"c","env":[{"name":"ARGUS_API_TOKEN","value":"fake-old-value"},{"name":"PLAIN","value":"visible-old"}]}]}}}}`)
	out := strings.Join(FormatChanges(Diff(o, Prepare(o), l)), "\n")
	for _, bad := range []string{"fake-new-value", "fake-old-value"} {
		if strings.Contains(out, bad) {
			t.Errorf("a sensitive env value leaked into the diff:\n%s", out)
		}
	}
	if !strings.Contains(out, "visible-new") || !strings.Contains(out, "visible-old") {
		t.Errorf("a plain env value should be shown:\n%s", out)
	}
	if !strings.Contains(out, "exec-tokens") || !strings.Contains(out, "FROMSECRET") {
		t.Errorf("a secretKeyRef is shown by Secret NAME and key:\n%s", out)
	}
}

func TestFormat_MultilineStringsShowAsALineDiff(t *testing.T) {
	o := obj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  f: |\n    one\n    two-new\n    three\n")
	l := live(t, `{"data":{"f":"one\ntwo-old\nthree\n"}}`)
	out := strings.Join(FormatChanges(Diff(o, Prepare(o), l)), "\n")
	if !strings.Contains(out, "- two-old") || !strings.Contains(out, "+ two-new") {
		t.Errorf("want a line diff:\n%s", out)
	}
	if strings.Contains(out, "- one") || strings.Contains(out, "+ one") {
		t.Errorf("unchanged lines must not be shown as changed:\n%s", out)
	}
}

func TestPatch_StrategicForBuiltInMergeForCustomResources(t *testing.T) {
	d := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d, namespace: n}\nspec: {revisionHistoryLimit: 5}\n")
	typ, body, err := Patch(d, Prepare(d))
	if err != nil || typ != "strategic" || !strings.Contains(string(body), "revisionHistoryLimit") {
		t.Fatalf("Deployment: type=%q err=%v body=%s", typ, err, body)
	}
	pm := obj(t, "apiVersion: monitoring.coreos.com/v1\nkind: PodMonitor\nmetadata: {name: p, namespace: n}\nspec: {}\n")
	typ, _, err = Patch(pm, Prepare(pm))
	if err != nil || typ != "merge" {
		t.Fatalf("PodMonitor: type=%q err=%v", typ, err)
	}
}

func TestPatch_RetainKeysOnVolumesAndStrategy(t *testing.T) {
	d := obj(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, namespace: n}
spec:
  strategy: {type: RollingUpdate, rollingUpdate: {maxSurge: 0, maxUnavailable: 1}}
  template:
    spec:
      volumes:
        - name: config
          projected: {sources: []}
`)
	_, body, err := Patch(d, Prepare(d))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	spec := m["spec"].(map[string]any)
	vol := spec["template"].(map[string]any)["spec"].(map[string]any)["volumes"].([]any)[0].(map[string]any)
	rk, _ := vol["$retainKeys"].([]any)
	if len(rk) != 2 {
		t.Errorf("a volume whose source type changes needs $retainKeys [name projected]: %v", vol)
	}
	if _, ok := spec["strategy"].(map[string]any)["$retainKeys"]; !ok {
		t.Errorf("strategy needs $retainKeys: %v", spec["strategy"])
	}
}
