package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T5.4 — the money-path guard at the FIRST door: `argus validate-config` (and onboarding, which dies
// on it) refuses a money-handling SUT's scenario that is not a plain read, naming why.

func writeScenario(t *testing.T, dir, id, trigger string) {
	t.Helper()
	d := filepath.Join(dir, "http-ingestion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: smoke", "",
		"## TRIGGER", trigger, "",
		"## EXPECT", "### Runnable", "- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_MoneyHandlingRefusesWritesAndMoneyPathsByName(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "M-POST", "POST \x60http://sut.invalid/api/v1/strategies\x60")
	writeScenario(t, dir, "M-QUOTE", "GET \x60http://sut.invalid/api/v1/quote\x60")
	writeScenario(t, dir, "M-HEALTH", "GET \x60http://sut.invalid/health\x60")

	for _, money := range []bool{false, true} {
		c := &Config{MoneyHandling: money}
		c.Project.Name = "p"
		c.Targets.HTTP = &HTTPTarget{BaseURL: "http://sut.invalid"}
		errs, n, err := c.Validate(dir)
		if err != nil || n != 3 {
			t.Fatalf("Validate = %d scenarios, err %v; want 3, nil", n, err)
		}
		by := map[string]string{}
		for _, e := range errs {
			by[e.Scenario] += e.Message
		}
		if !money {
			if len(errs) != 0 {
				t.Fatalf("without money_handling the guard must change nothing, got %v", errs)
			}
			continue
		}
		if !strings.Contains(by["M-POST"], "money_handling:") || !strings.Contains(by["M-POST"], "is POST") {
			t.Errorf("M-POST not refused by name: %q", by["M-POST"])
		}
		if !strings.Contains(by["M-QUOTE"], `"quote" path`) {
			t.Errorf("M-QUOTE (a GET on a quote path) not refused: %q", by["M-QUOTE"])
		}
		if by["M-HEALTH"] != "" {
			t.Errorf("M-HEALTH (a GET health check) was refused: %q", by["M-HEALTH"])
		}
	}
}

func TestLoad_MoneyHandlingIsReadFromTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte("project:\n  name: p\nmoney_handling: true\ntargets:\n  http:\n    base_url: http://sut.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MoneyHandling {
		t.Fatal("money_handling: true in the file did not reach Config.MoneyHandling — the guard would stay off")
	}
}
