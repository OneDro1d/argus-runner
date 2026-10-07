package k8supgrade

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// the same entries in a different order are "unchanged" to Diff, but the API server takes
// the patch's order for a merged list, so a patch built from the whole prepared document re-orders the pod
// template and rolls the Deployment. These tests apply the patch to the live object with a small strategic
// merge of their own (testMerge) and compare the result with live, byte for byte.

const reorderDesired = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: executor
  namespace: n
  annotations: {argus.onedroid.ai/pull-secret-registries: ghcr.io}
spec:
  template:
    spec:
      imagePullSecrets: [{name: pull-a}, {name: pull-b}]
      containers:
        - name: executor
          env:
            - {name: A, value: "1"}
            - {name: B, value: "2"}
            - {name: C, value: "3"}
          ports: [{containerPort: 1}, {containerPort: 2}]
          volumeMounts:
            - {name: v1, mountPath: /one}
            - {name: v2, mountPath: /two}
        - name: sidecar
          image: side:1
      volumes:
        - {name: v1, emptyDir: {}}
        - {name: v2, emptyDir: {}}
`

func reorderObj(t *testing.T) Object { return obj(t, reorderDesired) }

// podSpec and executorContainer reach into a Deployment document; reverse flips a list in place.
func podSpec(d map[string]any) map[string]any {
	return d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func executorContainer(d map[string]any) map[string]any {
	for _, c := range podSpec(d)["containers"].([]any) {
		if cm := c.(map[string]any); cm["name"] == "executor" {
			return cm
		}
	}
	return nil
}

func reverse(l []any) {
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
	}
}

// liveReordered is what the cluster holds: the rendered object with ONE list reversed and no
// pull-secret-registries annotation (the one real difference).
func liveReordered(t *testing.T, o Object, list string) map[string]any {
	t.Helper()
	l := deepCopy(Prepare(o)).(map[string]any)
	delete(l["metadata"].(map[string]any), "annotations")
	switch list {
	case "env", "ports", "volumeMounts":
		reverse(executorContainer(l)[list].([]any))
	case "containers", "volumes", "imagePullSecrets":
		reverse(podSpec(l)[list].([]any))
	case "all":
		for _, k := range []string{"env", "ports", "volumeMounts"} {
			reverse(executorContainer(l)[k].([]any))
		}
		for _, k := range []string{"containers", "volumes", "imagePullSecrets"} {
			reverse(podSpec(l)[k].([]any))
		}
	default:
		t.Fatalf("unknown list %q", list)
	}
	return l
}

// testMerge is a strategic merge patch of its own: lists of maps merge by key (mountPath, containerPort,
// else name); the patch's order wins for entries it carries, and an entry only live holds stays right
// after the entry that preceded it in live. Every other list is replaced. $retainKeys is ignored.
func testMerge(live, patch map[string]any, parent string) map[string]any {
	out := deepCopy(live).(map[string]any)
	for k, pv := range patch {
		if k == "$retainKeys" {
			continue
		}
		lv, ok := out[k]
		if !ok {
			out[k] = stripRetain(pv)
			continue
		}
		switch p := pv.(type) {
		case map[string]any:
			if lm, ok := lv.(map[string]any); ok {
				out[k] = testMerge(lm, p, k)
			} else {
				out[k] = stripRetain(pv)
			}
		case []any:
			ll, _ := lv.([]any)
			key := testKey(k, p)
			if key == "" || ll == nil {
				out[k] = stripRetain(pv)
				continue
			}
			out[k] = testMergeList(ll, p, key, k)
		default:
			out[k] = pv
		}
	}
	return out
}

func testKey(field string, p []any) string {
	if len(p) == 0 {
		return ""
	}
	if _, ok := p[0].(map[string]any); !ok {
		return ""
	}
	switch field {
	case "volumeMounts":
		return "mountPath"
	case "ports":
		return "containerPort"
	}
	return "name"
}

func testMergeList(ll, p []any, key, field string) []any {
	byKey := func(l []any, v any) map[string]any {
		for _, e := range l {
			if m := e.(map[string]any); fmt.Sprint(m[key]) == fmt.Sprint(v) {
				return m
			}
		}
		return nil
	}
	var res []any
	for _, pe := range p {
		pm := pe.(map[string]any)
		if lm := byKey(ll, pm[key]); lm != nil {
			res = append(res, testMerge(lm, pm, field))
		} else {
			res = append(res, stripRetain(pe))
		}
	}
	for i, le := range ll { // entries only live holds
		lm := le.(map[string]any)
		if byKey(p, lm[key]) != nil {
			continue
		}
		pos := 0
		if i > 0 {
			prev := ll[i-1].(map[string]any)[key]
			for j, r := range res {
				if fmt.Sprint(r.(map[string]any)[key]) == fmt.Sprint(prev) {
					pos = j + 1
				}
			}
		}
		res = append(res, nil)
		copy(res[pos+1:], res[pos:])
		res[pos] = deepCopy(le)
	}
	return res
}

func stripRetain(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := map[string]any{}
		for k, x := range t {
			if k != "$retainKeys" {
				m[k] = stripRetain(x)
			}
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = stripRetain(x)
		}
		return s
	}
	return v
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func applyPatch(t *testing.T, o Object, live map[string]any) map[string]any {
	t.Helper()
	_, body, err := PatchFor(o, Prepare(o), live)
	if err != nil {
		t.Fatal(err)
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatal(err)
	}
	return testMerge(live, patch, "")
}

func envNames(d map[string]any) []string {
	var out []string
	for _, e := range executorContainer(d)["env"].([]any) {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	return out
}

// (a) and (b): only the annotation differs; one list at a time, and all together with a hand-added env.
func TestPatchFor_AnnotationOnlyDifferenceLeavesThePodTemplateByteIdentical(t *testing.T) {
	for _, list := range []string{"env", "containers", "volumes", "volumeMounts", "ports", "imagePullSecrets", "all"} {
		t.Run(list, func(t *testing.T) {
			o := reorderObj(t)
			live := liveReordered(t, o, list)
			if list == "all" { // a variable someone added by hand, in the middle
				c := executorContainer(live)
				env := c["env"].([]any)
				c["env"] = []any{env[0], map[string]any{"name": "HAND", "value": "x"}, env[1], env[2]}
			}
			cs := Diff(o, Prepare(o), live)
			if len(cs) != 1 || cs[0].Path != "metadata.annotations" {
				t.Fatalf("the only difference is the annotation, got %+v", cs)
			}
			if note := RestartNote(o, cs, false); note != "" {
				t.Errorf("an annotation-only change must not announce a restart: %q", note)
			}
			got := applyPatch(t, o, live)
			if g, w := jsonOf(t, got["spec"].(map[string]any)["template"]), jsonOf(t, live["spec"].(map[string]any)["template"]); g != w {
				t.Errorf("the patch changed the pod template.\n got %s\nwant %s", g, w)
			}
			if !strings.Contains(jsonOf(t, got["metadata"]), "pull-secret-registries") {
				t.Errorf("the annotation must still be applied: %s", jsonOf(t, got["metadata"]))
			}
			// structurally: the patch itself carries the live order
			_, body, _ := PatchFor(o, Prepare(o), live)
			var patch map[string]any
			_ = json.Unmarshal(body, &patch)
			if list == "env" || list == "all" {
				want := envNames(live)
				var have []string
				for _, e := range executorContainer(patch)["env"].([]any) {
					have = append(have, e.(map[string]any)["name"].(string))
				}
				// the patch has no HAND (desired lacks it): it must be the live order minus HAND
				var wantNoHand []string
				for _, n := range want {
					if n != "HAND" {
						wantNoHand = append(wantNoHand, n)
					}
				}
				if !reflect.DeepEqual(have, wantNoHand) {
					t.Errorf("patch env order %v, want the live order %v", have, wantNoHand)
				}
			}
		})
	}
}

// the defect as reported: the old patch (the whole prepared document) does re-order the template.
func TestPatch_WithoutLiveReordersTheTemplate_TheDefect(t *testing.T) {
	o := reorderObj(t)
	live := liveReordered(t, o, "env")
	_, body, _ := Patch(o, Prepare(o))
	var patch map[string]any
	_ = json.Unmarshal(body, &patch)
	got := testMerge(live, patch, "")
	if jsonOf(t, got["spec"].(map[string]any)["template"]) == jsonOf(t, live["spec"].(map[string]any)["template"]) {
		t.Fatal("the fixture no longer demonstrates the defect: Patch without live kept the order")
	}
}

// (c): a real change inside the template is announced and names what changes; unchanged entries keep live order.
func TestPatchFor_ARealTemplateChangeIsAnnouncedAndKeepsTheLiveOrder(t *testing.T) {
	o := reorderObj(t)
	live := liveReordered(t, o, "all")
	c := executorContainer(live)
	for _, e := range c["env"].([]any) {
		if m := e.(map[string]any); m["name"] == "B" {
			m["value"] = "old"
		}
	}
	// desired also adds a new variable D between B and C
	d := deepCopy(o.Doc).(map[string]any)
	dc := executorContainer(d)
	env := dc["env"].([]any)
	dc["env"] = []any{env[0], env[1], map[string]any{"name": "D", "value": "4"}, env[2]}
	o.Doc = d

	cs := Diff(o, Prepare(o), live)
	note := RestartNote(o, cs, false)
	for _, want := range []string{"Deployment/executor", "REPLACE", "env[B].value", "env[D]", "annotations"} {
		if want == "annotations" {
			if strings.Contains(note, want) {
				t.Errorf("a non-template change must not be named in the restart line: %q", note)
			}
			continue
		}
		if !strings.Contains(note, want) {
			t.Errorf("restart line lacks %q: %q", want, note)
		}
	}
	got := applyPatch(t, o, live)
	names := envNames(got)
	var kept []string // the entries live already had, in the order they came out
	for _, n := range names {
		if n != "D" {
			kept = append(kept, n)
		}
	}
	if want := envNames(live); !reflect.DeepEqual(kept, want) {
		t.Errorf("unchanged entries lost the live order: %v, live %v", kept, want)
	}
	if len(names) != len(envNames(live))+1 {
		t.Errorf("D must be added once: %v", names)
	}
	if !strings.Contains(jsonOf(t, got), `"value":"2"`) {
		t.Errorf("B's value must be updated")
	}
	// the other lists keep live order too
	if g, w := jsonOf(t, podSpec(got)["volumes"]), jsonOf(t, podSpec(live)["volumes"]); g != w {
		t.Errorf("volumes changed order: %s vs %s", g, w)
	}
	if g, w := jsonOf(t, podSpec(got)["containers"].([]any)[0].(map[string]any)["name"]), jsonOf(t, podSpec(live)["containers"].([]any)[0].(map[string]any)["name"]); g != w {
		t.Errorf("containers changed order: %s vs %s", g, w)
	}
}

// (d) a change outside the template says nothing about a restart, even on a workload.
func TestRestartNote_NothingForChangesOutsideThePodTemplate(t *testing.T) {
	o := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: executor, namespace: n, annotations: {a: b}}\nspec: {revisionHistoryLimit: 5}\n")
	live := liveDoc(t, `{"metadata":{"name":"executor"},"spec":{"revisionHistoryLimit":10}}`)
	cs := Diff(o, Prepare(o), live)
	if len(cs) != 2 {
		t.Fatalf("want 2 differences, got %+v", cs)
	}
	if note := RestartNote(o, cs, true); note != "" {
		t.Errorf("no restart line for a change outside spec.template: %q", note)
	}
	// and a template change on a non-workload is not a restart (a ConfigMap has no pods)
	cm := obj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {template: x}\n")
	if note := RestartNote(cm, []Change{{Path: "spec.template.x"}}, true); note != "" {
		t.Errorf("only a workload restarts: %q", note)
	}
}

func TestRestartNote_SaysWouldInADryRunAndWillInAnApply(t *testing.T) {
	o := reorderObj(t)
	cs := []Change{{Path: "spec.template.spec.containers[executor].env[X]", Missing: true}}
	if n := RestartNote(o, cs, false); !strings.Contains(n, "would REPLACE") {
		t.Errorf("dry run: %q", n)
	}
	if n := RestartNote(o, cs, true); !strings.Contains(n, "will REPLACE") {
		t.Errorf("apply: %q", n)
	}
}

// a quantity Diff called equal is carried as live holds it, so the patch cannot re-write the template.
func TestPatchFor_QuantityEqualValuesAreCarriedAsLive(t *testing.T) {
	o := obj(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: d, namespace: n, annotations: {a: b}}
spec:
  template:
    spec:
      containers:
        - {name: c, resources: {limits: {cpu: 500m}}}
`)
	live := liveDoc(t, `{"metadata":{"name":"d"},"spec":{"template":{"spec":{"containers":[{"name":"c","resources":{"limits":{"cpu":"0.5"}}}]}}}}`)
	cs := Diff(o, Prepare(o), live)
	if len(cs) != 1 || cs[0].Path != "metadata.annotations" {
		t.Fatalf("got %+v", cs)
	}
	got := applyPatch(t, o, live)
	if g, w := jsonOf(t, got["spec"]), jsonOf(t, live["spec"]); g != w {
		t.Errorf("spec changed: %s vs %s", g, w)
	}
}

// the on-purpose behaviour stays: $retainKeys and the Secret/whole-list paths are untouched by PatchFor.
func TestPatchFor_KeepsRetainKeysAndTheTypeChoice(t *testing.T) {
	o := reorderObj(t)
	live := liveReordered(t, o, "volumes")
	typ, body, err := PatchFor(o, Prepare(o), live)
	if err != nil || typ != "strategic" || !strings.Contains(string(body), "$retainKeys") {
		t.Fatalf("type=%q err=%v body=%s", typ, err, body)
	}
	pm := obj(t, "apiVersion: monitoring.coreos.com/v1\nkind: PodMonitor\nmetadata: {name: p, namespace: n}\nspec: {}\n")
	if typ, _, _ := PatchFor(pm, Prepare(pm), map[string]any{}); typ != "merge" {
		t.Errorf("custom resources keep the merge patch, got %q", typ)
	}
}
