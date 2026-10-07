package scenario

import (
	"strings"
	"testing"
)

// VR12-C1 / VR12-C3 (V29-015) — the `## CLEANUP` CONTRACT at the author path.

func cleanupMD(body string) string {
	return strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CLN-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"GET `${INGESTION_URL}/health`", "",
		"## EXPECT",
		"### Runnable",
		"- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP",
		body, "",
	}, "\n")
}

// EVERY block, IN ORDER. The old parser took the sql block ELSE the bash block — first wins, the
// rest silently discarded — so a scenario that declared both cleaned up half of what it made.
func TestParseCleanup_EveryBlockInOrder(t *testing.T) {
	c, unknown := ParseCleanup("```sql\nDELETE FROM a;\n```\n\ntext between\n\n```bash\nrm -f /tmp/x\n```")
	if len(unknown) != 0 {
		t.Fatalf("no unknown fences here: %v", unknown)
	}
	if len(c.Blocks) != 2 || c.Blocks[0].Form != CleanupSQL || c.Blocks[1].Form != CleanupBash {
		t.Fatalf("want [sql bash] in order, got %+v", c.Blocks)
	}
	if c.Blocks[0].Body != "DELETE FROM a;" || c.Blocks[1].Body != "rm -f /tmp/x" {
		t.Errorf("bodies mangled: %+v", c.Blocks)
	}
	if len(c.Runnable()) != 2 || c.IsNA() {
		t.Errorf("both blocks are runnable and this is not an N/A: %+v", c)
	}
}

// VR12-C3 — an unrecognised fence is NAMED, never dropped. Dropping it is how an author's deletion
// code sat in a file for months doing nothing.
func TestParseCleanup_UnknownFenceIsNamed(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"```python\nprint('bye')\n```", "python"},
		{"```\nrm -rf /tmp/x\n```", "(no language tag)"},
	} {
		c, unknown := ParseCleanup(tc.body)
		if len(unknown) != 1 || unknown[0] != tc.want {
			t.Errorf("ParseCleanup(%q) unknown = %v, want [%s]", tc.body, unknown, tc.want)
		}
		if len(c.Blocks) != 0 {
			t.Errorf("an unrunnable fence must not become a block: %+v", c.Blocks)
		}
	}
	// …and the validator refuses it BY NAME.
	_, errs := Validate(cleanupMD("```python\nprint('bye')\n```"))
	if !find(errs, "cannot execute (python)") {
		t.Errorf("the refusal must name the language, got %v", errs)
	}
}

func TestValidate_CleanupContract(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // "" = must be accepted
	}{
		{"a runnable sql block", "```sql\nDELETE FROM t WHERE correlation_id = '${correlation_id}';\n```", ""},
		{"a runnable bash block", "```bash\nrm -f /tmp/${correlation_id}\n```", ""},
		{"sh is bash", "```sh\ntrue\n```", ""},
		// ⚠ WHAT MUST NOT CHANGE: a proper `N/A — <reason>` keeps validating EXACTLY as today.
		// 70 of the 119 shipped files are in this shape and are correct as they stand.
		{"N/A with a reason", "N/A — this scenario is read-only.", ""},
		{"N/A with a terse reason", "N/A — read-only.", ""},
		{"N/A, no dash", "N/A nothing is written by this scenario", ""},
		// …and the refusals.
		{"bare N/A", "N/A", "no justification"},
		{"prose", "Removes the single order row written by this correlation_id.", "says nothing this runner can act on"},
		{"empty", "", "says nothing this runner can act on"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Validate(cleanupMD(c.body))
			got := find(errs, "CLEANUP")
			if c.want == "" && got {
				t.Fatalf("must be ACCEPTED; got %v", errs)
			}
			if c.want != "" && !find(errs, c.want) {
				t.Fatalf("want a refusal containing %q; got %v", c.want, errs)
			}
		})
	}
}

// A MISSING section is refused with its own message — the fix is different from an empty one.
func TestValidate_CleanupSectionIsRequired(t *testing.T) {
	md := cleanupMD("N/A — nothing to do.")
	md = md[:strings.Index(md, "## CLEANUP")]
	_, errs := Validate(md)
	if !find(errs, "## CLEANUP is required") {
		t.Fatalf("a missing section must be refused by name; got %v", errs)
	}
}
