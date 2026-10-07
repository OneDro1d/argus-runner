package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ROUND-12 GATE 2 — THE DOCUMENTATION REFLECTS WHAT WAS ACTUALLY BUILT.
//
// The owner's standard, verbatim: *"builder need to make sure that they are updated. It means they
// reflect current build. It means if something new was added or removed those documents should
// reflect it."*
//
// ⛔ THAT IS STRONGER THAN "no stale strings". A grep proves a REMOVED thing is gone; it does not
// prove an ADDED thing is described. BOTH DIRECTIONS ARE REQUIRED, so this file has two halves.

// docFiles are the documents round 12 touched, plus the ones that describe the scenario format.
func docFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range []string{
		"skills/scenario-author/SKILL.md",
		"specs/14-skill-scenario-author.md",
		"specs/17-chain-scenarios.md",
		"specs/06-scenarios-orderservice.md",
		"specs/01-mcp-runner.md",
		"schemas/scenario.schema.yaml",
		"onboarding/HOW-TO-ARGUS-CONFIG.md",
		"onboarding/argus-config.template.yaml",
		"schemas/argus-config.schema.yaml",
		"docs/COMPATIBLE-SUT.md",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s must exist: %v", rel, err)
		}
		out[rel] = string(b)
	}
	return out
}

// ── HALF 1 — REMOVED THINGS MUST BE GONE ──────────────────────────────────────────────────────

// ⛔ `**Priority**` must appear in ZERO scenarios, docs and skill files. 119 scenarios carried it.
func TestGate2_PriorityIsGoneEverywhere(t *testing.T) {
	var found []string
	walkRepo(t, func(rel, body string) {
		// The findings register and the stage records are HISTORY — they record that the key was
		// removed and must keep saying so. Everything else is the current contract.
		if strings.HasPrefix(rel, "docs/dark-factory/") || strings.HasPrefix(rel, "argus/") ||
			strings.HasSuffix(rel, "_test.go") {
			return
		}
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "**Priority**") {
				continue
			}
			// A line that NAMES the removal is the fix, not a relapse.
			if strings.Contains(line, "REMOVED") || strings.Contains(line, "VR12-M") ||
				strings.Contains(line, "V30-003") || strings.Contains(line, "gone from the contract") {
				continue
			}
			found = append(found, rel+": "+strings.TrimSpace(line))
		}
	})
	if len(found) > 0 {
		t.Fatalf("`**Priority**` survives in %d place(s) — it was removed from the contract, so a "+
			"document that still declares it is telling an author to write a key the validator "+
			"refuses:\n  %s", len(found), strings.Join(found, "\n  "))
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────────────────────

func walkRepo(t *testing.T, fn func(rel, body string)) {
	t.Helper()
	root := filepath.FromSlash("../..")
	exts := map[string]bool{".md": true, ".yaml": true, ".yml": true, ".go": true}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			switch fi.Name() {
			case ".git", "node_modules", "argus", "bin", "_scratch":
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func walkScenarios(t *testing.T, fn func(rel, body string)) {
	t.Helper()
	var n int
	walkRepo(t, func(rel, body string) {
		if !strings.HasPrefix(rel, "examples/") || !strings.HasSuffix(rel, ".md") ||
			!strings.Contains(body, "- **ID**:") {
			return
		}
		n++
		fn(rel, body)
	})
	if n < 100 {
		t.Fatalf("only %d scenarios were walked — the catalogue holds ~119, so this test is not "+
			"seeing what it claims to", n)
	}
}

func sectionOf(body, name string) string {
	_, rest, ok := strings.Cut(body, "\n## "+name+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func lineAfter(body, prefix string) string {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), prefix))
		}
	}
	return ""
}

// ⛔ `cleanup_enabled` MUST BE GONE FROM EVERY SHIPPED CONFIG, not just the template.
//
// V29-015 deleted the switch, and I removed it from the template, the how-to and the schema — and
// MISSED the six shipped `argus-config*.yaml` files, including the one `argus init` hands every
// new user. Found by running `init` from the published image and reading what came out, which is
// what a deploy is for.
func TestGate2_CleanupEnabledIsGoneFromEveryShippedConfig(t *testing.T) {
	var found []string
	walkRepo(t, func(rel, body string) {
		if !strings.Contains(rel, "argus-config") || !strings.HasSuffix(rel, ".yaml") {
			return
		}
		for _, line := range strings.Split(body, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "cleanup_enabled:") {
				found = append(found, rel)
			}
		}
	})
	if len(found) > 0 {
		t.Fatalf("`cleanup_enabled` survives in %d shipped config(s) — an operator who copies one gets a "+
			"key the schema no longer declares, for a switch that no longer exists:\n  %s",
			len(found), strings.Join(found, "\n  "))
	}
}
