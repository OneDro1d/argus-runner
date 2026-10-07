package config

import (
	"os"
	"path/filepath"
	"testing"
)

// AC-D20b — a chain made only of `http` steps (AC-D20) calls the URLs its steps name; it never
// touches the SUT's MCP endpoint, so validate-config must not demand targets.mcp.base_url for it.
// A chain with even one mcp step (and no per-step server_url) still needs it.
func chainMD(id, steps string) string {
	return "# Scenario: c\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: HTTP Ingestion\n- **Tags**: chain\n\n" +
		"## TRIGGER\nPOST `chain`\n\n```json\n{\"steps\":[" + steps + "]}\n```\n\n" +
		"## EXPECT\n### Runnable\n- step a: status=200\n"
}

func TestValidate_PureHTTPChainNeedsNoMCPTarget(t *testing.T) {
	dir := t.TempDir()
	md := chainMD("CHH-1", `{"type":"http","name":"a","method":"GET","url":"http://sut.example/x"}`)
	if err := os.WriteFile(filepath.Join(dir, "CHH-1.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadYAML(t, "project:\n  name: coder\ntargets:\n  http:\n    base_url: http://sut.example\nscenarios:\n  timeout_default: 20s\n")
	errs, _, err := c.Validate(dir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, e := range errs {
		if contains(e.Message, "targets.mcp") {
			t.Fatalf("a chain of http steps only must not require targets.mcp: %+v", errs)
		}
	}
}

func TestValidate_ChainWithAnMCPStepStillNeedsTheMCPTarget(t *testing.T) {
	dir := t.TempDir()
	md := chainMD("CHH-2", `{"type":"http","name":"a","method":"GET","url":"http://sut.example/x"},`+
		`{"type":"mcp","name":"b","tool":"t","args":{}}`)
	if err := os.WriteFile(filepath.Join(dir, "CHH-2.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadYAML(t, "project:\n  name: coder\ntargets:\n  http:\n    base_url: http://sut.example\nscenarios:\n  timeout_default: 20s\n")
	errs, _, err := c.Validate(dir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	found := false
	for _, e := range errs {
		if contains(e.Message, "targets.mcp.base_url") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a chain with an mcp step and no server_url must still require targets.mcp.base_url: %+v", errs)
	}
}
