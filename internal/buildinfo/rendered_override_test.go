package buildinfo

// INT-001 (wave 1.5): stamping SOMEONE ELSE'S manifest is not the same act as reporting your OWN version.
//
// render-k8s used Resolve() for the executor Deployment's ARGUS_VERSION. With no operator override
// set, Resolve falls through to THIS binary's build stamp — the onboarding kit's — and baked it in as a
// literal. Since ARGUS_VERSION outranks the running binary's own stamp, and update.sh:256 only ever
// runs `kubectl set image`, the executor then ran binary A and reported version B.
//
// These tests pin the distinction, because the two functions look interchangeable at a call site and
// that is exactly how the defect got in.

import "testing"

func TestRenderedOverride_EmptyWhenTheOperatorSetNothing(t *testing.T) {
	// THE defect. Resolve() answers a version here; RenderedOverride must answer nothing, so the
	// executor's own build stamp wins at runtime.
	if got := RenderedOverride(""); got != "" {
		t.Errorf("RenderedOverride(\"\") = %q, want empty — a literal here outranks the running binary's "+
			"stamp, so a re-imaged executor reports the version it was ONBOARDED with (INT-001)", got)
	}
	if Resolve("") == "" {
		t.Skip("this build has no stamp and no VCS info, so the contrast below cannot be shown")
	}
	if RenderedOverride("") == Resolve("") {
		t.Error("RenderedOverride and Resolve agreed on the empty case — the whole point is that they " +
			"must NOT: Resolve substitutes this binary's stamp, RenderedOverride refuses to")
	}
}

func TestRenderedOverride_PassesAnExplicitOverrideThrough(t *testing.T) {
	// The override exists for pinning and experiments; the fix must not remove that path.
	if got := RenderedOverride("0.9.9-pinned"); got != "0.9.9-pinned" {
		t.Errorf("RenderedOverride = %q, want the operator's explicit value", got)
	}
}

func TestRenderedOverride_TrimsSoWhitespaceIsNotAFalseOverride(t *testing.T) {
	// A stray space from a shell would otherwise be a non-empty literal — silently reintroducing the bug.
	if got := RenderedOverride("   "); got != "" {
		t.Errorf("RenderedOverride(\"   \") = %q, want empty — whitespace must not count as an override", got)
	}
}
