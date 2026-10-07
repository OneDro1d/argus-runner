package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// the dry run and the apply say when a patch replaces the executor's pods, and say nothing
// when only a field outside the pod template changes.

// reorderedExecutor is the executor Deployment of a tester who once added an environment variable by hand:
// the same env entries as the render, in another order, plus HAND.
func reorderedExecutor(t *testing.T, c *upCluster) {
	t.Helper()
	c.setLive(t, "deployment.apps", "executor", func(m map[string]any) {
		spec := m["spec"].(map[string]any)
		cs := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		for _, x := range cs {
			if cm := x.(map[string]any); cm["name"] == "executor" {
				env := cm["env"].([]any)
				for i, j := 0, len(env)-1; i < j; i, j = i+1, j-1 {
					env[i], env[j] = env[j], env[i]
				}
				cm["env"] = append([]any{map[string]any{"name": "HAND", "value": "x"}}, env...)
			}
		}
		spec["revisionHistoryLimit"] = 10 // the one real difference, outside the pod template
	})
}

func executorEnvNames(dep map[string]any) []string {
	var out []string
	cs := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	for _, x := range cs {
		if cm := x.(map[string]any); cm["name"] == "executor" {
			for _, e := range cm["env"].([]any) {
				out = append(out, e.(map[string]any)["name"].(string))
			}
		}
	}
	return out
}

func TestUpgrade_AFieldOutsideThePodTemplateSaysNothingAboutARestart(t *testing.T) {
	c := newCluster(t, nil)
	reorderedExecutor(t, c)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Deployment/executor  DIFFERS (1 field)") || !strings.Contains(out, "revisionHistoryLimit") {
		t.Fatalf("the dry run must show the one difference:\n%s", out)
	}
	if strings.Contains(out, "POD RESTART") {
		t.Errorf("no restart line for a change outside spec.template:\n%s", out)
	}
	liveOrder := executorEnvNames(c.live(t, "deployment.apps", "executor"))
	applied, _, _ := run(t, c.upgradeArgs("--apply")...)
	// The body kubectl was given carries the LIVE env order (the wiring: upgrade passes live to PatchFor).
	var sent map[string]any
	for _, l := range strings.Split(c.patches(), "\n") {
		if body, ok := strings.CutPrefix(l, "deployment.apps/executor "); ok {
			if err := json.Unmarshal([]byte(body), &sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sent == nil {
		t.Fatalf("no patch sent for the executor:\n%s", c.patches())
	}
	var want []string
	for _, n := range liveOrder {
		if n != "HAND" {
			want = append(want, n)
		}
	}
	if got := executorEnvNames(sent); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the patch sent re-orders env: got %v, live order %v", got, want)
	}
	if strings.Contains(applied, "POD RESTART") || strings.Contains(applied, "pods are being replaced") {
		t.Errorf("no restart wording in the apply output either:\n%s", applied)
	}
	if !strings.Contains(applied, "=> patched Deployment/executor\n") {
		t.Errorf("the Deployment must be patched:\n%s", applied)
	}
	t.Logf("dry-run output for the reported shape (reordered env, one field outside the template):\n%s", out)
}

func TestUpgrade_ATemplateChangeIsAnnouncedInTheDryRunAndTheApply(t *testing.T) {
	c := newCluster(t, nil)
	reorderedExecutor(t, c)
	newerKit(t, c) // adds an env var and imagePullSecrets to the pod template
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"POD RESTART", "would REPLACE the pods of Deployment/executor", "ARGUS_GRAFANA_PUBLIC_URL", "imagePullSecrets"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ConfigMap/argus-config  DIFFERS") && strings.Count(out, "POD RESTART") != 1 {
		t.Errorf("only the Deployment restarts:\n%s", out)
	}
	t.Logf("dry-run output, template change:\n%s", out)
	applied, _, _ := run(t, c.upgradeArgs("--apply")...)
	for _, want := range []string{"will REPLACE the pods of Deployment/executor", "=> patched Deployment/executor (its pod template changed"} {
		if !strings.Contains(applied, want) {
			t.Errorf("apply output lacks %q:\n%s", want, applied)
		}
	}
}
