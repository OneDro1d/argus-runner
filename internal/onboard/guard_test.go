package onboard

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const scenarioMD = "# Scenario: X\n## Metadata\n- **ID**: X-001\n## TRIGGER\nPOST `/x`\n## EXPECT\n- status=202\n"

// D4: the product folder (argus-config.yaml) and the scenarios folder must be DISTINCT,
// non-overlapping, and NO scenario .md may live under the product folder — Claude Code's
// native Read/Glob are NOT token-gated, so a product-hat window whose cwd contains
// scenarios could read EXPECT directly, bypassing the MCP holdout.
func TestFolderGuard_OK(t *testing.T) {
	root := t.TempDir()
	prod := filepath.Join(root, "Product_agent")
	scen := filepath.Join(root, "Test_agent", "scenarios")
	write(t, filepath.Join(prod, "argus-config.yaml"), "project:\n  name: x\n")
	write(t, filepath.Join(scen, "X-001.md"), scenarioMD)
	if err := FolderGuard(prod, scen); err != nil {
		t.Fatalf("distinct non-overlapping folders must pass: %v", err)
	}
}

func TestFolderGuard_SameFolder(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "argus-config.yaml"), "project:\n  name: x\n")
	if err := FolderGuard(root, root); err == nil {
		t.Fatal("product folder == scenarios folder must be REJECTED (holdout)")
	}
}

func TestFolderGuard_Overlap(t *testing.T) {
	root := t.TempDir()
	prod := root
	scen := filepath.Join(root, "scenarios") // scenarios nested UNDER the product folder
	write(t, filepath.Join(prod, "argus-config.yaml"), "project:\n  name: x\n")
	write(t, filepath.Join(scen, "X-001.md"), scenarioMD)
	if err := FolderGuard(prod, scen); err == nil {
		t.Fatal("a scenarios folder nested under the product folder must be REJECTED (holdout)")
	}
}

func TestFolderGuard_ScenarioMarkdownUnderProduct(t *testing.T) {
	root := t.TempDir()
	prod := filepath.Join(root, "Product_agent")
	scen := filepath.Join(root, "Test_agent", "scenarios")
	write(t, filepath.Join(prod, "argus-config.yaml"), "project:\n  name: x\n")
	write(t, filepath.Join(prod, "leaked.md"), scenarioMD) // a scenario .md sitting in the product folder
	write(t, filepath.Join(scen, "X-001.md"), scenarioMD)
	if err := FolderGuard(prod, scen); err == nil {
		t.Fatal("a scenario .md present under the product folder must be REJECTED (holdout leak)")
	}
}

// A non-scenario markdown (README) under the product folder is fine.
func TestFolderGuard_ReadmeUnderProductOK(t *testing.T) {
	root := t.TempDir()
	prod := filepath.Join(root, "Product_agent")
	scen := filepath.Join(root, "Test_agent", "scenarios")
	write(t, filepath.Join(prod, "argus-config.yaml"), "project:\n  name: x\n")
	write(t, filepath.Join(prod, "README.md"), "# How to run\nplain docs, not a scenario\n")
	write(t, filepath.Join(scen, "X-001.md"), scenarioMD)
	if err := FolderGuard(prod, scen); err != nil {
		t.Fatalf("a non-scenario README under the product folder is fine: %v", err)
	}
}
