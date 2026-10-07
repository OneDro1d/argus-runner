package main

import (
	"os"
	"path/filepath"
	"testing"
)

// V32 adversary gate — `update plan` REFUSES THE INPUTS THAT WOULD BUILD THE WRONG PLAN, through the CLI.
//
//   - a rollback staged from THIS image's bundle lands the newer kit, skills and dashboard and records them
//     at the older version: a rollback must name the previous release's bundle (--bundle);
//   - an instance id is joined into host paths (the stage, the manifest): only a name onboarding accepts;
//   - a version that cannot be ordered switches off the downgrade gate and the shared-folder hold-back.
func TestUpdatePlan_RefusesWhatWouldBuildTheWrongPlan(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	root := t.TempDir()
	kit := filepath.Join(root, "kit")
	state := filepath.Join(root, "state")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	thisImage := filepath.Join(root, "bundle-this-image")
	previous := filepath.Join(root, "bundle-previous-release")
	write(filepath.Join(thisImage, "MARKER"), "THIS-IMAGE")
	write(filepath.Join(previous, "MARKER"), "PREVIOUS-RELEASE")
	write(filepath.Join(kit, "deploy", "compose", "env.i1"), "ARGUS_CP_URL=https://cp.invalid\n")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	obs := filepath.Join(root, "observed.json")
	write(obs, `{"tier":"compose","compose":{"project":"argus-inst-i1","services":[{"name":"executor","image":"ghcr.io/x/exec@sha256:old","digest":"ghcr.io/x/exec@sha256:old"}]},"env":{"ARGUS_CP_URL":"https://cp.invalid"}}`)
	t.Setenv("ARGUS_BUNDLE_DIR", thisImage)

	n := 0
	plan := func(extra ...string) (string, int) {
		n++
		stage := filepath.Join(root, "stage", string(rune('a'+n)))
		args := []string{"update", "plan", "--stage", stage, "--kit", kit, "--router-state", state, "--observed", obs,
			"--tier", "compose", "--image", "ghcr.io/x/exec@sha256:new", "--image-digest", "ghcr.io/x/exec@sha256:new"}
		return stage, dispatch(append(args, extra...))
	}
	marker := func(stage string) string {
		b, _ := os.ReadFile(filepath.Join(stage, "kit", "MARKER"))
		return string(b)
	}
	noPlan := func(t *testing.T, stage string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(stage, "apply.sh")); err == nil {
			t.Error("a refused plan still wrote apply.sh")
		}
	}

	t.Run("a forward plan stages this image's own bundle", func(t *testing.T) {
		stage, code := plan("--instance-id", "i1", "--version", "0.3.32")
		if code != exitOK || marker(stage) != "THIS-IMAGE" {
			t.Fatalf("a valid forward plan exited %d and staged %q", code, marker(stage))
		}
	})

	t.Run("⛔ a rollback with no previous bundle is refused", func(t *testing.T) {
		stage, code := plan("--instance-id", "i1", "--rollback-to", "0.3.31")
		if code == exitOK {
			t.Fatal("a rollback was planned from THIS image's bundle — it would put the newer kit back and call it 0.3.31")
		}
		noPlan(t, stage)
	})

	t.Run("a rollback stages the previous release's bundle", func(t *testing.T) {
		stage, code := plan("--instance-id", "i1", "--rollback-to", "0.3.31", "--bundle", previous)
		if code != exitOK {
			t.Fatalf("a rollback naming the previous bundle exited %d", code)
		}
		if got := marker(stage); got != "PREVIOUS-RELEASE" {
			t.Errorf("the rollback staged %q, want the previous release's kit", got)
		}
	})

	t.Run("⛔ an instance id that is a path is refused", func(t *testing.T) {
		stage, code := plan("--instance-id", "x/../../Documents", "--version", "0.3.32")
		if code == exitOK {
			t.Fatal("update plan accepted an instance id that walks out of the stage")
		}
		noPlan(t, stage)
		if code := dispatch([]string{"update", "discover", "--stage", filepath.Join(root, "stage-d"), "--kit", kit,
			"--instance-id", "x/../../Documents"}); code == exitOK {
			t.Error("update discover accepted an instance id that walks out of the stage")
		}
	})

	t.Run("⛔ a version that cannot be ordered is refused", func(t *testing.T) {
		for _, v := range []string{"0.0.0-dev", "0.0.0-src+abc123"} {
			stage, code := plan("--instance-id", "i1", "--version", v)
			if code == exitOK {
				t.Errorf("version %q was accepted — every version gate passes silently on it", v)
			}
			noPlan(t, stage)
		}
	})
}
