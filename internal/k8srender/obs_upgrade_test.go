package k8srender

// Adopted from the independent review of PR 546 (its verification test, made self-contained: it rendered
// from a file in the reviewer's sandbox, this renders the real obs.yaml in-process). `argus upgrade --apply`
// against an instance onboarded BEFORE this change (live Loki: RollingUpdate with the API server's defaults,
// an emptyDir `data`, no securityContext) must see the strategy and volume changes and send a patch that
// carries `rollingUpdate: null` and `$retainKeys`; applied, the same objects must show no drift on those paths.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8supgrade"
)

func liveJSON(t *testing.T, j string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestObsLoki_upgradeApplyHandlesOldLiveRollingUpdateAndEmptyDir(t *testing.T) {
	in := sampleInstance()
	in.Tier = "aks"
	manifest, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	objs, err := k8supgrade.Parse(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var loki k8supgrade.Object
	var found bool
	var pvcs []string
	for _, o := range objs {
		if o.Kind == "Deployment" && o.Name == "loki" {
			loki, found = o, true
		}
		if o.Kind == "PersistentVolumeClaim" {
			pvcs = append(pvcs, o.Name)
		}
	}
	if !found {
		t.Fatal("no loki Deployment in the rendered obs.yaml")
	}
	if len(pvcs) != 2 {
		t.Errorf("upgrade must see both claims as objects to create, got %v", pvcs)
	}

	liveOld := liveJSON(t, `{"spec":{"replicas":1,"selector":{"matchLabels":{"app":"loki"}},
	 "strategy":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":"25%","maxUnavailable":"25%"}},
	 "template":{"metadata":{"labels":{"app":"loki"},"annotations":{"argus.onedroid.ai/loki-config-sha256":"old"}},
	  "spec":{"containers":[{"name":"loki","volumeMounts":[{"name":"data","mountPath":"/loki"},{"name":"config","mountPath":"/etc/loki-argus","readOnly":true}]}],
	   "volumes":[{"name":"data","emptyDir":{}},{"name":"config","configMap":{"name":"loki-config"}}]}}}}`)
	desired := k8supgrade.Prepare(loki)
	changes := k8supgrade.Diff(loki, desired, liveOld)
	got := strings.Join(k8supgrade.FormatChanges(changes), "\n")
	for _, want := range []string{"spec.strategy", "volumes[data]"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diff against an old live Loki lacks %q:\n%s", want, got)
		}
	}
	for _, c := range changes {
		if c.Immutable {
			t.Errorf("unexpected immutable change: %+v", c)
		}
	}
	typ, body, err := k8supgrade.PatchFor(loki, desired, liveOld)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "strategic" {
		t.Errorf("patch type = %s, want strategic", typ)
	}
	if !strings.Contains(string(body), `"rollingUpdate":null`) {
		t.Errorf("the patch body lacks an explicit rollingUpdate: null: %s", body)
	}
	if !strings.Contains(string(body), `"$retainKeys"`) {
		t.Errorf("the patch body lacks $retainKeys: %s", body)
	}
}
