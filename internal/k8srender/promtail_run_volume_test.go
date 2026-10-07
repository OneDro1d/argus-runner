package k8srender

// promtail_run_volume_test.go — AC-D35, k8s sites 3-5: promtail's `run` volume (where its rendered
// config points `positions: filename: /run/promtail/positions.yaml`) must not be an emptyDir. An
// emptyDir dies with the pod, so every promtail restart/recreate re-reads the WHOLE node's log
// history from scratch — the same cursor-loss defect as the two compose tiers. The fix is a
// hostPath under /var/lib/argus-promtail/<namespace>, keyed by the INSTANCE NAMESPACE so two
// instances' promtail DaemonSets scheduled on the same node never share, or reset, each other's
// cursor. One helper (promtailRunVolume) feeds all three render paths so they cannot drift.

import (
	"strings"
	"testing"
)

// promtailRunVolumeOf extracts the promtail DaemonSet's `run` volume entry (the raw YAML flow-map,
// e.g. "{name: run, hostPath: {...}}") from a rendered multi-doc manifest, or "" if no promtail
// DaemonSet is present. Read from the decoded doc set (not a substring search on the whole
// manifest) so this cannot accidentally match some OTHER volume named "run".
func promtailRunVolumeOf(t *testing.T, manifest string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if k, _ := d["kind"].(string); k != "DaemonSet" {
			continue
		}
		if nameOf(d) != "promtail" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		vols, _ := podSpec["volumes"].([]any)
		for _, v := range vols {
			vm, _ := v.(map[string]any)
			if vm["name"] == "run" {
				return vm
			}
		}
	}
	return nil
}

// assertRunVolumeIsPerNamespaceHostPath is the shared assertion for all three render modes: the
// `run` volume must be a hostPath under /var/lib/argus-promtail/<ns>, DirectoryOrCreate, and must
// NOT be an emptyDir.
func assertRunVolumeIsPerNamespaceHostPath(t *testing.T, manifest, ns string) {
	t.Helper()
	if strings.Contains(manifest, "{name: run, emptyDir: {}}") {
		t.Fatalf("promtail's run volume is still an emptyDir — its positions cursor dies with every pod restart:\n%s", manifest)
	}
	run := promtailRunVolumeOf(t, manifest)
	if run == nil {
		t.Fatalf("no promtail DaemonSet `run` volume found in rendered manifest:\n%s", manifest)
	}
	if _, isEmptyDir := run["emptyDir"]; isEmptyDir {
		t.Fatalf("promtail's run volume is an emptyDir: %+v", run)
	}
	hp, ok := run["hostPath"].(map[string]any)
	if !ok {
		t.Fatalf("promtail's run volume has no hostPath: %+v", run)
	}
	wantPrefix := "/var/lib/argus-promtail/" + ns
	path, _ := hp["path"].(string)
	if path != wantPrefix {
		t.Errorf("run volume hostPath.path = %q, want %q (keyed by the instance namespace so two "+
			"instances on one node never share a cursor)", path, wantPrefix)
	}
	if hp["type"] != "DirectoryOrCreate" {
		t.Errorf("run volume hostPath.type = %v, want DirectoryOrCreate (the node has no pre-existing directory to mount)", hp["type"])
	}
}

// Site 3: the bundled promtail DaemonSet (RenderObs, no ObsMode / --obs=bundled).
func TestRenderObs_bundled_promtailRunVolumeIsHostPathNotEmptyDir(t *testing.T) {
	in := sampleInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(bundled): %v", err)
	}
	assertRunVolumeIsPerNamespaceHostPath(t, got, in.Namespace())
}

// Site 4: renderExportPromtail (--obs=export, hosted-Loki target).
func TestRenderObs_export_promtailRunVolumeIsHostPathNotEmptyDir(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "https://logs.example.grafana.net"
	in.ObsLokiPushURL = "https://logs.example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"

	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, hosted-Loki): %v", err)
	}
	assertRunVolumeIsPerNamespaceHostPath(t, got, in.Namespace())
}

// Site 5: renderSharedInstanceObs (--obs=shared).
func TestRenderObs_shared_promtailRunVolumeIsHostPathNotEmptyDir(t *testing.T) {
	in := sharedInstance()
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(shared): %v", err)
	}
	assertRunVolumeIsPerNamespaceHostPath(t, got, in.Namespace())
}

// Two instances on the same node must get DIFFERENT hostPath directories (namespace-keyed), or a
// recreate on one could collide with, or reset, the other's cursor.
func TestPromtailRunVolume_differsPerNamespace(t *testing.T) {
	a := promtailRunVolume("argus-inst-a")
	b := promtailRunVolume("argus-inst-b")
	if a == b {
		t.Fatalf("promtailRunVolume must be keyed by namespace, got identical output for two different namespaces: %q", a)
	}
	if !strings.Contains(a, "argus-inst-a") || !strings.Contains(b, "argus-inst-b") {
		t.Fatalf("promtailRunVolume did not embed its namespace argument: a=%q b=%q", a, b)
	}
	if strings.Contains(a, "emptyDir") || strings.Contains(b, "emptyDir") {
		t.Fatalf("promtailRunVolume must never render an emptyDir: a=%q b=%q", a, b)
	}
}

// No promtail volume anywhere in this package's rendered output may be an emptyDir — a coarse net
// across all three render paths in one assertion, independent of the structured extraction above.
func TestRenderObs_noPromtailVolumeIsEmptyDir(t *testing.T) {
	cfg := cfgJSON(t)
	cases := []struct {
		name string
		in   Instance
	}{
		{"bundled", sampleInstance()},
		{"export", func() Instance {
			in := sampleInstance()
			in.ObsMode = "export"
			in.ObsLokiURL = "https://logs.example.grafana.net"
			in.ObsLokiPushURL = "https://logs.example.grafana.net/loki/api/v1/push"
			in.ObsCredentialVarName = "LOKI_CREDENTIAL"
			return in
		}()},
		{"shared", sharedInstance()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RenderObs(c.in, cfg)
			if err != nil {
				t.Fatalf("RenderObs(%s): %v", c.name, err)
			}
			if strings.Contains(got, "emptyDir: {}}") && strings.Contains(got, "name: run") {
				// A stricter substring check than assertRunVolumeIsPerNamespaceHostPath's exact
				// match, in case the emptyDir rendering ever changes shape (spacing, key order).
				if strings.Contains(got, "{name: run, emptyDir: {}}") {
					t.Fatalf("[%s] promtail run volume is still an emptyDir:\n%s", c.name, got)
				}
			}
		})
	}
}
