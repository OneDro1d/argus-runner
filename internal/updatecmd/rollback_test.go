package updatecmd

import (
	"strings"
	"testing"
)

// V31-001 §G-1 (VR13-UP) — THE ROLLBACK BLOCK.
//
// The owner's own design change (2026-09-13): after a successful update, the Environments page shows,
// under the executor version, a copy-able block of commands that puts the instance back on the
// previous version — and *"the command block must be ready to run on that machine, that's mean all
// variables must be properly filled"*.
//
// So it is the SAME renderer as the update block. One renderer, because two would drift, and the
// difference between "update to" and "roll back to" is three fields, not a second program.
func TestRenderBlock_Rollback(t *testing.T) {
	base := Instance{
		Tier:       "compose",
		InstanceID: "memstore-compose",
		Image:      "ghcr.io/onedro1d/argus-runner@sha256:new",
		KitDir:     "/home/op/kits/memstore",
	}

	t.Run("⛔ the block RUNS the installed image and TARGETS the previous one", func(t *testing.T) {
		// This is the rule that is easy to get backwards, and getting it backwards produces a block
		// that cannot work: a pre-0.3.32 image answers `unknown command "update"`, because 0.3.32 is
		// the release that ADDS the verb. So the NEWEST image — the one that just updated the
		// machine — is what runs, and the previous image is what it is pointed AT.
		in := base
		in.Rollback = true
		in.RollbackToVersion = "0.3.31"
		in.TargetImage = "ghcr.io/onedro1d/argus-runner@sha256:old"

		block, placeholder := RenderBlock(in)
		if block == "" {
			t.Fatal("a complete rollback instance must render")
		}
		if placeholder {
			t.Error("a compose instance carries no placeholder")
		}
		if !strings.Contains(block, "IMG='"+in.Image+"'") {
			t.Errorf("the block must RUN the installed image:\n%s", block)
		}
		if !strings.Contains(block, "--image '"+in.TargetImage+"'") {
			t.Errorf("…and target the previous one with --image:\n%s", block)
		}
		if !strings.Contains(block, "--rollback-to '0.3.31'") {
			t.Errorf("the block must name the version it goes back to:\n%s", block)
		}
		// ⛔ the scripts the plan writes call back into the INSTALLED image's verbs (SA-D19)
		if !strings.Contains(block, `--runner-image "$IMG"`) {
			t.Errorf("the rollback plan must run the installed image's verbs (--runner-image \"$IMG\"):\n%s", block)
		}
		if !strings.Contains(block, "--image-digest '"+in.TargetImage+"'") {
			t.Errorf("the rollback's digest is the recorded previous image, which the planner asserts:\n%s", block)
		}
		// ⛔ and the forward form's `--image "$IMG"` must be GONE — that is what would make the
		// rollback silently update to the version it is meant to leave.
		if strings.Contains(block, `--image "$IMG"`) {
			t.Errorf("the rollback still points --image at the INSTALLED image:\n%s", block)
		}
		// ⛔ THE KIT IT LANDS IS THE PREVIOUS RELEASE'S (V32 adversary gate). `update plan` runs in the installed
		// image, whose own bundle is the NEWER kit; the previous image's own `init` extracts its bundle into the
		// stage, and the plan stages THAT — before the plan runs, or the plan stages the wrong kit.
		initLine := `dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" '` + in.TargetImage + `' init "$STAGE_U/bundle"`
		ini, plan := strings.Index(block, initLine), strings.Index(block, " update plan ")
		if ini < 0 || plan < 0 || ini > plan {
			t.Errorf("the previous image's bundle is not extracted before the plan (init=%d plan=%d):\n%s", ini, plan, block)
		}
		if !strings.Contains(block, `--bundle "$STAGE_U/bundle"`) {
			t.Errorf("the rollback plan does not stage the previous release's bundle:\n%s", block)
		}
		if strings.Contains(block, "'"+in.Image+"' init") || strings.Contains(block, `"$IMG" init`) {
			t.Errorf("the kit is extracted from the INSTALLED image:\n%s", block)
		}
	})

	t.Run("⛔ a previous image recorded as a TAG renders no rollback", func(t *testing.T) {
		// `update plan` refuses any --image-digest that is not repo@sha256:…, so a tag is a block that fails
		// every time it is pasted — no block is the honest answer.
		in := base
		in.Rollback = true
		in.RollbackToVersion = "0.3.31"
		in.TargetImage = "ghcr.io/onedro1d/argus-runner:slim"
		if block, _ := RenderBlock(in); block != "" {
			t.Errorf("a rollback to an unpinned image was rendered:\n%s", block)
		}
	})

	t.Run("the update block is unchanged", func(t *testing.T) {
		block, _ := RenderBlock(base)
		if !strings.Contains(block, `--image "$IMG"`) {
			t.Errorf("a forward update still updates TO the image it pulls:\n%s", block)
		}
		if strings.Contains(block, "--rollback-to") {
			t.Errorf("a forward update carries no rollback flag:\n%s", block)
		}
	})

	t.Run("⛔ a half-built rollback renders NOTHING", func(t *testing.T) {
		// The renderer's existing rule: a block that is missing a value is not rendered at all,
		// because a half-built block is worse than no block — an operator pastes it and something
		// unintended happens.
		for _, name := range []string{"no target image", "no version"} {
			in := base
			in.Rollback = true
			in.RollbackToVersion = "0.3.31"
			in.TargetImage = "ghcr.io/onedro1d/argus-runner@sha256:old"
			switch name {
			case "no target image":
				in.TargetImage = ""
			case "no version":
				in.RollbackToVersion = ""
			}
			if block, _ := RenderBlock(in); block != "" {
				t.Errorf("%s: rendered a half-built rollback block:\n%s", name, block)
			}
		}
	})

	t.Run("an unsafe value is refused, as on the forward path", func(t *testing.T) {
		in := base
		in.Rollback = true
		in.RollbackToVersion = "0.3.31'; rm -rf /"
		in.TargetImage = "ghcr.io/onedro1d/argus-runner@sha256:old"
		if block, _ := RenderBlock(in); block != "" {
			t.Errorf("an unsafe version reached the block:\n%s", block)
		}
	})

	t.Run("a k8s tier carries its context on the rollback too", func(t *testing.T) {
		in := Instance{
			Tier: "k3d", InstanceID: "memstore1-k3d",
			Image: "ghcr.io/x@sha256:new", KitDir: "/home/op/kits/e1", KubeContext: "k3d-argus",
			Rollback: true, RollbackToVersion: "0.3.31", TargetImage: "ghcr.io/x@sha256:old",
		}
		block, placeholder := RenderBlock(in)
		if placeholder {
			t.Error("a recorded context is not a placeholder")
		}
		if !strings.Contains(block, "KCTX='k3d-argus'\n") || strings.Count(block, `--kube-context "$KCTX"`) != 2 {
			t.Errorf("the rollback must carry the context to BOTH verbs, or it acts on whatever is current:\n%s", block)
		}
	})
}
