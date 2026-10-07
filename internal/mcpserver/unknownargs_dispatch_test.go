package mcpserver

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// VR10-S1 (V28-015): a tools/call carrying a key the tool does not declare is refused in full,
// BEFORE PreCheck, before the run lock, before a run id exists and before injectRunID — as a TOOL
// error that names the unknown key, lists the accepted ones with the required marked, and suggests
// the nearest match without ever substituting it.

// closedSchema is a tool schema that declares its arguments and CLOSES the object, the way both
// plane builders do after VR10-S1.
func closedSchema(required []string, keys ...string) map[string]any {
	props := map[string]any{}
	for _, k := range keys {
		props[k] = map[string]any{"type": "string", "description": k}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// spyTool counts how often the dispatcher reached its PreCheck and its Handler.
type spyTool struct {
	preChecks int
	handled   int
}

func (sp *spyTool) tool(name string, ns Namespace, async bool, schema map[string]any) Tool {
	return Tool{Name: name, Namespace: ns, Description: "spy", InputSchema: schema, Async: async,
		PreCheck: func(_ json.RawMessage, _ Principal) *Outcome { sp.preChecks++; return nil },
		Handler:  func(_ json.RawMessage, _ Principal) Outcome { sp.handled++; return Ok(map[string]any{"ran": true}) },
	}
}

func dispatchCall(t *testing.T, s *Server, token, name string, args map[string]any) (Response, string) {
	t.Helper()
	raw, _ := s.Dispatch("s", token, reqBytes(7, "tools/call", map[string]any{"name": name, "arguments": args}))
	return decode(t, raw), string(raw)
}

// toolResultOf re-reads a Response as the tools/call result shape and returns its concatenated text.
func toolResultOf(t *testing.T, r Response) (ToolResult, string) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("VR10-S1-3: expected a TOOL result (result.isError), got a protocol error %d: %s", r.Error.Code, r.Error.Message)
	}
	b, _ := json.Marshal(r.Result)
	var tr ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("not a ToolResult: %s", b)
	}
	var sb strings.Builder
	for _, c := range tr.Content {
		sb.WriteString(c.Text)
	}
	return tr, sb.String()
}

// TestToolsCall_UnknownArgument_RefusedBeforePreCheckAndRunID — the spy PreCheck proves the ORDER
// (VR10-S1-1, -2, -3, -4, -5): the refusal happens before PreCheck, no run is announced, no run id
// is minted, the CP fence is not taken, the local lock does not leak.
func TestToolsCall_UnknownArgument_RefusedBeforePreCheckAndRunID(t *testing.T) {
	sp := &spyTool{}
	run := sp.tool("runner__run", NSRunner, true, closedSchema([]string{"instance_id"}, "instance_id", "scenario_ref", "tag", "layer"))
	s := newSrv(t, run)
	handshake(t, s, "s", rtok)
	fenced := false
	s.RunBeginner = func(runID, scope string) error { fenced = true; return nil }

	r, got := dispatchCall(t, s, rtok, "runner__run", map[string]any{"instance_id": "local", "scenaro_ref": "ORDE-017"})
	tr, text := toolResultOf(t, r)
	if !tr.IsError {
		t.Fatalf("a call with an undeclared key must be refused (result.isError:true); got: %s", got)
	}
	for _, want := range []string{"scenaro_ref", "scenario_ref", "instance_id (required)", "tag", "layer"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must carry %q (unknown named / accepted listed / required marked / nearest suggested); got: %s", want, text)
		}
	}
	if sp.preChecks != 0 {
		t.Errorf("spy PreCheck ran %d time(s) — the unknown-argument refusal must come BEFORE PreCheck", sp.preChecks)
	}
	if sp.handled != 0 {
		t.Errorf("the handler ran %d time(s) for a refused call — a suggestion must never become an acceptance", sp.handled)
	}
	if strings.Contains(got, `"status":"running"`) || strings.Contains(got, `"run_id"`) {
		t.Errorf("a refused call must NOT announce a run or hand out a run_id; got: %s", got)
	}
	if fenced {
		t.Error("the CP run fence was acquired for a call that was refused")
	}
	if s.runActive {
		t.Error("the local run lock leaked on a refused call")
	}
}

// TestToolsCall_UnderscoreKeysPassThrough_UnknownKeyStopsTheHandler — D2 (`_meta` and friends are
// another program's notes, not ours) and D3 (a valid call plus one unknown key is refused in full).
func TestToolsCall_UnderscoreKeysPassThrough_UnknownKeyStopsTheHandler(t *testing.T) {
	sp := &spyTool{}
	ping := sp.tool("runner__ping", NSRunner, false, closedSchema([]string{"instance_id"}, "instance_id"))
	s := newSrv(t, ping)
	handshake(t, s, "s", rtok)

	r, got := dispatchCall(t, s, rtok, "runner__ping", map[string]any{"instance_id": "local", "_meta": map[string]any{"progressToken": 1}})
	if tr, _ := toolResultOf(t, r); tr.IsError || sp.handled != 1 {
		t.Fatalf("a `_`-prefixed key is another client's annotation and must pass through (handled=%d); got: %s", sp.handled, got)
	}

	r, got = dispatchCall(t, s, rtok, "runner__ping", map[string]any{"instance_id": "local", "junk": "x"})
	tr, text := toolResultOf(t, r)
	if !tr.IsError || sp.handled != 1 {
		t.Fatalf("a valid call plus one unknown key must be refused in full (handled=%d); got: %s", sp.handled, got)
	}
	if !strings.Contains(text, "junk") || !strings.Contains(text, "instance_id (required)") {
		t.Errorf("the refusal must name the unknown key and list the accepted ones; got: %s", text)
	}
	if strings.Contains(text, "did you mean") {
		t.Errorf("no accepted key is within Levenshtein 2 of %q — a wrong hint is worse than none; got: %s", "junk", text)
	}
}

// TestToolsCall_UnknownArgument_ScopeRefusalComesFirst — order constraint 1: an unauthorised caller
// must not learn a tool's argument list from the refusal.
func TestToolsCall_UnknownArgument_ScopeRefusalComesFirst(t *testing.T) {
	sp := &spyTool{}
	secret := sp.tool("author__secret", NSAuthor, false, closedSchema([]string{"instance_id"}, "instance_id", "secret_knob_name"))
	s := newSrv(t, secret)
	handshake(t, s, "s", rtok) // the RUNNER hat
	raw, _ := s.Dispatch("s", rtok, reqBytes(7, "tools/call", map[string]any{"name": "author__secret", "arguments": map[string]any{"instance_id": "local", "junk": "x"}}))
	r := decode(t, raw)
	if r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("the scope refusal must come first; got: %s", raw)
	}
	if strings.Contains(string(raw), "secret_knob_name") || strings.Contains(string(raw), "junk") {
		t.Errorf("an unauthorised caller learned the argument list: %s", raw)
	}
}

// TestRunnerRun_CallerSuppliedRunIDIsRefused — D4 / VR10-S1-6: run_id is server-internal on
// runner__run. A caller who sends it is refused like any other undeclared key, and the check reads the
// CALLER's bytes (before injectRunID), so the server never rejects its own argument.
func TestRunnerRun_CallerSuppliedRunIDIsRefused(t *testing.T) {
	tmp := t.TempDir()
	env := toolcore.Env{Instance: "local", Grafana: "http://localhost:3000", ResultsRoot: tmp,
		ScenariosDir: tmp, ConfigPath: filepath.Join(tmp, "no-such-argus-config.yaml")}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", rtok)
	fenced := false
	s.RunBeginner = func(runID, scope string) error { fenced = true; return nil }
	s.RunPreflight = func(layer, tag, scenarioID string) error { return nil }

	r, got := dispatchCall(t, s, rtok, "runner__run", map[string]any{"instance_id": "local", "run_id": "20260101T000000000"})
	tr, text := toolResultOf(t, r)
	if !tr.IsError || !strings.Contains(text, `run_id`) {
		t.Fatalf("run_id from the caller must be refused by name on runner__run; got: %s", got)
	}
	if strings.Contains(got, `"status":"running"`) {
		t.Errorf("a refused runner__run must not announce a run; got: %s", got)
	}
	if fenced || s.runActive {
		t.Errorf("a refused runner__run must cost nothing (fenced=%v runActive=%v)", fenced, s.runActive)
	}
}

// TestGetReport_UnknownScenarioID_RefusedNotNarrowed — acceptance 1..3, the row's own reproduction:
// runner__get_report with an undeclared scenario_id used to return the WHOLE run (a plausible, often
// green answer to a question nobody asked). Now: refused, naming scenario_id and listing run_id; the
// legitimate calls still work.
func TestGetReport_UnknownScenarioID_RefusedNotNarrowed(t *testing.T) {
	env := writeFixtureReport(t)
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", atok)

	r, got := dispatchCall(t, s, atok, "runner__get_report", map[string]any{"instance_id": "local", "scenario_id": "ORD-X"})
	tr, text := toolResultOf(t, r)
	if !tr.IsError {
		t.Fatalf("POSITIVE CONTROL: get_report{instance_id, scenario_id} returned a report instead of a refusal — the whole run, unfiltered: %s", got)
	}
	for _, want := range []string{"scenario_id", "run_id", "instance_id (required)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must carry %q; got: %s", want, text)
		}
	}

	r, got = dispatchCall(t, s, atok, "runner__get_report", map[string]any{"instance_id": "local"})
	if tr, text := toolResultOf(t, r); tr.IsError || !strings.Contains(text, "ORD-X") {
		t.Errorf("get_report{instance_id} must still return the last run's report; got: %s", got)
	}
	r, got = dispatchCall(t, s, atok, "runner__get_report", map[string]any{"instance_id": "local", "run_id": "20260101T000000000"})
	if _, text := toolResultOf(t, r); strings.Contains(text, "unknown argument") {
		t.Errorf("get_report{instance_id, run_id} is a declared shape and must not be refused as unknown; got: %s", got)
	}
}

// quotedLocal matches a description that tells an agent to pass `local` as the instance id
// (acceptance 10, shared with V28-017): the value quoted in any of the three usual ways.
var quotedLocal = regexp.MustCompile(`["'` + "`" + `]local["'` + "`" + `]`)

// TestToolsList_RunnerPlane_ClosedSchemasAndDescribedArguments — acceptance 5, 9, 10 and §0.10 on the
// runner plane: every tool closes its object; no property is object-typed (only top-level keys are
// policed, and this makes the limit visible the day a nested object appears); every argument says what
// it is, whether it is required, and what happens when it is omitted; nothing says `local`.
func TestToolsList_RunnerPlane_ClosedSchemasAndDescribedArguments(t *testing.T) {
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(toolcore.Env{Instance: "local"})...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", atok)
	raw, _ := s.Dispatch("s", atok, reqBytes(3, "tools/list", map[string]any{}))
	r := decode(t, raw)
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	b, _ := json.Marshal(r.Result)
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 6 {
		t.Fatalf("want the 6 runner tools, got %d", len(res.Tools))
	}
	for _, tl := range res.Tools {
		assertClosedAndDescribed(t, tl.Name, tl.Description, tl.InputSchema)
		if quotedLocal.MatchString(tl.Description) {
			t.Errorf("%s: the tool description tells an agent to use `local`: %q", tl.Name, tl.Description)
		}
	}
}

// assertClosedAndDescribed is the per-tool contract check shared by both planes' tools/list tests.
func assertClosedAndDescribed(t *testing.T, name, desc string, schema map[string]any) {
	t.Helper()
	if ap, ok := schema["additionalProperties"]; !ok || ap != false {
		t.Errorf("%s: inputSchema must carry additionalProperties:false (acceptance 5); got %v", name, schema["additionalProperties"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["instance_id"]; !ok {
		t.Errorf("%s: instance_id must stay declared", name)
	}
	for key, raw := range props {
		p, _ := raw.(map[string]any)
		if p["type"] == "object" || p["properties"] != nil {
			t.Errorf("%s.%s: an object-typed property — only top-level keys are policed in this build (§0.10); declare the limit before nesting", name, key)
		}
		d, _ := p["description"].(string)
		low := strings.ToLower(d)
		if d == "" || !(strings.Contains(low, "required") || strings.Contains(low, "optional")) || !strings.Contains(low, "omit") {
			t.Errorf("%s.%s: the description must say what it is, required or optional, and what happens when omitted (VR10-S1-9); got %q", name, key, d)
		}
		if quotedLocal.MatchString(d) {
			t.Errorf("%s.%s: the description tells an agent to use `local`: %q", name, key, d)
		}
	}
}
