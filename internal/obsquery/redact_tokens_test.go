package obsquery

import (
	"strings"
	"testing"
)

// ARGUS-CMP-3 reuses this ONE token scrubber (exported from openshell.go) for recorded outputs: no
// second regexp in the tree.
func TestRedactTokens_ExportedForRecordedOutputs(t *testing.T) {
	const tok = "Zq9XkL2mP8vR4tY7wN1c"
	for _, in := range []string{
		"Authorization: Bearer " + tok,
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz",
	} {
		got := RedactTokens(in)
		if got == in || !strings.Contains(got, redactedMark) {
			t.Errorf("RedactTokens(%q) = %q, want a redaction", in, got)
		}
		if strings.Contains(got, tok) || strings.Contains(got, "dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz") {
			t.Errorf("RedactTokens(%q) = %q still carries the token", in, got)
		}
	}
	for _, plain := range []string{"hello world", "Bearer token", "the order was created", "zebra-quartz-meadow-lantern"} {
		if got := RedactTokens(plain); got != plain {
			t.Errorf("RedactTokens(%q) changed ordinary text to %q", plain, got)
		}
	}
}
