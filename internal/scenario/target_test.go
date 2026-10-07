package scenario

import (
	"strings"
	"testing"
)

// VR10-S3 (V28-012): a scenario selects a named target with ONE word in Metadata —
// `- **Target**: <name>`. The parser carries it; the validator refuses it where the word has no
// meaning (a Web UI scenario uses app_url; a chain selects per step).

func withTarget(md, name string) string {
	return strings.Replace(md, "- **Layer**:", "- **Target**: "+name+"\n- **Layer**:", 1)
}

// The word is parsed into the scenario (visible through the parsed map today, `Target` after the fix).
func TestParse_TargetWord(t *testing.T) {
	s := Parse(withTarget(validMD(), "graph"))
	if got, _ := s.AsParsedMap()["target"].(string); got != "graph" {
		t.Fatalf("`- **Target**: graph` must be parsed; parsed map carries target=%q", got)
	}
	if got, ok := Parse(validMD()).AsParsedMap()["target"]; ok {
		t.Errorf("a scenario without the word must not carry a target, got %v", got)
	}
}

// VR10-S3-5: the word means one thing per layer — a Web UI scenario has none (it uses app_url).
func TestValidate_TargetOnWebUIRefused(t *testing.T) {
	md := strings.Replace(validMD(), "- **Layer**: HTTP Ingestion", "- **Layer**: Web UI", 1)
	md = strings.Replace(md, "- **Tags**: http, critical, idempotency", "- **Tags**: ui", 1)
	md = withTarget(md, "graph")
	_, errs := Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Target") && strings.Contains(e.Message, "Web UI") {
			found = true
			if e.Line == 0 {
				t.Errorf("the refusal must carry the **Target** line number: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("**Target** on a Web UI scenario must be refused by name, got: %v", errs)
	}
}

// A chain selects its MCP target PER STEP (`"target": "<name>"` on the step); a Metadata word on a
// chain scenario is refused and told where the word goes — never read, never silently ignored.
func TestValidate_TargetOnChainRefused(t *testing.T) {
	md := strings.Replace(validMD(), "- **Tags**: http, critical, idempotency", "- **Tags**: chain", 1)
	md = withTarget(md, "admin")
	_, errs := Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Target") && strings.Contains(e.Message, "chain") && strings.Contains(e.Message, `"target"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("**Target** on a chain scenario must be refused and point at the step field, got: %v", errs)
	}
}

// The word is accepted on the layers that have a kind (the config-side check of the NAME lives in
// config.Validate, which knows the declared entries).
func TestValidate_TargetOnHTTPAccepted(t *testing.T) {
	if _, errs := Validate(withTarget(validMD(), "graph")); len(errs) != 0 {
		t.Fatalf("**Target** on an HTTP Ingestion scenario is valid markdown-side, got: %v", errs)
	}
}
