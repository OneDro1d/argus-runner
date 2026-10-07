package k8srender

import (
	"strings"
	"testing"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §1: "render-k8s (and the compose render) will
// also embed every file a `path:` names, as extra ConfigMap keys, mounted next to the config at
// the same relative path." These tests are the item-1 PROMISE: `Config.MessageSchemaFiles()`
// (internal/config/schemas.go) has a caller here, and that caller both embeds the bytes AND wires
// the executor to read them back at /config/<relative path> — not just as a flat, unmountable
// ConfigMap key (ConfigMap keys cannot contain "/", so a naive embed would silently fail to
// reproduce the declared relative path in the pod's filesystem).

func instanceWithSchemas(files map[string][]byte) Instance {
	in := sampleInstance()
	in.MessageSchemaFiles = files
	return in
}

func TestRenderExecutor_EmbedsMessageSchemaFile_AsExtraConfigMapKey(t *testing.T) {
	yaml, err := RenderExecutor(instanceWithSchemas(map[string][]byte{
		"schemas/ping.avsc": []byte(`{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`),
	}))
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ds := docs(t, yaml)
	var cm map[string]any
	for _, d := range ds {
		if k, _ := d["kind"].(string); k == "ConfigMap" {
			if name, _ := d["metadata"].(map[string]any)["name"].(string); name == "argus-config" {
				cm = d
				break
			}
		}
	}
	if cm == nil {
		t.Fatal("no argus-config ConfigMap in rendered output")
	}
	data, _ := cm["data"].(map[string]any)
	if data == nil {
		t.Fatal("argus-config ConfigMap has no data")
	}
	if _, ok := data["argus-config.yaml"]; !ok {
		t.Fatal("argus-config.yaml key missing from ConfigMap data")
	}
	// The schema's own content must be embedded SOMEWHERE in the ConfigMap's data (under
	// whatever key encoding the render chose — ConfigMap keys may not contain "/", so it will
	// not be literally "schemas/ping.avsc").
	found := false
	for k, v := range data {
		if k == "argus-config.yaml" {
			continue
		}
		if s, _ := v.(string); strings.Contains(s, `"name":"Ping"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("schema file content not found under any extra ConfigMap key; data keys: %v", keysOf(data))
	}
}

func TestRenderExecutor_MountsMessageSchemaFile_AtItsDeclaredRelativePath(t *testing.T) {
	yaml, err := RenderExecutor(instanceWithSchemas(map[string][]byte{
		"schemas/ping.avsc": []byte(`{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`),
	}))
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	ds := docs(t, yaml)
	var dep map[string]any
	for _, d := range ds {
		if k, _ := d["kind"].(string); k == "Deployment" {
			dep = d
		}
	}
	if dep == nil {
		t.Fatal("no Deployment in rendered output")
	}
	// Find the "config" volume and confirm it carries an `items:` list mapping SOME key to
	// path: schemas/ping.avsc — the executor reads the schema at /config/schemas/ping.avsc
	// (mountPath /config + that subPath), matching what the config loader resolves `path:`
	// relative to the config file (design §1).
	spec, _ := dep["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	vols, _ := podSpec["volumes"].([]any)
	var configVol map[string]any
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] == "config" {
			configVol = vm
		}
	}
	if configVol == nil {
		t.Fatal("no `config` volume in the executor Deployment")
	}
	cmVol, _ := configVol["configMap"].(map[string]any)
	if cmVol == nil {
		t.Fatal("`config` volume has no configMap source")
	}
	items, _ := cmVol["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("`config` volume's configMap has no `items:` — a ConfigMap key with an encoded name would otherwise mount FLAT as that key, never at schemas/ping.avsc; configMap: %+v", cmVol)
	}
	var gotConfigYAML, gotSchema bool
	for _, it := range items {
		im, _ := it.(map[string]any)
		switch im["path"] {
		case "argus-config.yaml":
			gotConfigYAML = true
		case "schemas/ping.avsc":
			gotSchema = true
		}
	}
	if !gotConfigYAML {
		t.Errorf("`items:` must still map argus-config.yaml to itself, got %+v", items)
	}
	if !gotSchema {
		t.Errorf("`items:` must map some key to path schemas/ping.avsc, got %+v", items)
	}
}

func TestRenderExecutor_NoMessageSchemas_ConfigVolumeUnchanged(t *testing.T) {
	// Backward compatibility: an instance declaring NO message_schemas must render the exact
	// flat `configMap: {name: argus-config}` form (no items:) that every pre-existing render
	// (and its golden/byte-comparison expectations) already assumes.
	yaml, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if !strings.Contains(yaml, "configMap: {name: argus-config}") {
		t.Errorf("with no message_schemas, the config volume must stay the flat one-line form; got:\n%s", yaml)
	}
}

// #418 — onboarding's k3d/AKS auth pre-flight pod no longer hand-writes its config volume: it reads
// the APPLIED executor Deployment's `config` volume back (onboard.sh preflight_config_volume,
// `jsonpath='{...volumes[?(@.name=="config")]}'`) so the two cannot drift. That read is only sound
// if the render keeps this contract: exactly ONE volume named `config`, a configMap source, and
// every `items:` key present in the argus-config ConfigMap's data (a dangling key would make the
// pod, executor and pre-flight alike, fail to start).
func TestRenderExecutor_ConfigVolume_IsTheSingleSourceOnboardingReadsBack(t *testing.T) {
	yaml, err := RenderExecutor(instanceWithSchemas(map[string][]byte{
		"schemas/ping.avsc": []byte(`{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`),
		"schemas/pong.avsc": []byte(`{"type":"record","name":"Pong","fields":[{"name":"id","type":"string"}]}`),
	}))
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	var data map[string]any
	var configVols []map[string]any
	for _, d := range docs(t, yaml) {
		switch d["kind"] {
		case "ConfigMap":
			if md, _ := d["metadata"].(map[string]any); md["name"] == "argus-config" {
				data, _ = d["data"].(map[string]any)
			}
		case "Deployment":
			spec, _ := d["spec"].(map[string]any)
			tmpl, _ := spec["template"].(map[string]any)
			ps, _ := tmpl["spec"].(map[string]any)
			vols, _ := ps["volumes"].([]any)
			for _, v := range vols {
				if vm, _ := v.(map[string]any); vm["name"] == "config" {
					configVols = append(configVols, vm)
				}
			}
		}
	}
	if len(configVols) != 1 {
		t.Fatalf("the executor must carry exactly ONE volume named config (onboarding filters on that name), got %d", len(configVols))
	}
	cm, _ := configVols[0]["configMap"].(map[string]any)
	if cm == nil || cm["name"] != "argus-config" {
		t.Fatalf("config volume must be a configMap source on argus-config, got %+v", configVols[0])
	}
	items, _ := cm["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("want argus-config.yaml + 2 schema items, got %+v", items)
	}
	for _, it := range items {
		im, _ := it.(map[string]any)
		k, _ := im["key"].(string)
		if _, ok := data[k]; !ok {
			t.Errorf("items key %q has no entry in the argus-config ConfigMap data (keys %v)", k, keysOf(data))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
