package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// moneyallow_cleanup_ui_fault_test.go — the `cleanup`, `ui` and `fault` entry kinds, through
// the ONE function every door calls, MoneyGuardViolations.

const (
	uiBase    = "it is a UI scenario — a browser drive can submit anything the page offers, so against a money-handling system every scenario must be a plain HTTP GET"
	cleanBase = "a cleanup exists to write, and a money-handling system may only be read. Declare `N/A` instead"
)

func uiScenario(payload, trigURL string) *Scenario {
	return &Scenario{ID: "U", Tags: []string{UITag}, Cleanup: naCleanup,
		Trigger: Trigger{Method: "POST", URL: trigURL, Payload: payload}}
}

func cleanupOf(forms ...CleanupForm) Cleanup {
	var c Cleanup
	for _, f := range forms {
		c.Blocks = append(c.Blocks, CleanupBlock{Form: f, Body: "DELETE x"})
	}
	return c
}

func cleanupScenarioOf(forms ...CleanupForm) *Scenario {
	s := httpScenario("GET", "https://x.example/health", cleanupOf(forms...))
	return s
}

// ── ui scenario ───────────────────────────────────────────────────────────────────────────────────

func TestUIEntry_ScenarioMatcherTable(t *testing.T) {
	const spec = "tests/live/argus-declared.spec.ts"
	al := MoneyWriteAllowlist{{Engine: "ui", Spec: spec}}
	for _, tc := range []struct {
		name string
		s    *Scenario
		al   MoneyWriteAllowlist
		want bool
	}{
		{"spec in the payload", uiScenario(`{"spec":"`+spec+`"}`, ""), al, true},
		{"spec from the TRIGGER url, backticks trimmed", uiScenario("", "`"+spec+"`"), al, true},
		{"spec from the TRIGGER url, bare", uiScenario("", spec), al, true},
		{"the payload spec wins over the url (the driver reads it first)", uiScenario(`{"spec":"`+spec+`"}`, "`tests/live/other.spec.ts`"), al, true},
		{"payload names another spec, url names the entry's", uiScenario(`{"spec":"tests/live/other.spec.ts"}`, "`"+spec+"`"), al, false},
		{"another spec", uiScenario(`{"spec":"tests/live/other.spec.ts"}`, ""), al, false},
		{"a prefix of the entry's spec", uiScenario(`{"spec":"tests/live/argus-declared.spec"}`, ""), al, false},
		{"a longer spec", uiScenario(`{"spec":"`+spec+`x"}`, ""), al, false},
		{"case differs", uiScenario(`{"spec":"Tests/live/argus-declared.spec.ts"}`, ""), al, false},
		{"a leading ./ is not the same string", uiScenario(`{"spec":"./`+spec+`"}`, ""), al, false},
		{"no spec named at all", uiScenario("", ""), al, false},
		{"a spec built at run time", uiScenario(`{"spec":"tests/live/${SPEC}.spec.ts"}`, ""), MoneyWriteAllowlist{{Engine: "ui", Spec: "tests/live/${SPEC}.spec.ts"}}, false},
		{"a spec built at run time from the url", uiScenario("", "`${SPEC}`"), MoneyWriteAllowlist{{Engine: "ui", Spec: "${SPEC}"}}, false},
		{"a payload that is not JSON", uiScenario(`not json`, "`"+spec+"`"), al, false},
		{"no list", uiScenario(`{"spec":"`+spec+`"}`, ""), nil, false},
		{"only an entry of another kind", uiScenario(`{"spec":"`+spec+`"}`, ""), MoneyWriteAllowlist{{Engine: "fault"}, {Engine: "cleanup", Form: "sql"}}, false},
		{"an entry carrying a limit field matches nothing", uiScenario(`{"spec":"`+spec+`"}`, ""), MoneyWriteAllowlist{{Engine: "ui", Spec: spec, Spends: true}}, false},
		{"an entry carrying a max_per_run matches nothing", uiScenario(`{"spec":"`+spec+`"}`, ""), MoneyWriteAllowlist{{Engine: "ui", Spec: spec, MaxPerRun: 1}}, false},
		{"an entry carrying a foreign field matches nothing", uiScenario(`{"spec":"`+spec+`"}`, ""), MoneyWriteAllowlist{{Engine: "ui", Spec: spec, Tool: "t"}}, false},
		{"an entry carrying a form matches nothing", uiScenario(`{"spec":"`+spec+`"}`, ""), MoneyWriteAllowlist{{Engine: "ui", Spec: spec, Form: "sql"}}, false},
		{"an entry with no spec matches nothing", uiScenario("", "`"+spec+"`"), MoneyWriteAllowlist{{Engine: "ui"}}, false},
	} {
		v := MoneyGuardViolations(tc.s, tc.al)
		if (len(v) == 0) != tc.want {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.want, v)
		}
		if !tc.want && (len(v) != 1 || !strings.HasPrefix(v[0], uiBase)) {
			t.Errorf("%s: the base sentence must lead the refusal: %v", tc.name, v)
		}
	}
}

func TestUIEntry_RefusalNamesTheEntryTheOwnerCouldAdd(t *testing.T) {
	v := MoneyGuardViolations(uiScenario(`{"spec":"tests/live/x.spec.ts"}`, ""), nil)
	if len(v) != 1 || !strings.HasPrefix(v[0], uiBase) {
		t.Fatalf("want the base sentence first: %v", v)
	}
	for _, want := range []string{"`engine: ui, spec: \"tests/live/x.spec.ts\"`", "whatever that spec does in the browser"} {
		if !strings.Contains(v[0], want) {
			t.Errorf("the hint must contain %q: %s", want, v[0])
		}
	}
}

func TestUIEntry_ChainStep(t *testing.T) {
	const spec = "tests/live/argus-declared.spec.ts"
	al := MoneyWriteAllowlist{{Engine: "ui", Spec: spec}}
	step := func(spec string) string { return `{"type":"ui","name":"u","spec":"` + spec + `"}` }
	for _, tc := range []struct {
		name string
		s    *Scenario
		al   MoneyWriteAllowlist
		want bool
	}{
		{"the entry's spec", chainOf(step(spec)), al, true},
		{"another spec", chainOf(step("tests/live/other.spec.ts")), al, false},
		{"a spec built at run time", chainOf(step("tests/${X}.spec.ts")), MoneyWriteAllowlist{{Engine: "ui", Spec: "tests/${X}.spec.ts"}}, false},
		{"no spec", chainOf(`{"type":"ui","name":"u"}`), al, false},
		{"no list", chainOf(step(spec)), nil, false},
		{"a ui entry does not allow a fault step", chainOf(`{"type":"fault","name":"f","op":"stop","deployment":"d"}`), al, false},
	} {
		v := MoneyGuardViolations(tc.s, tc.al)
		if (len(v) == 0) != tc.want {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.want, v)
		}
		if !tc.want && (len(v) != 1 || !strings.HasPrefix(v[0], `chain step `)) {
			t.Errorf("%s: want one chain-step sentence: %v", tc.name, v)
		}
	}
	// without the entry: the old sentence leads, a hint follows
	v := MoneyGuardViolations(chainOf(step(spec)), nil)
	want := `chain step u is a "ui" step — against a money-handling system only GET http steps may run`
	if len(v) != 1 || !strings.HasPrefix(v[0], want) || !strings.Contains(v[0], "`engine: ui, spec: \""+spec+"\"`") {
		t.Errorf("the base sentence then the hint: %v", v)
	}
	// a chain that mixes an allowed ui step with an unlisted one refuses only the unlisted
	mixed := chainOf(step(spec), `{"type":"ui","name":"v","spec":"tests/live/other.spec.ts"}`)
	if v := MoneyGuardViolations(mixed, al); len(v) != 1 || !strings.HasPrefix(v[0], `chain step v is a "ui" step`) {
		t.Errorf("want only the unlisted ui step refused: %v", v)
	}
}

// ── fault ─────────────────────────────────────────────────────────────────────────────────────────

func TestFaultEntry_ChainStep(t *testing.T) {
	fs := `{"type":"fault","name":"cut","op":"stop","deployment":"trades"}`
	for _, tc := range []struct {
		name string
		al   MoneyWriteAllowlist
		want bool
	}{
		{"the entry", MoneyWriteAllowlist{{Engine: "fault"}}, true},
		{"no list", nil, false},
		{"entries of other kinds", MoneyWriteAllowlist{{Engine: "ui", Spec: "x"}, {Engine: "logs"}, {Engine: "db"}}, false},
		{"an entry carrying a limit field", MoneyWriteAllowlist{{Engine: "fault", Spends: true}}, false},
		{"an entry carrying a target", MoneyWriteAllowlist{{Engine: "fault", Target: "trades"}}, false},
		{"an entry carrying a spec", MoneyWriteAllowlist{{Engine: "fault", Spec: "x"}}, false},
		{"an entry carrying an http method", MoneyWriteAllowlist{{Engine: "fault", Method: "GET"}}, false},
	} {
		v := MoneyGuardViolations(chainOf(fs), tc.al)
		if (len(v) == 0) != tc.want {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.want, v)
		}
	}
	v := MoneyGuardViolations(chainOf(fs), nil)
	base := `chain step cut is a "fault" step — against a money-handling system only GET http steps may run`
	if len(v) != 1 || !strings.HasPrefix(v[0], base) || !strings.Contains(v[0], "`engine: fault`") ||
		!strings.Contains(v[0], "fault_allowed_targets") {
		t.Errorf("the base sentence, then a hint naming the entry and the list that still bounds it: %v", v)
	}
	// a fault entry covers every fault op: it names no target (fault_allowed_targets does)
	for _, op := range []string{"stop", "start", "restore", "delay", "drop", "truncate"} {
		st := `{"type":"fault","name":"f","op":"` + op + `"}`
		if v := MoneyGuardViolations(chainOf(st), MoneyWriteAllowlist{{Engine: "fault"}}); len(v) != 0 {
			t.Errorf("op %s was refused by a fault entry: %v", op, v)
		}
	}
}

// ── cleanup ───────────────────────────────────────────────────────────────────────────────────────

func TestCleanupEntry_FormTable(t *testing.T) {
	sql := MoneyWriteAllow{Engine: "cleanup", Form: "sql"}
	bash := MoneyWriteAllow{Engine: "cleanup", Form: "bash"}
	for _, tc := range []struct {
		name  string
		s     *Scenario
		al    MoneyWriteAllowlist
		wantN int // violations
	}{
		{"a sql block, the sql entry", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{sql}, 0},
		{"a bash block, the bash entry", cleanupScenarioOf(CleanupBash), MoneyWriteAllowlist{bash}, 0},
		{"a sql block, only the bash entry", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{bash}, 1},
		{"a bash block, only the sql entry", cleanupScenarioOf(CleanupBash), MoneyWriteAllowlist{sql}, 1},
		{"both forms, both entries", cleanupScenarioOf(CleanupSQL, CleanupBash), MoneyWriteAllowlist{sql, bash}, 0},
		{"both forms, one entry", cleanupScenarioOf(CleanupSQL, CleanupBash), MoneyWriteAllowlist{sql}, 1},
		{"both forms, no entry", cleanupScenarioOf(CleanupSQL, CleanupBash), nil, 2},
		{"no list", cleanupScenarioOf(CleanupSQL), nil, 1},
		{"N/A needs no entry", httpScenario("GET", "https://x.example/health", naCleanup), nil, 0},
		{"an unknown form in the entry", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup", Form: "python"}}, 1},
		{"an entry with no form", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup"}}, 1},
		{"an entry carrying a spend field", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup", Form: "sql", Spends: true}}, 1},
		{"an entry carrying a limit field", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup", Form: "sql", MaxAmount: 5}}, 1},
		{"an entry carrying a foreign field", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup", Form: "sql", Queue: "q"}}, 1},
		{"an entry carrying a spec", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "cleanup", Form: "sql", Spec: "x"}}, 1},
		{"an entry of another kind", cleanupScenarioOf(CleanupSQL), MoneyWriteAllowlist{{Engine: "db"}, {Engine: "fault"}}, 1},
	} {
		v := MoneyGuardViolations(tc.s, tc.al)
		if len(v) != tc.wantN {
			t.Errorf("%s: %d violations, want %d: %v", tc.name, len(v), tc.wantN, v)
		}
		for _, line := range v {
			if !strings.Contains(line, cleanBase) {
				t.Errorf("%s: the base sentence must be inside the refusal: %q", tc.name, line)
			}
		}
	}
}

func TestCleanupEntry_RefusalKeepsTheOldSentenceAsItsPrefixAndNamesTheEntry(t *testing.T) {
	v := MoneyGuardViolations(cleanupScenarioOf(CleanupBash), nil)
	old := "its ## CLEANUP runs a bash block — a cleanup exists to write, and a money-handling system may only be read. Declare `N/A` instead"
	if len(v) != 1 || !strings.HasPrefix(v[0], old) {
		t.Fatalf("the old sentence must lead: %v", v)
	}
	for _, want := range []string{"`engine: cleanup, form: bash`", "whatever a bash block does"} {
		if !strings.Contains(v[0], want) {
			t.Errorf("the hint must contain %q: %s", want, v[0])
		}
	}
}

func TestCleanupEntry_OnlyForTheCleanupItNames(t *testing.T) {
	// a cleanup entry never allows a different kind of step or scenario
	al := MoneyWriteAllowlist{{Engine: "cleanup", Form: "sql"}, {Engine: "cleanup", Form: "bash"}}
	if v := MoneyGuardViolations(chainOf(`{"type":"fault","name":"f","op":"stop","deployment":"d"}`), al); len(v) == 0 {
		t.Error("a cleanup entry allowed a fault step")
	}
	if v := MoneyGuardViolations(uiScenario(`{"spec":"x.spec.ts"}`, ""), al); len(v) == 0 {
		t.Error("a cleanup entry allowed a ui scenario")
	}
	if v := MoneyGuardViolations(httpScenario("POST", "https://x.example/api/v1/strategies", naCleanup), al); len(v) == 0 {
		t.Error("a cleanup entry allowed an HTTP POST")
	}
}

// ── wire ──────────────────────────────────────────────────────────────────────────────────────────

func TestNewKinds_AreOmitemptyOnTheWire(t *testing.T) {
	// an entry of a new kind serialises with its own fields only; the HTTP half is the pre-existing
	// method/path/spends triple, which an older control plane decodes as an entry it can never match.
	raw, err := json.Marshal(MoneyWriteAllowlist{
		{Engine: "cleanup", Form: "sql"}, {Engine: "ui", Spec: "a.spec.ts"}, {Engine: "fault"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"method":"","path":"","spends":false,"engine":"cleanup","form":"sql"},` +
		`{"method":"","path":"","spends":false,"engine":"ui","spec":"a.spec.ts"},` +
		`{"method":"","path":"","spends":false,"engine":"fault"}]`
	if string(raw) != want {
		t.Errorf("wire form changed:\n got %s\nwant %s", raw, want)
	}
}

func TestNewKinds_NeverExemptAnHTTPRequest(t *testing.T) {
	var al MoneyWriteAllowlist
	raw := `[{"engine":"cleanup","form":"sql","method":"POST","path":"/api/v1/order"},
	         {"engine":"ui","spec":"x.spec.ts","method":"POST","path":"/api/v1/order"},
	         {"engine":"fault","method":"POST","path":"/api/v1/order"},
	         {"method":"POST","path":"/api/v1/order","form":"sql"},
	         {"method":"POST","path":"/api/v1/order","spec":"x"}]`
	if err := json.Unmarshal([]byte(raw), &al); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"POST", "GET", ""} {
		if e := al.Match(m, "/api/v1/order"); e != nil {
			t.Errorf("method %q: an entry of a new kind exempted an HTTP request: %+v", m, e)
		}
	}
	// and a new-kind entry with HTTP fields beside it matches nothing of its own kind either
	if al.MatchCleanup("sql") != nil || al.MatchUI("x.spec.ts") != nil || al.MatchFault() != nil {
		t.Error("an entry carrying HTTP fields matched its own kind")
	}
}
