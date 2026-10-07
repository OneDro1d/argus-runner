package scenario

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discovered is one discovered scenario: its file path + parsed scenario.
type Discovered struct {
	Path     string
	Scenario *Scenario
}

// IsPlaceholderID reports whether an ID is empty or a literal template placeholder
// (e.g. <PREFIX>-<NNN>, <ID>). Such "scenarios" come from skill/example docs, never a
// real test, and must never be scanned, validated, listed, or RUN (the M25 phantom).
func IsPlaceholderID(id string) bool {
	return id == "" || strings.ContainsAny(id, "<>")
}

// DiscoverFiles walks dir for scenario markdown files, applying the ONE shared discovery
// hygiene rule every call site (validate-config, list, run, find) must use, so they all
// see the SAME set:
//   - skip dot-directories (.claude, .git, ...) entirely — a skill doc at
//     .claude/skills/scenario-author/SKILL.md is NOT a scenario (it parses to the
//     placeholder ID <PREFIX>-<NNN>, the M25 phantom that ran and 404'd);
//   - skip non-.md files and README*;
//   - skip files whose parsed ID is empty or a placeholder (IsPlaceholderID).
//
// It returns each surviving file paired with its parsed scenario, sorted by ID for
// determinism. Best-effort: unreadable entries are skipped, never fatal.
func DiscoverFiles(dir string) []Discovered {
	var out []Discovered
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Prune dot-directories (but never the walk root itself).
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".md" || strings.HasPrefix(d.Name(), "README") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		s := Parse(string(b))
		if IsPlaceholderID(s.ID) {
			return nil
		}
		out = append(out, Discovered{Path: p, Scenario: s})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Scenario.ID < out[j].Scenario.ID })
	return out
}
