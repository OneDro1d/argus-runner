package updatecmd

import (
	"strings"
	"testing"
)

// ── BLIND ADVERSARY GATE 2 — VR10-U1, the block a HUMAN pastes into a shell ───────────────────
//
// Every value in the block is EXECUTOR-WRITTEN at some point of its life (SEC-3). The rules:
//   U1-2  every filled value passes SafeHostPath and is single-quoted; a refused value renders ""
//         (no block), never a repaired one
//   U1-5  the placeholder appears ONLY for tier ∈ {k3d, managed} with an empty recorded context
//   U1-12 the block is commands only
//
// Attacked here field by field, with the shapes a shell actually reacts to.

var gate2Injections = []struct{ name, val string }{
	{"single quote", `k3d-argus'`},
	{"quote-then-command", `x' ; rm -rf / ; echo '`},
	{"command substitution", `$(touch /tmp/pwned)`},
	{"backticks", "`touch /tmp/pwned`"},
	{"newline", "ctx\nrm -rf /"},
	{"carriage return", "ctx\rrm -rf /"},
	{"tab", "ctx\tx"},
	{"NUL", "ctx\x00x"},
	{"DEL", "ctx\x7fx"},
	{"bare semicolon", "a;b"},
	{"ampersand", "a&b"},
	{"dollar var", "$HOME"},
	{"space", "C:\\Program Files\\kit"},
}

// gate2Field builds an otherwise-valid Instance with ONE field poisoned.
func gate2Field(field, v string) Instance {
	in := Instance{Tier: "k3d", InstanceID: "orders-k3d", Image: "ghcr.io/x/y@sha256:abc",
		KitDir: `C:\Users\api\kit`, KubeContext: "k3d-argus"}
	switch field {
	case "InstanceID":
		in.InstanceID = v
	case "Image":
		in.Image = v
	case "KitDir":
		in.KitDir = v
	case "KubeContext":
		in.KubeContext = v
	case "Kubeconfig":
		in.Kubeconfig = v
	}
	return in
}

// TestGate2_RenderBlock_NoFieldCanBreakOutOfItsQuoting — for every field × every injection the block
// is either REFUSED ("") or the value sits inside single quotes with no quote of its own.
func TestGate2_RenderBlock_NoFieldCanBreakOutOfItsQuoting(t *testing.T) {
	for _, field := range []string{"InstanceID", "Image", "KitDir", "KubeContext", "Kubeconfig"} {
		for _, inj := range gate2Injections {
			in := gate2Field(field, inj.val)
			block, _ := RenderBlock(in)
			if block == "" {
				continue // refused — the correct answer for anything single-quoting cannot carry
			}
			// A rendered block must be ONE shell line per command: no bare control characters.
			for _, r := range block {
				if r != '\n' && (r < 0x20 || r == 0x7f) {
					t.Errorf("%s=%q (%s): the RENDERED block carries control byte %q", field, inj.val, inj.name, r)
				}
			}
			if strings.Count(block, "'")%2 != 0 {
				t.Errorf("%s=%q (%s): odd number of single quotes — the quoting does not close:\n%s",
					field, inj.val, inj.name, block)
			}
			// The one byte that can end single-quoting must never be inside a rendered value.
			if strings.Contains(inj.val, "'") {
				t.Errorf("%s=%q (%s): a value containing a single quote was RENDERED rather than refused:\n%s",
					field, inj.val, inj.name, block)
			}
			// A `$(…)` / backtick / ; value must appear only between single quotes.
			if strings.ContainsAny(inj.val, "$`;&") && !strings.Contains(block, "'"+inj.val+"'") &&
				!strings.Contains(block, "'"+MSYSPath(inj.val)+"'") && !strings.Contains(block, "'"+MixedPath(inj.val)+"'") {
				t.Errorf("%s=%q (%s): the value is not carried inside single quotes:\n%s", field, inj.val, inj.name, block)
			}
		}
	}
}

// TestGate2_RenderBlock_RefusalIsTotal — a refused field must not produce a HALF block.
func TestGate2_RenderBlock_RefusalIsTotal(t *testing.T) {
	for _, field := range []string{"InstanceID", "Image", "KitDir", "KubeContext", "Kubeconfig"} {
		in := gate2Field(field, "bad'value")
		block, ph := RenderBlock(in)
		if block != "" {
			t.Errorf("%s with a single quote rendered a block instead of \"\":\n%s", field, block)
		}
		if ph {
			t.Errorf("%s: a refused render still reported placeholder=true", field)
		}
	}
}

// TestGate2_RenderBlock_PlaceholderIsExactlyTheLegacyCase — U1-5, all nine combinations.
func TestGate2_RenderBlock_PlaceholderIsExactlyTheLegacyCase(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		for _, ctx := range []string{"", "k3d-argus"} {
			in := Instance{Tier: tier, InstanceID: "i", Image: "img", KitDir: `C:\kit`, KubeContext: ctx}
			block, ph := RenderBlock(in)
			wantPH := (tier == "k3d" || tier == "managed") && ctx == ""
			if ph != wantPH {
				t.Errorf("tier=%s ctx=%q: placeholder=%v want %v", tier, ctx, ph, wantPH)
			}
			if got := strings.Contains(block, Placeholder); got != wantPH {
				t.Errorf("tier=%s ctx=%q: block carries the placeholder=%v want %v\n%s", tier, ctx, got, wantPH, block)
			}
			if tier == "compose" && strings.Contains(block, "--kube-context") {
				t.Errorf("compose rendered a --kube-context:\n%s", block)
			}
		}
	}
	// An unknown tier renders nothing at all.
	if b, ph := RenderBlock(Instance{Tier: "aks", InstanceID: "i", Image: "img", KitDir: `C:\kit`}); b != "" || ph {
		t.Errorf("tier=aks (the SCRIPT vocabulary, not the CP's) rendered %q ph=%v — ValidTier must refuse it", b, ph)
	}
}

// TestGate2_RenderBlock_WindowsKitPathRendersTheWindowsForm — the MSYS spelling bash can open, the
// mixed spelling docker takes, and MSYS_NO_PATHCONV on the init line.
func TestGate2_RenderBlock_WindowsKitPathRendersTheWindowsForm(t *testing.T) {
	block, _ := RenderBlock(Instance{Tier: "k3d", InstanceID: "i", Image: "img",
		KitDir: `C:\Users\api\argus-kits\orders`, KubeContext: "k3d-argus",
		Kubeconfig: `C:\Users\api\.kube\example.yaml`})
	for _, want := range []string{
		"KIT='C:/Users/api/argus-kits/orders'",
		"KIT_U='/c/Users/api/argus-kits/orders'",
		`MSYS_NO_PATHCONV=1 docker run --rm ${HELPER_USER:+--user "$HELPER_USER" -e HOME=/tmp} "$@"`,
		"KCFG='/c/Users/api/.kube/example.yaml'",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the Windows form is missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, `\`) {
		t.Errorf("a backslash survived into the pasted block:\n%s", block)
	}
	// POSIX kit dir: no MSYS_NO_PATHCONV, and both spellings are the path as stored.
	pblock, _ := RenderBlock(Instance{Tier: "managed", InstanceID: "i", Image: "img",
		KitDir: "/home/bartek/kit", KubeContext: "example-overlay"})
	if strings.Contains(pblock, "MSYS_NO_PATHCONV") {
		t.Errorf("a POSIX kit dir rendered the Windows form:\n%s", pblock)
	}
	if !strings.Contains(pblock, "KIT_U='/home/bartek/kit'") || !strings.Contains(pblock, `bash "$STAGE_U/apply.sh"`) {
		t.Errorf("the POSIX form does not run the staged scripts from the path as stored:\n%s", pblock)
	}
}

// TestGate2_RenderBlock_CommandsOnly — U1-12: no comment lines, and the pull precedes the plan which
// precedes the apply (U1-3, carried to the new path).
func TestGate2_RenderBlock_CommandsOnly(t *testing.T) {
	block, _ := RenderBlock(Instance{Tier: "k3d", InstanceID: "i", Image: "img", KitDir: `C:\kit`, KubeContext: "c"})
	lines := strings.Split(block, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			t.Errorf("line %d is a comment, not a command: %q", i+1, l)
		}
	}
	pull, plan, apply := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "docker pull"):
			pull = i
		case strings.Contains(l, " update plan "):
			plan = i
		case strings.Contains(l, "apply.sh"):
			apply = i
		}
	}
	if !(pull >= 0 && plan > pull && apply > plan) {
		t.Errorf("the order is not pull -> plan -> apply (pull=%d plan=%d apply=%d):\n%s", pull, plan, apply, block)
	}
	if strings.Contains(block, "--control-plane") || strings.Contains(block, "--token") {
		t.Errorf("SA §0.7: the block must carry NO --control-plane and NO token:\n%s", block)
	}
}
