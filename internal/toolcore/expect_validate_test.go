package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// VR10-S2-8 (S2-b, CR-1): both validators — `argus validate-config --scenarios` (this
// package's ValidateConfig) and `author__validate_scenario` (which calls this package's
// ValidateScenario) — REFUSE an assertion-shaped EXPECT bullet the MCP parser cannot classify,
// naming the file, the line and the bullet. Before this build the bullet vanished in silence.

func mcpScenarioWith(bullets ...string) string {
	lines := []string{
		"# Scenario: MCP content", "",
		"## Metadata",
		"- **ID**: MCP-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: mcp", "",
		"## TRIGGER",
		"POST `${MCP_URL}`", "",
		"```json",
		`{"transport":"streamable-http","tool":"ok_tool","args":{}}`,
		"```", "",
		"## EXPECT",
		"### Runnable", // VR12-E1
		"- result.isError == false",
	}
	for _, b := range bullets {
		lines = append(lines, "- "+b)
	}
	lines = append(lines, "", "## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.")
	return strings.Join(append(lines, ""), "\n")
}

// chainScenarioWith builds a chain scenario whose claims are the given `- step search: …` bullets.
//
// ⛔ V31-002: it used to put them in the step's in-JSON `expect` key, and took that JSON as its
// argument. The key is removed and refused by name, so the claims live where they now belong —
// which is also what puts them through the one classifier every other bullet goes through.
func chainScenarioWith(claims ...string) string {
	body := []string{
		"# Scenario: chain", "",
		"## Metadata",
		"- **ID**: CHAIN-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: chain", "",
		"## TRIGGER",
		"POST `chain`", "",
		"```json",
		`{"steps":[{"type":"mcp","name":"search","tool":"t","args":{}}]}`,
		"```", "",
		"## EXPECT",
		"### Runnable", // VR12-E1
	}
	for _, c := range claims {
		body = append(body, "- step search: "+c)
	}
	body = append(body, "", "### Non-runnable", "- a fixture for the validate-config path", "",
		"## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return strings.Join(body, "\n")
}

func lineOf(text, needle string) int {
	for i, l := range strings.Split(text, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	return -1
}

// an mcp/chain scenario needs a usable targets.mcp.base_url for config.Validate to pass at all.
const mcpTierCfg = twoTierCfg + `targets:
  mcp:
    base_url: http://127.0.0.1:1/mcp
`

// Acceptance 5 (validate-config half): the refusal lands in `errors` (valid:false, non-zero exit —
// onboarding's gate) and names file, line and bullet.
func TestValidateConfig_RefusesUnclassifiableExpectBulletsNamingFileLineAndBullet(t *testing.T) {
	e := tierEnv(t, mcpTierCfg)
	e.Tier = "compose"
	bullet := `content[0].text == "document not found"`
	mcpMD := mcpScenarioWith(bullet)
	os.MkdirAll(filepath.Join(e.ScenariosDir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "MCP-001-content.md"), []byte(mcpMD), 0o644)
	os.MkdirAll(filepath.Join(e.ScenariosDir, "chain"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "chain", "CHAIN-001-semi.md"),
		[]byte(chainScenarioWith("result.isError == false; body contains x")), 0o644)

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("validate-config must FAIL on an EXPECT bullet the MCP parser cannot classify (CR-1)")
	}
	m := payload.(map[string]any)
	text := errorText(m)
	if !strings.Contains(text, "MCP-001-content.md") || !strings.Contains(text, bullet) {
		t.Errorf("the error must name the file and quote the bullet; got %q", text)
	}
	if want := "line " + strconv.Itoa(lineOf(mcpMD, bullet)); !strings.Contains(text, want) {
		t.Errorf("the error must name the line (%s); got %q", want, text)
	}
	// ⛔ V31-002: it used to demand the word "list" — expectList's advice, "use the list form",
	// for a `;`-joined string in the step's in-JSON `expect`. That key is gone, so the claim goes
	// through the one classifier like every other bullet and gets its advice: one bullet per
	// assertion. The file and the step are still named, which is what the test is really about.
	if !strings.Contains(text, "CHAIN-001-semi.md") || !strings.Contains(text, "search") ||
		!strings.Contains(text, "one bullet per assertion") {
		t.Errorf("the chain claim's `;` string must be refused naming the file, the step and the one-bullet form; got %q", text)
	}
}

// No false reds: clean MCP and chain scenarios (plane bullets, `body has …`, the list form with a
// run-time ${saved.…}) keep validate-config valid.
func TestValidateConfig_CleanMCPAndChainScenariosStayValid(t *testing.T) {
	e := tierEnv(t, mcpTierCfg)
	e.Tier = "compose"
	os.MkdirAll(filepath.Join(e.ScenariosDir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "MCP-001-ok.md"), []byte(mcpScenarioWith("body has id containing abc")), 0o644)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "CHAIN-001-ok.md"),
		[]byte(chainScenarioWith("result.isError == false", "body has id containing ${saved.docId}")), 0o644)
	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if failed {
		t.Fatalf("clean scenarios must stay valid; errors: %s", errorText(payload.(map[string]any)))
	}
}

// Acceptance 5 (author__validate_scenario half) + acceptance 6: the bullet is an ERROR (valid:false),
// not a warning; an uncompilable `matching` regex is an authoring error at validate time.
func TestValidateScenario_UnclassifiableBulletIsAnErrorNotAWarning(t *testing.T) {
	bullet := `content[0].text == "x"`
	md := mcpScenarioWith(bullet)
	p, invalid, _ := ValidateScenario(md)
	if !invalid {
		t.Fatalf("an unclassifiable assertion-shaped bullet must make the scenario INVALID (CR-1): %+v", p)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `content[0].text == \"x\"`) || !strings.Contains(string(b), `"line":`+strconv.Itoa(lineOf(md, bullet))) {
		t.Errorf("errors must quote the bullet with its line: %s", b)
	}
	if p, invalid, _ = ValidateScenario(mcpScenarioWith("body has text matching (")); !invalid {
		t.Errorf("an uncompilable matching regex must be an authoring error: %+v", p)
	} else if b, _ := json.Marshal(p); !strings.Contains(string(b), "regex") {
		t.Errorf("the regex error must say it is the regex: %s", b)
	}
	if p, invalid, _ = ValidateScenario(mcpScenarioWith("body has id containing abc")); invalid {
		t.Errorf("a well-formed body assertion must stay valid: %+v", p)
	}
}

// S2-c: the test hat sees the enforced assertion TEXT; the product hat sees only the COUNT —
// the text carries the scenario's expected value (the dark-factory holdout).
func TestRedactExpected_ProductHatKeepsTheAssertionCountNotTheText(t *testing.T) {
	mk := func() *report.Report {
		return &report.Report{Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{{
			ID: "RACE-002", Status: "failed",
			Steps: []report.StepResult{{Name: "search", Status: "failed",
				AssertionsEnforced: []string{"content contains argus-race-1"}, AssertionsEnforcedCount: 1}},
		}}}}}
	}
	prod := mk()
	redactExpected(prod, role.Product)
	st := prod.Layers[0].Scenarios[0].Steps[0]
	if st.AssertionsEnforced != nil || st.AssertionsEnforcedCount != 1 {
		t.Errorf("product hat: text stripped, count kept; got %+v", st)
	}
	test := mk()
	redactExpected(test, role.Test)
	if st := test.Layers[0].Scenarios[0].Steps[0]; len(st.AssertionsEnforced) != 1 {
		t.Errorf("test hat keeps the text; got %+v", st)
	}
}

// P3 #24a — the amqp consume step's new body claim is redacted EXACTLY like every other engine's
// enforced-assertion text: it reaches report.StepResult.AssertionsEnforced through the same field
// (chain.judgeBroker), so redactExpected needs no amqp-specific case — this proves that holds,
// rather than assuming it from the shared field name.
func TestRedactExpected_AMQPConsumeBodyClaimRedactedLikeAnyOtherAssertion(t *testing.T) {
	mk := func() *report.Report {
		return &report.Report{Layers: []report.Layer{{Layer: "message-flow", Scenarios: []report.ScenarioResult{{
			ID: "AMQP-CONSUME-001", Status: "failed",
			Steps: []report.StepResult{{Name: "steal", Status: "failed",
				AssertionsEnforced:      []string{"broker accepts", `content contains "reveal-code-42"`},
				AssertionsEnforcedCount: 2}},
		}}}}}
	}
	prod := mk()
	redactExpected(prod, role.Product)
	st := prod.Layers[0].Scenarios[0].Steps[0]
	if st.AssertionsEnforced != nil || st.AssertionsEnforcedCount != 2 {
		t.Errorf("product hat: the amqp consume body claim's text must be stripped, count kept; got %+v", st)
	}
	test := mk()
	redactExpected(test, role.Test)
	if st := test.Layers[0].Scenarios[0].Steps[0]; len(st.AssertionsEnforced) != 2 ||
		st.AssertionsEnforced[1] != `content contains "reveal-code-42"` {
		t.Errorf("test hat keeps the text; got %+v", st)
	}
}

// The scenario-level twin (http, mcp and ui scenarios record their enforced checks on the scenario,
// not on a step): the same rule — the product hat keeps the count, never the text.
func TestRedactExpected_ProductHatKeepsTheScenarioAssertionCountNotTheText(t *testing.T) {
	mk := func() *report.Report {
		return &report.Report{Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{{
			ID: "ORD-010", Status: "passed",
			AssertionsEnforced:      []string{`content contains "ord-held-out"`, "column currency == EUR"},
			AssertionsEnforcedCount: 2,
		}}}}}
	}
	prod := mk()
	redactExpected(prod, role.Product)
	if sc := prod.Layers[0].Scenarios[0]; sc.AssertionsEnforced != nil || sc.AssertionsEnforcedCount != 2 {
		t.Errorf("product hat: text stripped, count kept; got %q / %d", sc.AssertionsEnforced, sc.AssertionsEnforcedCount)
	}
	test := mk()
	redactExpected(test, role.Test)
	if sc := test.Layers[0].Scenarios[0]; len(sc.AssertionsEnforced) != 2 || sc.AssertionsEnforcedCount != 2 {
		t.Errorf("test hat keeps the text; got %q / %d", sc.AssertionsEnforced, sc.AssertionsEnforcedCount)
	}
}

// item 25 — A NUMERIC BODY COMPARISON IS HOLDOUT MATERIAL TOO, THE SAME WAY AS ANY OTHER CLAIM.
//
// The threshold is exactly the kind of value redactExpected exists to strip — a product agent must
// not learn "the SUT must answer with latency_ms < 400" any more than it may learn a `contains`
// claim's asserted substring. The redaction is structural (by FIELD, never by the claim's own text
// or op), so this is a regression guard: a numeric claim must not slip through some future
// op-specific special case.
func TestRedactExpected_NumericBodyComparisonAlsoRedacted(t *testing.T) {
	const threshold = "399.5" // a distinctive number: must never survive into the product-hat JSON
	mk := func() *report.Report {
		return &report.Report{Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{{
			ID: "PERF-001", Status: "failed",
			Steps: []report.StepResult{{Name: "call", Status: "failed",
				AssertionsEnforced:      []string{"field latency_ms < " + threshold},
				AssertionsEnforcedCount: 1}},
			Failure: &report.Failure{Observed: "http status 200 matched but a numeric comparison in the " +
				"scenario's body assertion(s) found a non-numeric observed value"},
		}}}}}
	}
	prod := mk()
	redactExpected(prod, role.Product)
	b, _ := json.Marshal(prod)
	if strings.Contains(string(b), threshold) {
		t.Fatalf("the numeric threshold must not reach the product hat: %s", b)
	}
	st := prod.Layers[0].Scenarios[0].Steps[0]
	if st.AssertionsEnforced != nil || st.AssertionsEnforcedCount != 1 {
		t.Errorf("product hat: text stripped, count kept; got %+v", st)
	}
	test := mk()
	redactExpected(test, role.Test)
	if st := test.Layers[0].Scenarios[0].Steps[0]; len(st.AssertionsEnforced) != 1 ||
		!strings.Contains(st.AssertionsEnforced[0], threshold) {
		t.Errorf("test hat keeps the numeric claim's text; got %+v", st)
	}
}
