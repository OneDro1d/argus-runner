// Package onboard holds the host-side onboarding guards/checks the colleague-rollout
// command runs (the four-gate harness contract). They are pure + unit-tested so the
// (disposable) onboarder script stays a thin wrapper.
package onboard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FolderGuard enforces the dark-factory holdout's FILESYSTEM boundary (D4). The MCP token
// gating hides scenario tools from the product hat, but Claude Code's native Read/Glob are
// NOT token-gated — so if the product-hat window's working dir contained scenarios, the
// agent could read EXPECT directly and bypass every redaction. Therefore: the product
// folder (argus-config.yaml) and the scenarios folder must be DISTINCT, neither nested in
// the other, and NO scenario .md may sit under the product folder. The onboarder refuses
// to proceed otherwise.
func FolderGuard(productDir, scenariosDir string) error {
	prod, err := filepath.Abs(productDir)
	if err != nil {
		return err
	}
	scen, err := filepath.Abs(scenariosDir)
	if err != nil {
		return err
	}
	prod = filepath.Clean(prod)
	scen = filepath.Clean(scen)

	if prod == scen {
		return fmt.Errorf("holdout: the product folder and the scenarios folder must be DIFFERENT directories (both = %q)", prod)
	}
	if isUnder(scen, prod) || isUnder(prod, scen) {
		return fmt.Errorf("holdout: the product folder %q and the scenarios folder %q must not be nested in each other — the product-hat window must never have the scenarios in reach (Read/Glob aren't token-gated)", prod, scen)
	}
	// No scenario .md may live under the product folder.
	var leaked string
	_ = filepath.WalkDir(prod, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || leaked != "" || !strings.HasSuffix(strings.ToLower(p), ".md") {
			return nil
		}
		if looksLikeScenario(p) {
			leaked = p
		}
		return nil
	})
	if leaked != "" {
		return fmt.Errorf("holdout: a scenario markdown is present under the product folder (%q) — move all scenarios into the scenarios folder so the product hat cannot read EXPECT", leaked)
	}
	return nil
}

// isUnder reports whether path is the same as or nested under base.
func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// looksLikeScenario reports whether a .md file carries the scenario contract markers
// (an **ID** plus a TRIGGER/EXPECT/VERIFY section) — so a plain README is not flagged.
func looksLikeScenario(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(b)
	hasID := strings.Contains(s, "**ID**")
	hasSection := strings.Contains(s, "## EXPECT") || strings.Contains(s, "## VERIFY") || strings.Contains(s, "## TRIGGER")
	return hasID && hasSection
}
