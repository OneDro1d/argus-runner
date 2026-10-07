package k8supgrade

import (
	"encoding/json"
	"strings"
	"testing"
)

const keyedDeploy = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: n}
spec:
  template:
    spec:
      containers:
        - name: web
          ports:
            - {name: http, containerPort: 8080}
          volumeMounts:
            - {name: data, mountPath: /data}
`

func TestDiff_ContainerPortsMatchByContainerPortAndVolumeMountsByMountPath(t *testing.T) {
	o := obj(t, keyedDeploy)
	l := live(t, `{"spec":{"template":{"spec":{"containers":[{"name":"web",
	  "ports":[{"name":"http","containerPort":9090}],
	  "volumeMounts":[{"name":"data","mountPath":"/old"}]}]}}}}`)
	got := strings.Join(FormatChanges(Diff(o, Prepare(o), l)), "\n")
	for _, want := range []string{"containers[web].ports[8080]", "containers[web].volumeMounts[/data]"} {
		if !strings.Contains(got, want) {
			t.Errorf("want a change keyed %q (the server merges on that key), got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ports[http]") || strings.Contains(got, "volumeMounts[data]") {
		t.Errorf("lists must not be keyed by name where the server merges on another field:\n%s", got)
	}
}

func TestDiff_ServicePortsMatchByPort(t *testing.T) {
	o := obj(t, "apiVersion: v1\nkind: Service\nmetadata: {name: s, namespace: n}\nspec:\n  ports:\n    - {name: mcp, port: 8080}\n")
	same := live(t, `{"spec":{"ports":[{"name":"renamed","port":8080,"targetPort":1}]}}`)
	if cs := Diff(o, Prepare(o), same); len(cs) != 1 || cs[0].Path != "spec.ports[8080].name" {
		t.Errorf("same port number = same element; only the name differs: %+v", cs)
	}
	moved := live(t, `{"spec":{"ports":[{"name":"mcp","port":9999}]}}`)
	if got := strings.Join(FormatChanges(Diff(o, Prepare(o), moved)), "\n"); !strings.Contains(got, "spec.ports[8080]") {
		t.Errorf("a changed port number is a new element:\n%s", got)
	}
}

func TestDiff_NetworkPolicyPortsAreAnAtomicList(t *testing.T) {
	o := obj(t, "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: p, namespace: n}\nspec:\n  ingress:\n    - ports:\n        - {port: 80}\n")
	l := live(t, `{"spec":{"ingress":[{"ports":[{"port":81}]}]}}`)
	cs := Diff(o, Prepare(o), l)
	if len(cs) != 1 || cs[0].Path != "spec.ingress[0].ports[0].port" {
		t.Errorf("a list with no merge key compares by position: %+v", cs)
	}
}

func TestPrepare_DropsTheExecutorImageFromTheDesiredDocument(t *testing.T) {
	o := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: executor, namespace: n}\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n        - {name: executor, image: reg/x@sha256:1, imagePullPolicy: Always}\n        - {name: sidecar, image: reg/side:1}\n")
	d := Prepare(o)
	_, body, err := Patch(o, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "reg/x@sha256:1") {
		t.Errorf("the executor image is in the patch body: %s", body)
	}
	if !strings.Contains(string(body), "reg/side:1") || !strings.Contains(string(body), "imagePullPolicy") {
		t.Errorf("only the executor container's image is dropped: %s", body)
	}
	if raw, _ := json.Marshal(o.Doc); !strings.Contains(string(raw), "reg/x@sha256:1") {
		t.Errorf("Prepare must not mutate the parsed object")
	}
	other := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: loki, namespace: n}\nspec:\n  template:\n    spec:\n      containers:\n        - {name: loki, image: grafana/loki:1}\n")
	if _, b, _ := Patch(other, Prepare(other)); !strings.Contains(string(b), "grafana/loki:1") {
		t.Errorf("another Deployment keeps its image: %s", b)
	}
}

func TestMaskCreds_HidesUserinfoWhateverTheFieldIsCalled(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host/x":              "https://<redacted>@host/x",
		"postgres://u:p%40ss@db:5432/app?x=1": "postgres://<redacted>@db:5432/app?x=1",
		"see https://a:b@h and http://c@i/z":  "see https://<redacted>@h and http://<redacted>@i/z",
		"https://host/path@notuserinfo":       "https://host/path@notuserinfo",
		"mailto:someone@example.invalid":      "mailto:someone@example.invalid",
		"plain text with an @ sign":           "plain text with an @ sign",
	} {
		if got := MaskCreds(in); got != want {
			t.Errorf("MaskCreds(%q) = %q, want %q", in, got, want)
		}
	}
	got := FormatChanges([]Change{{Path: "spec.x[0].url", Live: "https://olduser:oldpw@h", Desired: "https://newuser:newpw@h"}})
	if s := strings.Join(got, "\n"); strings.Contains(s, "pw") || strings.Contains(s, "user:") {
		t.Errorf("FormatChanges printed a credential: %s", s)
	}
	got = FormatChanges([]Change{{Path: "spec.list", Missing: true, Desired: []any{map[string]any{"url": "https://u:secretpw@h"}}}})
	if s := strings.Join(got, "\n"); strings.Contains(s, "secretpw") {
		t.Errorf("a URL inside a structured value leaked: %s", s)
	}
}

func TestIdentityChanges_OnlyAChangedLiveValueCounts(t *testing.T) {
	ex := obj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: executor, namespace: n}\nspec:\n  template:\n    spec:\n      containers:\n        - name: executor\n          env:\n            - {name: ARGUS_CP_URL, value: \"https://new\"}\n            - {name: ARGUS_CLUSTER, value: \"c\"}\n            - {name: ARGUS_KUBE_CONTEXT_HOST, value: \"ctx\"}\n            - {name: ARGUS_GRAFANA_PUBLIC_URL, value: \"https://g\"}\n")
	l := live(t, `{"spec":{"template":{"spec":{"containers":[{"name":"executor","env":[
	  {"name":"ARGUS_CP_URL","value":"https://old"},
	  {"name":"ARGUS_CLUSTER","value":"c"},
	  {"name":"ARGUS_GRAFANA_PUBLIC_URL","value":"https://other"}]}]}}}}`)
	ics := IdentityChanges(ex, Diff(ex, Prepare(ex), l))
	if len(ics) != 1 || ics[0].Field != "env ARGUS_CP_URL" || ics[0].Live != "https://old" || ics[0].Desired != "https://new" {
		t.Errorf("want exactly the ARGUS_CP_URL change (kube context is an ADD, grafana is not identity): %+v", ics)
	}
	ns := obj(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: n\n  labels:\n    argus.onedroid.ai/tier: managed\n    argus.onedroid.ai/sut-namespace: sut\n    argus.onedroid.ai/instance: i\n")
	nl := live(t, `{"metadata":{"labels":{"argus.onedroid.ai/tier":"k3d","argus.onedroid.ai/instance":"j"}}}`)
	ics = IdentityChanges(ns, Diff(ns, Prepare(ns), nl))
	if len(ics) != 1 || ics[0].Field != "label argus.onedroid.ai/tier" {
		t.Errorf("want the tier label change only (sut-namespace is an add, instance is guarded elsewhere): %+v", ics)
	}
}

func TestWorkloadRefs_NamesWhatThePodsNeedAndHonoursOptional(t *testing.T) {
	o := obj(t, `apiVersion: apps/v1
kind: Deployment
metadata: {name: executor, namespace: n}
spec:
  template:
    spec:
      serviceAccountName: argus-executor
      imagePullSecrets: [{name: ghcr-pull}]
      containers:
        - name: executor
          envFrom:
            - secretRef: {name: exec-tokens}
            - configMapRef: {name: maybe, optional: true}
          env:
            - name: A
              valueFrom: {secretKeyRef: {name: keyed, key: k}}
      volumes:
        - {name: c, configMap: {name: argus-config}}
        - {name: i, secret: {secretName: exec-identity}}
        - {name: r, persistentVolumeClaim: {claimName: exec-results}}
        - {name: e, emptyDir: {}}
`)
	var got []string
	for _, r := range WorkloadRefs(o.Doc) {
		got = append(got, r.String())
	}
	want := "ConfigMap/argus-config PersistentVolumeClaim/exec-results Secret/exec-identity Secret/exec-tokens Secret/ghcr-pull Secret/keyed ServiceAccount/argus-executor"
	if strings.Join(got, " ") != want {
		t.Errorf("refs = %v\nwant   %s", got, want)
	}
}

func TestErrorClassifiers(t *testing.T) {
	if !IsAPINotFound(`kubectl get x: Error from server (NotFound): x "y" not found`) {
		t.Error("the API server's NotFound is absence")
	}
	for _, e := range []string{`could not execute kubectl: exec: "kubectl": executable file not found in $PATH`, `error: context "x" not found`, `Error from server (Forbidden): not found`} {
		if IsAPINotFound(e) {
			t.Errorf("%q is not the API server's NotFound", e)
		}
	}
	if !IsKindNotInstalled(`error: the server doesn't have a resource type "podmonitor"`) {
		t.Error("a resource type the server lacks is a kind not installed")
	}
}
