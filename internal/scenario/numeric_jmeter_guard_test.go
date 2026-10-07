package scenario

import (
	"strings"
	"testing"
)

// numeric_jmeter_guard_test.go — item 25(b): a numeric body comparison is only evaluated in Go
// (the chain `http` step, or an `mcp` scenario) — a JMeter-executed scenario's assertion script
// does not understand the operator and would silently mis-evaluate it as a substring check. The
// author path must refuse this BEFORE it reaches a run.

// insertRunnableBullet inserts an extra bullet right after the fixture's existing
// `- status=202` line, inside `### Runnable`.
func insertRunnableBullet(md, bullet string) string {
	return strings.Replace(md, "- status=202\n", "- status=202\n"+bullet+"\n", 1)
}

func TestValidate_NumericBodyComparisonRefusedOnAPlainHTTPScenario(t *testing.T) {
	md := insertRunnableBullet(validMD(), "- body has latency_ms > 100")
	_, errs := Validate(md)
	if !find(errs, "only evaluated by the chain") {
		t.Fatalf("a numeric body comparison on a plain (non-chain/mcp) scenario must be refused by name; got %v", errs)
	}
}

// The SAME scenario, but on a chain http step, must be ACCEPTED — the whole point of the guard is
// to key on the engine, not to ban the grammar.
func TestValidate_NumericBodyComparisonAcceptedOnAChainHTTPStep(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"check","method":"GET","url":"http://x/y"}]}`
	md := chainMD(trig, "### Runnable\n- step check: body has latency_ms > 100\n")
	_, errs := Validate(md)
	if find(errs, "only evaluated by the chain") {
		t.Fatalf("a numeric body comparison on a chain http step must NOT be refused; got %v", errs)
	}
}

// A malformed numeric form (bad operator) must be refused BY NAME, independent of the engine guard
// above — both errors may fire, but the specific one must always be present.
func TestValidate_MalformedNumericFormRefusedByNameEvenOnAPlainScenario(t *testing.T) {
	md := insertRunnableBullet(validMD(), "- body has latency_ms => 100")
	_, errs := Validate(md)
	if !find(errs, "operator") {
		t.Fatalf("a bad numeric operator must be refused by name; got %v", errs)
	}
}
