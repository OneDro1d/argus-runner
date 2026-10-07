package k8supgrade

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// IsWorkload reports whether the object runs pods: a workload is never patched or created while a
// thing its pods need is missing.
func (o Object) IsWorkload() bool {
	return o.Kind == "Deployment" || o.Kind == "DaemonSet" || o.Kind == "StatefulSet"
}

// Ref names an object a workload's pods depend on.
type Ref struct{ Kind, Name string }

func (r Ref) String() string { return r.Kind + "/" + r.Name }

// WorkloadRefs lists, sorted and de-duplicated, the objects the pod template of a workload needs to
// exist before its pods can start: Secrets and ConfigMaps it mounts or reads env from (unless marked
// optional), PVCs, the ServiceAccount and the image pull Secrets. Only names are read; never a value.
func WorkloadRefs(doc map[string]any) []Ref {
	seen := map[Ref]bool{}
	add := func(kind, name string, optional any) {
		if name == "" || optional == true {
			return
		}
		seen[Ref{kind, name}] = true
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	spec, _ := doc["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	pod, _ := tpl["spec"].(map[string]any)
	if pod == nil {
		return nil
	}
	if sa := str(pod, "serviceAccountName"); sa != "default" {
		add("ServiceAccount", sa, nil)
	}
	for _, p := range asList(pod["imagePullSecrets"]) {
		add("Secret", str(p, "name"), nil)
	}
	var source func(m map[string]any)
	source = func(m map[string]any) {
		if s, ok := m["secret"].(map[string]any); ok {
			add("Secret", str(s, "name")+str(s, "secretName"), s["optional"])
		}
		if s, ok := m["secretName"].(string); ok {
			add("Secret", s, m["optional"])
		}
		if c, ok := m["configMap"].(map[string]any); ok {
			add("ConfigMap", str(c, "name"), c["optional"])
		}
		if c, ok := m["persistentVolumeClaim"].(map[string]any); ok {
			add("PersistentVolumeClaim", str(c, "claimName"), nil)
		}
		if pr, ok := m["projected"].(map[string]any); ok {
			for _, src := range asList(pr["sources"]) {
				source(src)
			}
		}
	}
	for _, v := range asList(pod["volumes"]) {
		source(v)
	}
	for _, key := range []string{"containers", "initContainers"} {
		for _, c := range asList(pod[key]) {
			for _, ef := range asList(c["envFrom"]) {
				if r, ok := ef["secretRef"].(map[string]any); ok {
					add("Secret", str(r, "name"), r["optional"])
				}
				if r, ok := ef["configMapRef"].(map[string]any); ok {
					add("ConfigMap", str(r, "name"), r["optional"])
				}
			}
			for _, e := range asList(c["env"]) {
				vf, _ := e["valueFrom"].(map[string]any)
				if r, ok := vf["secretKeyRef"].(map[string]any); ok {
					add("Secret", str(r, "name"), r["optional"])
				}
				if r, ok := vf["configMapKeyRef"].(map[string]any); ok {
					add("ConfigMap", str(r, "name"), r["optional"])
				}
			}
		}
	}
	out := make([]Ref, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func asList(v any) []map[string]any {
	l, _ := v.([]any)
	var out []map[string]any
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---- identity ---------------------------------------------------------------------------------------

// IdentityEnv are the executor env values that say WHICH instance this is, of which control plane, in
// which workspace, on which cluster. Rendering another value into an existing instance repoints it.
var IdentityEnv = []string{
	"ARGUS_CP_URL", "ARGUS_WORKSPACE_ID", "ARGUS_INSTANCE_ID", "ARGUS_TIER", "ARGUS_CLUSTER",
	"ARGUS_SUT_NAMESPACE", "ARGUS_KUBE_CONTEXT_HOST",
}

// IdentityLabels are the Namespace labels that carry the same facts.
var IdentityLabels = []string{"argus.onedroid.ai/sut-namespace", "argus.onedroid.ai/tier"}

// IdentityChange is one identity field the render would CHANGE on the live object.
type IdentityChange struct {
	Field         string // e.g. "env ARGUS_CP_URL" or "label argus.onedroid.ai/tier"
	Live, Desired string
}

// IdentityChanges picks, from the differences of one object, the identity fields whose live value the
// render would change. ADDING one where the live object has none is not a change of identity, and is
// left as an ordinary difference.
func IdentityChanges(o Object, cs []Change) []IdentityChange {
	want := map[string]string{}
	switch {
	case o.Kind == "Deployment" && o.Name == ExecutorName:
		for _, n := range IdentityEnv {
			want["spec.template.spec.containers["+ExecutorName+"].env["+n+"].value"] = "env " + n
		}
	case o.Kind == "Namespace":
		for _, l := range IdentityLabels {
			want["metadata.labels."+l] = "label " + l
		}
	default:
		return nil
	}
	var out []IdentityChange
	for _, c := range cs {
		field, ok := want[c.Path]
		if !ok || c.Missing || isEmpty(c.Live) {
			continue
		}
		ls, _ := c.Live.(string)
		ds, _ := c.Desired.(string)
		out = append(out, IdentityChange{Field: field, Live: ls, Desired: ds})
	}
	return out
}

// CreateBody is the JSON `kubectl create -f -` reads for an object the cluster lacks: the plain object.
func CreateBody(desired map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(desired); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// NotFoundText and the other kubectl phrases upgrade tells apart. Matching is on the API server's own
// wording, never on a bare "not found": a missing kubectl binary or a missing kube context says that too.
func IsAPINotFound(err string) bool { return strings.Contains(err, "Error from server (NotFound)") }

// IsKindNotInstalled is kubectl's answer for a resource type the API server does not serve (a
// PodMonitor without the Prometheus Operator's CRD).
func IsKindNotInstalled(err string) bool {
	return strings.Contains(err, "the server doesn't have a resource type") ||
		strings.Contains(err, "no matches for kind")
}
