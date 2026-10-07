package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

// FX-3 (M25): scenario discovery must apply ONE shared hygiene rule everywhere
// (validate / list / run / find), so a skill doc under a dot-dir or a placeholder
// draft can never be scanned as a real scenario — the source of the <PREFIX>-<NNN>
// phantom that ran and 404'd on the colleague rig.
func TestDiscoverFiles_SkipsDotDirsReadmesAndPlaceholders(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scn := func(id, layer string) string {
		return "## Metadata\n- **ID**: " + id + "\n- **Layer**: " + layer + "\n"
	}

	// a real scenario — must survive
	write("http/ORDE-001-valid.md", scn("ORDE-001", "HTTP Ingestion"))
	// a real scenario in a nested real dir — must survive
	write("database/ORDE-007-row.md", scn("ORDE-007", "Database State"))
	// README — must be skipped
	write("README.md", "# scenarios readme")
	// the phantom: scenario-author skill doc under a dot-dir, placeholder ID — must be skipped
	write(".claude/skills/scenario-author/SKILL.md", scn("<PREFIX>-<NNN>", "<Layer>... one of the 7"))
	// a stray placeholder draft NOT under a dot-dir — must be skipped (defense in depth)
	write("drafts/TEMPLATE.md", scn("<ID>", "HTTP Ingestion"))
	// a non-.md file — must be skipped
	write("notes.txt", scn("ORDE-999", "HTTP Ingestion"))

	got := DiscoverFiles(dir)
	ids := map[string]bool{}
	for _, d := range got {
		if d.Scenario == nil {
			t.Fatalf("DiscoverFiles returned a nil parsed scenario for %s", d.Path)
		}
		ids[d.Scenario.ID] = true
	}
	if len(got) != 2 || !ids["ORDE-001"] || !ids["ORDE-007"] {
		t.Fatalf("want exactly {ORDE-001, ORDE-007}; got %d files with ids %v", len(got), ids)
	}
	if ids["<PREFIX>-<NNN>"] || ids["<ID>"] {
		t.Fatal("a placeholder-ID file leaked through discovery (the phantom)")
	}
}

func TestIsPlaceholderID(t *testing.T) {
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"", true},
		{"<PREFIX>-<NNN>", true},
		{"<ID>", true},
		{"ORDE-001", false},
		{"SAGA-002", false},
		{"PERM-001", false},
	} {
		if got := IsPlaceholderID(c.id); got != c.want {
			t.Errorf("IsPlaceholderID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}
