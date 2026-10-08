package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// — validate-config decides "does this check need the plain MCP target?" by the
// ENGINE the runtime would use, not by an `mcp` tag anywhere in the list.
//
// The runtime takes the first dispatch tag in a fixed order, chain before mcp (internal/argus/argus.go,
// scenario.NativeEngineTag). A check tagged `chain, mcp, auth` runs as a CHAIN: a chain of http steps
// never calls targets.mcp.base_url. needsPlainMCPTarget returned true on the `mcp` tag before it looked
// at `chain`, so onboarding refused such a check with "is an mcp/chain scenario but config has no
// usable targets.mcp.base_url".
func taggedChainMD(id, tags, steps string) string {
	return "# Scenario: c\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: HTTP Ingestion\n- **Tags**: " + tags + "\n\n" +
		"## TRIGGER\nPOST `chain`\n\n```json\n{\"steps\":[" + steps + "]}\n```\n\n" +
		"## EXPECT\n### Runnable\n- step a: status=200\n"
}

const httpOnlyConfig = "project:\n  name: coder\ntargets:\n  http:\n    base_url: http://sut.example\nscenarios:\n  timeout_default: 20s\n"

func validateOne(t *testing.T, id, md string) []ConfigError {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadYAML(t, httpOnlyConfig)
	errs, n, err := c.Validate(dir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n != 1 {
		t.Fatalf("Validate found %d scenarios, want 1 (the fixture did not parse as a scenario)", n)
	}
	return errs
}

func mcpTargetDemanded(errs []ConfigError) bool {
	for _, e := range errs {
		if contains(e.Message, "targets.mcp.base_url") {
			return true
		}
	}
	return false
}

func TestValidate_ChainCheckWithAnMCPSubjectTagNeedsNoMCPTarget(t *testing.T) {
	steps := `{"type":"http","name":"a","method":"GET","url":"http://sut.example/x"}`
	for _, tags := range []string{"chain, mcp, auth", "mcp, chain, auth"} {
		t.Run(tags, func(t *testing.T) {
			md := taggedChainMD("CST-1", tags, steps)
			s := scenario.Parse(md)
			if got := scenario.NativeEngineTag(s); got != scenario.ChainTag {
				t.Fatalf("premise: the runtime engine for tags %q is %q, want chain", tags, got)
			}
			if errs := validateOne(t, "CST-1", md); mcpTargetDemanded(errs) {
				t.Fatalf("a chain of http steps tagged %q runs on the chain engine and never calls the plain "+
					"MCP slot, but validate-config demanded targets.mcp.base_url: %+v", tags, errs)
			}
		})
	}
}

func TestValidate_ChainCheckWithAnMCPSubjectTagAndAnMCPStepStillNeedsTheMCPTarget(t *testing.T) {
	md := taggedChainMD("CST-2", "chain, mcp, auth",
		`{"type":"http","name":"a","method":"GET","url":"http://sut.example/x"},{"type":"mcp","name":"b","tool":"t","args":{}}`)
	if errs := validateOne(t, "CST-2", md); !mcpTargetDemanded(errs) {
		t.Fatalf("a chain with an mcp step and no target must still require targets.mcp.base_url: %+v", errs)
	}
}

func TestValidate_APlainMCPCheckStillNeedsTheMCPTarget(t *testing.T) {
	md := "# Scenario: m\n\n## Metadata\n- **ID**: CST-3\n- **Layer**: HTTP Ingestion\n- **Tags**: mcp, auth\n\n" +
		"## TRIGGER\nPOST `mcp`\n\n```json\n{\"tool\":\"t\",\"args\":{}}\n```\n\n" +
		"## EXPECT\n### Runnable\n- status=200\n"
	if errs := validateOne(t, "CST-3", md); !mcpTargetDemanded(errs) {
		t.Fatalf("an mcp check (the mcp engine) must still require targets.mcp.base_url: %+v", errs)
	}
}

// A self-contained chain whose mcp steps each carry their own server_url needs no
// targets.mcp.base_url; one mcp step without a URL still does.
func TestValidate_ChainWhoseMCPStepsCarryTheirOwnServerURLNeedsNoMCPTarget(t *testing.T) {
	md := taggedChainMD("CST-OWNURL-A", "chain, calm",
		`{"type":"mcp","name":"a","server_url":"http://mcp.example/mcp","tool":"t","args":{}},`+
			`{"type":"http","name":"b","method":"GET","url":"http://sut.example/x"},`+
			`{"type":"mcp","name":"c","server_url":"http://mcp2.example/mcp","tool":"t","args":{}}`)
	if errs := validateOne(t, "CST-OWNURL-A", md); mcpTargetDemanded(errs) {
		t.Fatalf("every mcp step names its own server_url, yet validate-config demanded targets.mcp.base_url: %+v", errs)
	}
}

func TestValidate_ChainWithOneMCPStepLackingAServerURLStillNeedsTheMCPTarget(t *testing.T) {
	md := taggedChainMD("CST-OWNURL-B", "chain, calm",
		`{"type":"mcp","name":"a","server_url":"http://mcp.example/mcp","tool":"t","args":{}},`+
			`{"type":"mcp","name":"c","tool":"t","args":{}}`)
	if errs := validateOne(t, "CST-OWNURL-B", md); !mcpTargetDemanded(errs) {
		t.Fatalf("an mcp step with no server_url and no target must still require targets.mcp.base_url: %+v", errs)
	}
}
