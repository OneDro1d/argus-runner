package k8srender

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The instance Loki must run OUR config, with deletes and retention on. The image's baked
// local-config.yaml has both off: /loki/api/v1/delete answers 404 and lines never age out, so a
// line carrying PII could only be removed by wiping the whole store (memstore-dev, 2026-09-19).
func TestRenderObs_lokiRunsOwnConfigWithDeletesAndRetention(t *testing.T) {
	managed := sampleInstance()
	managed.Tier = "aks"
	for _, in := range []Instance{sampleInstance(), managed} {
		got, err := RenderObs(in, cfgJSON(t))
		if err != nil {
			t.Fatal(err)
		}
		var cfgText string
		var dep map[string]any
		for _, d := range docs(t, got) {
			switch {
			case d["kind"] == "ConfigMap" && nameOf(d) == "loki-config":
				data, _ := d["data"].(map[string]any)
				cfgText, _ = data["loki.yaml"].(string)
			case d["kind"] == "Deployment" && nameOf(d) == "loki":
				dep = d
			}
		}
		if cfgText == "" {
			t.Fatalf("%s: no loki-config ConfigMap with a loki.yaml key", in.ID)
		}
		var lc struct {
			Compactor struct {
				RetentionEnabled   bool   `yaml:"retention_enabled"`
				DeleteRequestStore string `yaml:"delete_request_store"`
				WorkingDirectory   string `yaml:"working_directory"`
			} `yaml:"compactor"`
			Limits struct {
				AllowDeletes    bool   `yaml:"allow_deletes"`
				DeletionMode    string `yaml:"deletion_mode"`
				RetentionPeriod string `yaml:"retention_period"`
			} `yaml:"limits_config"`
			Common struct {
				PathPrefix string `yaml:"path_prefix"`
			} `yaml:"common"`
		}
		if err := yaml.Unmarshal([]byte(cfgText), &lc); err != nil {
			t.Fatalf("%s: loki.yaml does not parse: %v", in.ID, err)
		}
		if !lc.Limits.AllowDeletes {
			t.Errorf("%s: limits_config.allow_deletes must be true", in.ID)
		}
		if lc.Limits.DeletionMode != "filter-and-delete" {
			t.Errorf("%s: deletion_mode = %q, want filter-and-delete", in.ID, lc.Limits.DeletionMode)
		}
		if lc.Compactor.DeleteRequestStore != "filesystem" {
			t.Errorf("%s: compactor.delete_request_store = %q, want filesystem (unset = no delete API)", in.ID, lc.Compactor.DeleteRequestStore)
		}
		if !lc.Compactor.RetentionEnabled || lc.Limits.RetentionPeriod != "168h" {
			t.Errorf("%s: retention must be on at 168h, got enabled=%v period=%q", in.ID, lc.Compactor.RetentionEnabled, lc.Limits.RetentionPeriod)
		}
		if lc.Common.PathPrefix != "/loki" {
			t.Errorf("%s: common.path_prefix = %q, want /loki (the data volume)", in.ID, lc.Common.PathPrefix)
		}

		// The Deployment must point Loki at the mounted ConfigMap, not the image's baked file.
		spec := dig(dep, "spec", "template", "spec")
		cs, _ := spec["containers"].([]any)
		if len(cs) == 0 {
			t.Fatalf("%s: loki Deployment has no containers", in.ID)
		}
		c0 := cs[0].(map[string]any)
		args, _ := c0["args"].([]any)
		if len(args) != 1 || args[0] != "-config.file=/etc/loki-argus/loki.yaml" {
			t.Errorf("%s: loki args = %v, want [-config.file=/etc/loki-argus/loki.yaml]", in.ID, args)
		}
		mounted := false
		for _, m := range c0["volumeMounts"].([]any) {
			mm := m.(map[string]any)
			if mm["name"] == "config" && mm["mountPath"] == "/etc/loki-argus" {
				mounted = true
			}
		}
		fromCM := false
		for _, v := range spec["volumes"].([]any) {
			vm := v.(map[string]any)
			if cm, ok := vm["configMap"].(map[string]any); ok && vm["name"] == "config" && cm["name"] == "loki-config" {
				fromCM = true
			}
		}
		if !mounted || !fromCM {
			t.Errorf("%s: the loki-config ConfigMap is not mounted at /etc/loki-argus (mount=%v volume=%v)", in.ID, mounted, fromCM)
		}
		ann := dig(dep, "spec", "template", "metadata", "annotations")
		if ann["argus.onedroid.ai/loki-config-sha256"] != lokiConfigSHA256() {
			t.Errorf("%s: the pod template must carry the config checksum so a config change rolls Loki", in.ID)
		}
	}
}

func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		m = next
	}
	return m
}
