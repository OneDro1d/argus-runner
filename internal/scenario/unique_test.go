package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR12-M5 — the uniqueness PREDICATE works; the LOCUS is an open owner question.
//
// ⛔ This test deliberately does NOT assert that anything calls it. Wiring it would silently choose
// between three incompatible meanings of "unique" (per instance / per folder / per repo), which is
// exactly what the SA declined to do. The predicate is proven so that the owner's ruling, whenever
// it comes, costs a three-line call site and no further build.
func TestCheckIDUnique(t *testing.T) {
	taken := map[string]string{"ORD-001": "http-ingestion/ORD-001.md"}
	lookup := func(id string) (string, bool) { w, ok := taken[id]; return w, ok }

	if msg := CheckIDUnique("ORD-002", lookup); msg != "" {
		t.Errorf("a free id must be accepted, got %q", msg)
	}
	msg := CheckIDUnique("ORD-001", lookup)
	if msg == "" {
		t.Fatal("a taken id must be refused")
	}
	// The message must NAME where the collision is — "already used" without a location leaves the
	// author hunting, which is how a rule stops being used.
	if !strings.Contains(msg, "http-ingestion/ORD-001.md") {
		t.Errorf("the refusal must name WHERE the id is already used: %q", msg)
	}
	// No lookup ⇒ no opinion. A caller that cannot see a catalogue must not be given a verdict.
	if msg := CheckIDUnique("ORD-001", nil); msg != "" {
		t.Errorf("with no lookup there is no catalogue to be unique WITHIN: %q", msg)
	}
}

func readRepoFileForTest(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s must exist: %v", rel, err)
	}
	return string(b)
}
