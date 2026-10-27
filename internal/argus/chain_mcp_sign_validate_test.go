package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The validators' half: an undeclared key reference is refused with the config in hand, a
// malformed block by the config-free validator, and neither says a key.

func signScenario(sign string) *scenario.Scenario {
	step := `{"type":"mcp","name":"w","tool":"t","args":{"a":"x"},"sign":` + sign + `}`
	return scenario.Parse(chainRunMD("", `{"steps":[`+step+`]}`, "### Runnable\n- step w: body contains ok\n"))
}

const okSign = `{"key":"MEMBER_A_KEY","fields":[{"path":"a"}],"header":"X-Sig"}`

func TestCheckSigningKeys_UndeclaredReferenceIsRefusedByName(t *testing.T) {
	c := &config.Config{CheckEnv: []string{"OTHER_KEY"}}
	errs := CheckSigningKeys(signScenario(okSign), c)
	if len(errs) != 1 || !strings.Contains(errs[0], `"MEMBER_A_KEY"`) || !strings.Contains(errs[0], "check_env") {
		t.Fatalf("want one refusal naming MEMBER_A_KEY and check_env: %v", errs)
	}
	c.CheckEnv = append(c.CheckEnv, "MEMBER_A_KEY")
	if errs := CheckSigningKeys(signScenario(okSign), c); len(errs) != 0 {
		t.Fatalf("a declared reference is fine: %v", errs)
	}
	if errs := CheckSigningKeys(signScenario(okSign), nil); len(errs) != 0 {
		t.Fatalf("without a config nothing is reported: %v", errs)
	}
}

func TestSignBlock_MalformedIsRefusedByTheScenarioValidator(t *testing.T) {
	step := `{"type":"mcp","name":"w","tool":"t","args":{},"sign":{"key":"K","fields":[{"path":"a"}]}}`
	md := chainRunMD("", `{"steps":[`+step+`]}`, "### Runnable\n- step w: body contains ok\n")
	_, errs := scenario.Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "`arg` and/or `header`") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a sign block with no destination must be refused by scenario.Validate: %+v", errs)
	}
}

func TestSignBlock_MalformedIsRefusedAtTheExecutorsDoorToo(t *testing.T) {
	t.Setenv("MEMBER_A_KEY", "irrelevant")
	c := signCfg(t, "http://127.0.0.1:1", "MEMBER_A_KEY")
	step := `{"type":"mcp","name":"w","tool":"t","args":{},"sign":{"key":"MEMBER_A_KEY","fields":[{"path":"a"}]}}`
	s := scenario.Parse(chainRunMD("", `{"steps":[`+step+`]}`, "### Runnable\n- step w: body contains ok\n"))
	res := runChainScenario(c, s, "tr-x", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("a malformed block is an authoring error refused at preflight: %q %+v", res.Status, res.Failure)
	}
}
