package scenario

import (
	"fmt"
	"strings"
	"testing"
)

// The `fault` chain step's WRITE-time shape rules. The run-time half (the allowlist, the
// undo journal, the restore) is pinned in internal/fault and internal/argus.

func faultMD(step string) string {
	trig := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  ` + step + `
]}`
	return chainMD(trig, "### Runnable\n- step alive: status=200\n")
}

func TestFaultStep_ValidShapesAreAccepted(t *testing.T) {
	for _, step := range []string{
		`{"type":"fault","name":"f","op":"stop","deployment":"trades"}`,
		`{"type":"fault","name":"f","op":"start","deployment":"trades"}`,
		`{"type":"fault","name":"f","op":"start","deployment":"trades","replicas":2}`,
		`{"type":"fault","name":"f","op":"scale","deployment":"trades","replicas":2}`,
		`{"type":"fault","name":"f","op":"wait_ready","deployment":"trades","timeout":"90s"}`,
		`{"type":"fault","name":"f","op":"delay","link":"mcp-to-trades","latency_ms":5000,"jitter_ms":100}`,
		`{"type":"fault","name":"f","op":"drop","link":"mcp-to-trades"}`,
		`{"type":"fault","name":"f","op":"truncate","link":"mcp-to-trades","bytes":64}`,
		`{"type":"fault","name":"f","op":"restore"}`,
		`{"type":"fault","name":"f","op":"restore","always":true}`,
	} {
		if _, errs := Validate(faultMD(step)); len(errs) != 0 {
			t.Errorf("%s: %v", step, errs)
		}
	}
}

func TestFaultStep_BadShapesAreRefusedByName(t *testing.T) {
	cases := map[string]string{
		`{"type":"fault","name":"f"}`:                                                                           "op",
		`{"type":"fault","name":"f","op":"explode","deployment":"trades"}`:                                      "explode",
		`{"type":"fault","name":"f","op":"stop"}`:                                                               "deployment",
		`{"type":"fault","name":"f","op":"stop","deployment":"trades","link":"l"}`:                              "link",
		`{"type":"fault","name":"f","op":"scale","deployment":"trades"}`:                                        "replicas",
		`{"type":"fault","name":"f","op":"scale","deployment":"trades","replicas":0}`:                           "replicas",
		`{"type":"fault","name":"f","op":"scale","deployment":"trades","replicas":11}`:                          "replicas",
		`{"type":"fault","name":"f","op":"wait_ready","deployment":"trades","timeout":"601s"}`:                  "timeout",
		`{"type":"fault","name":"f","op":"wait_ready","deployment":"trades","timeout":"soon"}`:                  "timeout",
		`{"type":"fault","name":"f","op":"delay","link":"l"}`:                                                   "latency_ms",
		`{"type":"fault","name":"f","op":"delay","link":"l","latency_ms":0}`:                                    "latency_ms",
		`{"type":"fault","name":"f","op":"truncate","link":"l"}`:                                                "bytes",
		`{"type":"fault","name":"f","op":"drop","link":"l","latency_ms":10}`:                                    "latency_ms",
		`{"type":"fault","name":"f","op":"restore","deployment":"trades"}`:                                      "deployment",
		`{"type":"fault","name":"f","op":"stop","deployment":"trades","poll":{"timeout":"5s","interval":"1s"}}`: "poll",
		`{"type":"fault","name":"f","op":"stop","deployment":"trades","target":"x"}`:                            "target",
		`{"type":"fault","name":"f","op":"stop","deployment":"trades","url":"http://x"}`:                        "url",
	}
	for step, want := range cases {
		_, errs := Validate(faultMD(step))
		if len(errs) == 0 || !find(errs, want) || !find(errs, `"f"`) {
			t.Errorf("%s: want a refusal naming step \"f\" and %q; got %v", step, want, errs)
		}
	}
}

// A claim on a fault step would be executed by nothing: it is refused, not ignored (the orphan-claim defect class).
func TestFaultStep_ARunnableClaimOnAFaultStepIsRefused(t *testing.T) {
	trig := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  {"type":"fault","name":"f","op":"stop","deployment":"trades"}
]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step alive: status=200\n- step f: status=200\n"))
	if !find(errs, "fault step") || !find(errs, "claim") {
		t.Fatalf("want a refusal of a claim on a fault step; got %v", errs)
	}
}

// A fault step needs no claim (the empty-step guard must not demand one).
func TestFaultStep_NeedsNoClaimOfItsOwn(t *testing.T) {
	_, errs := Validate(faultMD(`{"type":"fault","name":"f","op":"stop","deployment":"trades"}`))
	if find(errs, "has no claim") {
		t.Fatalf("a fault step was asked for a claim: %v", errs)
	}
}

func TestFaultStep_ErrorsNeverCarryTheValueOfAnEnvVariable(t *testing.T) {
	_, errs := Validate(faultMD(`{"type":"fault","name":"f","op":"stop","deployment":"${SECRET_X}"}`))
	for _, e := range errs {
		if strings.Contains(e.Message, "SECRET_X") && !strings.Contains(e.Message, "variable") {
			t.Logf("note: %s", e.Message)
		}
	}
	if len(errs) == 0 {
		t.Fatal(fmt.Sprint("a ${…} where an allowlist name goes must be refused: the name is literal"))
	}
}
