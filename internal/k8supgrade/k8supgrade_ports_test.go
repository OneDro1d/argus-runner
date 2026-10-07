package k8supgrade

import (
	"encoding/json"
	"strings"
	"testing"
)

func svcObject(t *testing.T, y string) Object {
	t.Helper()
	objs, err := Parse(y)
	if err != nil || len(objs) != 1 {
		t.Fatalf("parse: %v", err)
	}
	return objs[0]
}

func liveDoc(t *testing.T, j string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestServicePortsPatch_KeepsTheLiveNodePortByNameThenByNumber(t *testing.T) {
	o := svcObject(t, `
apiVersion: v1
kind: Service
metadata: {name: s, namespace: n}
spec:
  type: NodePort
  ports:
    - {name: http, port: 3100, targetPort: http}
    - {name: metrics, port: 9100, targetPort: metrics}
    - {port: 7000}
`)
	live := liveDoc(t, `{"spec":{"type":"NodePort","ports":[
		{"name":"http","port":9999,"nodePort":31000},
		{"name":"other","port":9100,"nodePort":31001},
		{"port":7000,"nodePort":31002}]}}`)
	if !PortsNeedAWholeListPatch(o, Diff(o, Prepare(o), live)) {
		t.Fatal("a Service whose ports differ must be patched as a whole list")
	}
	typ, body, err := ServicePortsPatch(o, Prepare(o), live)
	if err != nil || typ != "merge" {
		t.Fatalf("type %q err %v", typ, err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	ports := got["spec"].(map[string]any)["ports"].([]any)
	want := []float64{31000, 31001, 31002} // by name, by number (name differs), by number (no name)
	for i, p := range ports {
		if p.(map[string]any)["nodePort"] != want[i] {
			t.Errorf("port %d: nodePort %v, want %v (%s)", i, p.(map[string]any)["nodePort"], want[i], body)
		}
	}
}

func TestServicePortsPatch_ClusterIPCarriesNoNodePortAndOtherKindsAreNotWholeList(t *testing.T) {
	o := svcObject(t, "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec:\n  type: ClusterIP\n  ports: [{name: a, port: 1}]\n")
	live := liveDoc(t, `{"spec":{"type":"ClusterIP","ports":[{"name":"a","port":2}]}}`)
	_, body, _ := ServicePortsPatch(o, Prepare(o), live)
	if strings.Contains(string(body), "nodePort") {
		t.Errorf("a ClusterIP Service has no nodePort to carry: %s", body)
	}
	dep := svcObject(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {}\n")
	if PortsNeedAWholeListPatch(dep, []Change{{Path: "spec.ports[1]"}}) {
		t.Error("only a Service is patched as a whole list")
	}
}

func TestFormatChanges_ACredentialOnlyChangeAmongOtherLinesIsNamedNotSilent(t *testing.T) {
	out := strings.Join(FormatChanges([]Change{{
		Path:    "data.cfg",
		Live:    "url: https://a1:b1@h/x\nkeep: 1\nold: 1\n",
		Desired: "url: https://a2:b2@h/x\nkeep: 1\nnew: 2\n",
	}}), "\n")
	if !strings.Contains(out, "- old: 1") || !strings.Contains(out, "+ new: 2") || !strings.Contains(out, hiddenCredentialNote) {
		t.Errorf("want the visible change and the note:\n%s", out)
	}
	for _, leak := range []string{"a1", "b1", "a2", "b2"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked:\n%s", leak, out)
		}
	}
}
