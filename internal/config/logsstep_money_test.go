package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The money pin at the FIRST door (`argus validate-config`): a chain carrying a logs step on a
// money-handling instance is refused by name. moneyguard.go is untouched; its `default:` arm does this.

func writeLogsChain(t *testing.T, dir, id string) {
	t.Helper()
	d := filepath.Join(dir, "chains")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"logs","name":"l","scope":"correlation"}]}`,
		"```", "",
		"## EXPECT", "### Runnable", "- step l: body has count == 0", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_MoneyHandlingRefusesAChainWithALogsStepByName(t *testing.T) {
	dir := t.TempDir()
	writeLogsChain(t, dir, "M-LOGS")
	for _, money := range []bool{true, false} {
		c := &Config{MoneyHandling: money}
		c.Project.Name = "p"
		errs, n, err := c.Validate(dir)
		if err != nil || n != 1 {
			t.Fatalf("Validate = %d scenarios, err %v; want 1, nil", n, err)
		}
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Message)
		}
		joined := strings.Join(msgs, "\n")
		if money {
			if !strings.Contains(joined, `money_handling: chain step l is a "logs" step`) || !strings.Contains(joined, "only GET http steps may run") {
				t.Errorf("a logs step on a money-handling instance must be refused by name, got %q", joined)
			}
		} else if strings.Contains(joined, "money_handling") {
			t.Errorf("without money_handling the guard changes nothing, got %q", joined)
		}
	}
}
