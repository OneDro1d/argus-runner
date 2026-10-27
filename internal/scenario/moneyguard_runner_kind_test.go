package scenario

import (
	"strings"
	"testing"
)

// The guard judges the scenario AS the runner the dispatch will give it (RunnerKindOf).

func kindMD(layer, tags, trigger, payload string) string {
	l := []string{"# Scenario: k", "", "## Metadata", "- **ID**: KND-001", "- **Layer**: " + layer, "- **Tags**: " + tags, ""}
	l = append(l, "## TRIGGER", trigger)
	if payload != "" {
		l = append(l, "", "```json", payload, "```")
	}
	l = append(l, "", "## EXPECT", "### Runnable", "- step s: status=200", "", "## TIMEOUT", "30s", "", "## CLEANUP", "N/A — probe.", "")
	return strings.Join(l, "\n")
}

const kindChainPost = `{"steps":[{"type":"http","name":"s","method":"POST","url":"http://x.invalid/api/v1/orders/transfer"}]}`

func TestRunnerKindOf_FollowsTheDispatchOrder(t *testing.T) {
	for _, tc := range []struct {
		layer, tags string
		want        RunnerKind
	}{
		{"Permissions", "http", RunnerPlain},
		{"Permissions", "chain", RunnerChain},
		{"Permissions", "mcp", RunnerMCP},
		{"Permissions", "ui", RunnerUI},
		{"Permissions", "mcp, chain", RunnerChain},
		{"Permissions", "ui, chain", RunnerChain},
		{"Permissions", "mcp, ui", RunnerMCP},
		{"Permissions", "mcp, ui, chain", RunnerChain},
		{"HTTP Load", "chain", RunnerHTTPLoad},
		{"HTTP Load", "ui", RunnerHTTPLoad},
		{"AMQP Load", "mcp", RunnerAMQPLoad},
		{"AMQP Load", "mcp, ui, chain", RunnerAMQPLoad},
	} {
		s := Parse(kindMD(tc.layer, tc.tags, "GET `/x`", ""))
		if got := RunnerKindOf(s); got != tc.want {
			t.Errorf("%s | %s: RunnerKindOf = %q, want %q", tc.layer, tc.tags, got, tc.want)
		}
	}
	if RunnerKindOf(nil) != RunnerPlain {
		t.Error("a nil scenario is plain")
	}
}

// The reviewer's probe on the branch head: `ui, chain`, the list holds only the ui entry.
func TestMoneyGuard_UIChain_IsJudgedAsAChain_EveryStepIsJudged(t *testing.T) {
	allow := MoneyWriteAllowlist{{Engine: "ui", Path: "tests/live/argus-declared.spec.ts"}}
	s := Parse(kindMD("Permissions", "ui, chain", "POST `tests/live/argus-declared.spec.ts`", kindChainPost))
	kind, v := MoneyGuardJudgement(s, allow)
	if kind != RunnerChain {
		t.Fatalf("judged as %q, want chain", kind)
	}
	if len(v) == 0 || !strings.Contains(strings.Join(v, ";"), "chain step s is POST") {
		t.Errorf("the POST step must be refused: %v", v)
	}
}

func TestMoneyGuard_MCPChain_IsJudgedAsAChain(t *testing.T) {
	allow := MoneyWriteAllowlist{{Engine: "mcp", Tool: "t"}}
	payload := `{"tool":"t","steps":[{"type":"http","name":"s","method":"POST","url":"http://x.invalid/api/v1/orders/transfer"}]}`
	s := Parse(kindMD("Permissions", "mcp, chain", "", payload))
	if kind, v := MoneyGuardJudgement(s, allow); kind != RunnerChain || len(v) == 0 {
		t.Errorf("judged %q with violations %v: the POST step must be refused", kind, v)
	}
}

func TestMoneyGuard_LoadLayers_AreJudgedAsWhatTheLoadRunnerFires(t *testing.T) {
	spend := MoneyWriteAllow{Method: "POST", Path: "/api/v1/strategies", Spends: true, AmountField: "amount", MaxAmount: 10, MaxPerRun: 1}
	// AMQP Load: refused by name, whatever tag it carries and whatever the list holds.
	for _, tags := range []string{"http, load", "mcp", "ui", "chain", "mcp, ui, chain"} {
		s := Parse(kindMD("AMQP Load", tags, "POST `/amqp-load`", ""))
		kind, v := MoneyGuardJudgement(s, MoneyWriteAllowlist{{Engine: "mcp", Tool: "t"}, {Engine: "ui", Path: "/amqp-load"}})
		if kind != RunnerAMQPLoad || !strings.Contains(strings.Join(v, ";"), "AMQP Load scenario") {
			t.Errorf("AMQP Load | %s: kind %q, violations %v", tags, kind, v)
		}
	}
	// HTTP Load: the TRIGGER is judged by the plain http rules, whatever tag it carries.
	for _, tags := range []string{"http, load", "mcp", "ui", "chain"} {
		if kind, v := MoneyGuardJudgement(Parse(kindMD("HTTP Load", tags, "GET `/api/items`", "")), nil); kind != RunnerHTTPLoad || len(v) != 0 {
			t.Errorf("HTTP Load GET | %s: kind %q, violations %v (a plain GET is allowed)", tags, kind, v)
		}
		if _, v := MoneyGuardJudgement(Parse(kindMD("HTTP Load", tags, "POST `/api/items`", "")), nil); len(v) == 0 {
			t.Errorf("HTTP Load POST | %s: an unlisted POST must be refused", tags)
		}
		// a chain of GETs on a POST ramp: the chain is not what runs
		getChain := `{"steps":[{"type":"http","name":"s","method":"GET","url":"http://x.invalid/health"}]}`
		if _, v := MoneyGuardJudgement(Parse(kindMD("HTTP Load", tags, "POST `/api/items`", getChain)), nil); len(v) == 0 {
			t.Errorf("HTTP Load POST with a GET-only chain payload | %s: the ramp's TRIGGER is what fires", tags)
		}
		if _, v := MoneyGuardJudgement(Parse(kindMD("HTTP Load", tags, "POST `/api/v1/strategies`", "")), MoneyWriteAllowlist{spend}); len(v) == 0 ||
			!strings.Contains(strings.Join(v, ";"), "HTTP Load ramp") {
			t.Errorf("HTTP Load spending entry | %s: load sends it many times, it must be refused: %v", tags, v)
		}
	}
	// a non-spending allowed write stays allowed on an HTTP Load ramp, as it is on a plain TRIGGER
	plainWrite := MoneyWriteAllow{Method: "POST", Path: "/api/v1/strategies"}
	if _, v := MoneyGuardJudgement(Parse(kindMD("HTTP Load", "http, load", "POST `/api/v1/strategies`", "")), MoneyWriteAllowlist{plainWrite}); len(v) != 0 {
		t.Errorf("a listed non-spending write must still pass on the ramp: %v", v)
	}
}

// A `ui, chain` file's spending POST escaped the amount/limit check (it skipped ui scenarios); it is a chain now.
func TestMoneyWriteAmountViolations_UIChain_IsCheckedAsAChain(t *testing.T) {
	spend := MoneyWriteAllow{Method: "POST", Path: "/api/v1/strategies", Spends: true, AmountField: "amount", MaxAmount: 10, MaxPerRun: 1}
	payload := `{"steps":[{"type":"http","name":"s","method":"POST","url":"http://x.invalid/api/v1/strategies","body":{"amount":500}}]}`
	for _, tags := range []string{"chain", "ui, chain", "mcp, chain"} {
		s := Parse(kindMD("Permissions", tags, "POST `chain`", payload))
		if v := MoneyWriteAmountViolations(s, MoneyWriteAllowlist{spend}); len(v) != 1 {
			t.Errorf("%s: the over-limit literal amount must be reported once: %v", tags, v)
		}
	}
}
