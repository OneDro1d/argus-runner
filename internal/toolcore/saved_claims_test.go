package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// — the REAL author entry point. `author__validate_scenario` and both writers go
// through ValidateAll; a numeric claim on an `http` chain step that the executor would refuse at
// preflight used to pass it and save.

func httpChainScenario(second string) string {
	return strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata", "- **ID**: CHN-S1", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "",
		"```json",
		`{"steps":[{"type":"http","name":"first","method":"GET","url":"http://x/a","save":{"n":"count"}},` +
			`{"type":"http","name":"second","method":"GET","url":"http://x/b"}]}`,
		"```", "",
		"## EXPECT", "### Runnable", "- step first: status=200", "- step second: " + second, "",
		"## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func TestValidateAll_RefusesANumericClaimTheExecutorWouldRefuse(t *testing.T) {
	for _, c := range []struct{ claim, want string }{
		{"body has messages > lots", "not a number"},
		{"body has messages => 5", "operator"},
		{"body has messages > ${saved.unsaved}", "no earlier step saves"},
		{"body has messages >", "step \"second\""},
	} {
		_, errs := ValidateAll(httpChainScenario(c.claim))
		var joined []string
		for _, e := range errs {
			joined = append(joined, e.Message)
		}
		if !strings.Contains(strings.Join(joined, "\n"), c.want) {
			t.Errorf("%q: want a write-time refusal containing %q, got %v", c.claim, c.want, joined)
		}
	}
	if _, errs := ValidateAll(httpChainScenario("body has messages > ${saved.n}")); len(errs) != 0 {
		t.Errorf("a numeric claim against a saved value must validate: %v", errs)
	}
}

func TestWriteScenario_RefusesANumericClaimTheExecutorWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	res, _, err := WriteScenario(dir, []byte(httpChainScenario("body has messages > lots")), "bad.md")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := res.(map[string]any); m["written"] != false {
		t.Fatalf("the write must be refused: %+v", res)
	}
	if _, serr := os.Stat(filepath.Join(dir, "bad.md")); serr == nil {
		t.Fatal("a refused scenario must not land on disk")
	}
}
