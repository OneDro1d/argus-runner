package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── AC-D54 (#365) — A TEARDOWN LEAVES THE MACHINE AS IT WAS BEFORE THE INSTANCE WAS ONBOARDED ─────
//
// Owner rulings 2026-10-02 (issue #365, second comment): everything created for the instance is deleted —
// onboarding, updates, rollbacks, diagnosis; a re-onboard behaves like a first one; per instance, so a
// neighbour keeps what it needs.
//
// These tests RUN the real teardown.sh under the harness (stubbed docker/kubectl/curl) from a scratch kit
// against a seeded machine, and assert on what is on disk afterwards. Their first form was written by
// a reviewer's agent ("analysis and proof by a reviewer's agent"); it is committed here as a real test, with
// two changes: the neighbour's router-state record now points at ITS OWN kit in the single-instance case
// (the original recorded it in the very kit expected to go, which a correct fix must refuse to delete), and
// the placeholder token values are spelled out.
//
// ⛔ EVERY SURVIVOR IS ITS OWN t.Errorf, so on an unfixed tree this file is RED with the full list.

const p54Inst = "acd54-proof"

func p54Write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func p54Exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// p54SeedKit writes a kit that onboarded inst (credentials, rendered set, sut-secrets, logs), optionally with a
// neighbour instance's files in the same kit.
func p54SeedKit(t *testing.T, kit, inst string, withNeighbour bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(kit, "onboarding"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := "ARGUS_RUNNER_TOKEN=runner-secret-" + inst + "\nARGUS_AUTHOR_TOKEN=author-secret-" + inst +
		"\nARGUS_IDENTITY_KEY_B64=key-secret-" + inst + "\nARGUS_MCP_IMAGE=img:test\nARGUS_MCP_PORT=8765\n"
	files := map[string]string{
		"deploy/compose/docker-compose.yaml":                     "name: argus\n",
		"deploy/compose/env." + inst:                             env,
		"deploy/compose/identity." + inst + ".key":               "raw-key-bytes\n",
		"deploy/compose/cp-author.token":                         "odts_synthetic_author\n",
		"deploy/compose/cp-session.token":                        "odts_synthetic_session\n",
		"deploy/compose/sut-secrets.env":                         "SUT_DB_PASSWORD=sut-secret-value\n",
		"deploy/compose/k8s-rendered/" + inst + "/executor.yaml": "kind: Deployment\n",
		"deploy/compose/promtail-config." + inst + ".yaml":       "server: {}\n",
		"deploy/compose/.image-prev." + inst:                     "img:older\n",
		"logs/onboard-20261001T100000Z-1.log":                    "transcript of " + inst + "\n",
	}
	if withNeighbour {
		files["deploy/compose/env.neighbour-c"] = "ARGUS_RUNNER_TOKEN=neighbour-runner-secret\nARGUS_MCP_IMAGE=img:test\n"
		files["deploy/compose/identity.neighbour-c.key"] = "neighbour-key\n"
	}
	for rel, body := range files {
		p54Write(t, filepath.Join(kit, filepath.FromSlash(rel)), body)
	}
}

type p54Machine struct {
	Kit, Prev, Stage, Ack string
	R                     harnessRun
}

// p54Run seeds the router state, the kit that onboarded, one update copy of it, the stage and an acknowledged
// stage, then runs teardown.sh (compose) from the harness's scratch kit with a de-register reply naming the kit.
// dereg is the cloud-deregister reply; extra seeds more router-state files; mutate runs after the seeding.
func p54Run(t *testing.T, withNeighbour bool, dereg string, mutate func(kit, prev string), extraFx []harnessFixture) p54Machine {
	t.Helper()
	root := t.TempDir()
	kit := filepath.Join(root, "argus-kits", p54Inst)
	p54SeedKit(t, kit, p54Inst, withNeighbour)
	prev := kit + ".prev-20261001T120000Z"
	if err := copyTreeForTest(kit, prev); err != nil {
		t.Fatalf("copy the kit to its update copy: %v", err)
	}
	stage := filepath.Join(filepath.Dir(kit), ".argus-update-"+p54Inst)
	p54Write(t, filepath.Join(stage, "outcome"), "rollback-failed\n")
	p54Write(t, filepath.Join(stage, "progress.json"), `{"id":"A-3","state":"failed","at":"2026-10-01T12:05:00Z"}`+"\n")
	ack := stage + ".acknowledged-20261001T130000Z"
	p54Write(t, filepath.Join(ack, "outcome"), "rollback-failed\n")

	seed := map[string]string{
		"state.json":                     `{"folders":[]}`,
		"installed/" + p54Inst + ".json": `{"instance_id":"` + p54Inst + `","tier":"compose","version":"0.3.48"}`,
		"installed/neighbour-c.json":     `{"instance_id":"neighbour-c","tier":"compose","version":"0.3.48"}`,
		"kits/" + p54Inst:                filepath.ToSlash(kit),
	}
	if withNeighbour {
		seed["kits/neighbour-c"] = filepath.ToSlash(kit)
	} else {
		// the neighbour lives in ITS OWN kit, elsewhere: this kit must still be deletable.
		other := filepath.Join(root, "other-kit")
		p54SeedKit(t, other, "neighbour-c", false)
		seed["kits/neighbour-c"] = filepath.ToSlash(other)
	}
	withRouterStateSeed(t, seed)
	if mutate != nil {
		mutate(kit, prev)
	}
	if dereg == "" {
		dereg = `{"deregistered": true, "kit_dir": "` + strings.ReplaceAll(filepath.FromSlash(kit), `\`, `\\`) + `"}` + "\n"
	}
	fx := append([]harnessFixture{{Match: "cloud-deregister", Out: dereg}}, extraFx...)
	r := runKitScriptSeeded(t, 40*time.Second, "teardown.sh", nil, fx,
		"--instance-id", p54Inst, "--tier", "compose", "--control-plane", "https://cp.example", "--image", "img:test")
	return p54Machine{Kit: kit, Prev: prev, Stage: stage, Ack: ack, R: r}
}

func p54Survivors(t *testing.T, label string, paths map[string]string) {
	t.Helper()
	n := 0
	for name, p := range paths {
		if p54Exists(p) {
			n++
			t.Errorf("SURVIVES the teardown - %s: %s", name, p)
		}
	}
	t.Logf("%s: %d of %d checked items survive", label, n, len(paths))
}

func p54NeedBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
}

// p54Controls: the run took the path under test and is not vacuous.
func p54Controls(t *testing.T, m p54Machine) {
	t.Helper()
	m.R.mustHaveCalled(t, "cloud-deregister")
	// PATH TAKEN: the recorded-kit pass reached the kit (it removes the live key there).
	if p54Exists(filepath.Join(m.Kit, "deploy", "compose", "identity."+p54Inst+".key")) {
		t.Fatalf("the recorded-kit pass never reached the kit - this run proves nothing\n%s", m.R.Stdout)
	}
	// NOT VACUOUS: the neighbour's installed record must survive, or the whole router state was deleted.
	if !p54Exists(filepath.Join(m.R.RouterState, "installed", "neighbour-c.json")) {
		t.Fatalf("the neighbour's installed record is gone - the whole router state was deleted, so this run "+
			"says nothing about per-instance cleanup\n%s", m.R.Stdout)
	}
}
