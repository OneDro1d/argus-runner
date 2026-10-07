package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR10-S2 (V28-014): parseExpectPlane fills all four mcp.Expect fields and REFUSES (CR-1) any
// bullet it cannot classify as a plane bullet, a `body has …` bullet, or prose. Before this
// build its default branch meant "expect success", so an unrecognised assertion vanished.

// ── parseExpectPlane ──────────────────────────────────────────────────────────────────────

// Acceptance 4 + 9: the parsed STRUCT, not the run's colour.
func TestParseExpectPlane_FillsBodyAssertsAlongsideThePlane(t *testing.T) {
	want, err := parseExpectPlane([]string{"result.isError == false", "body has marker containing xyz"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want.ErrorPlane != mcp.PlaneNone || firstContains(want) != "xyz" || firstMatches(want) != "" {
		t.Fatalf("want plane none + BodyContains xyz, got %+v", want)
	}
	m, err := parseExpectPlane([]string{"body has text matching ^ok"})
	if err != nil || firstMatches(m) != "^ok" {
		t.Fatalf("matching → BodyMatches, got %+v err=%v", m, err)
	}
	// ⚠ VR12-E8 R2 CHANGED THIS CONTRACT DELIBERATELY. S2-a used to read "bare `body has <f>` → the
	// TEXT <f> must appear", i.e. the field name was used as a substring — which is exactly why
	// `body has error containing X` and `body has anything containing X` were the same assertion.
	// The bare form now means THE FIELD EXISTS.
	bare, err := parseExpectPlane([]string{"body has id"})
	if err != nil || len(bare.Body) != 1 || bare.Body[0].Field != "id" || bare.Body[0].Op != mcp.BodyExistsOp {
		t.Fatalf("bare `body has id` must be an EXISTS on the field, got %+v err=%v", bare, err)
	}
}

// The plane phrases the parser matched before this build are unchanged (VR10-S2-13).
func TestParseExpectPlane_PlaneBulletsUnchanged(t *testing.T) {
	cases := map[string]struct {
		plane mcp.ErrPlane
		code  int
	}{
		"result.isError == false": {mcp.PlaneNone, 0},
		"result.isError == true":  {mcp.PlaneTool, 0},
		"jsonrpc error == -32602": {mcp.PlaneProtocol, -32602},
		"jsonrpc error — an unknown tool is rejected on the protocol plane (code-agnostic)": {mcp.PlaneProtocol, 0},
		"protocol error -32601": {mcp.PlaneProtocol, -32601},
		"error code -32601":     {mcp.PlaneProtocol, -32601},
	}
	for bullet, c := range cases {
		got, err := parseExpectPlane([]string{bullet})
		if err != nil {
			t.Errorf("%q: unexpected error %v", bullet, err)
			continue
		}
		if got.ErrorPlane != c.plane || got.ErrorCode != c.code {
			t.Errorf("%q → plane %q code %d; want %q %d", bullet, got.ErrorPlane, got.ErrorCode, c.plane, c.code)
		}
	}
}

// CR-1 / VR10-S2-6: an assertion-shaped bullet the parser does not understand is an ERROR that
// quotes the bullet and names the accepted forms — never silently read as "expect success".
func TestParseExpectPlane_UnclassifiableAssertionShapedBulletIsAnError(t *testing.T) {
	for _, b := range []string{
		`content[0].text == "document not found"`,
		"content[0].text matches `{\"error_code\":\"X\"}`",
		"result.count must be 1",
		"body has",
		`body has "results" contains x`,
	} {
		_, err := parseExpectPlane([]string{"result.isError == false", b})
		if err == nil {
			t.Errorf("%q must be refused, not silently read as expect-success", b)
			continue
		}
		if !strings.Contains(err.Error(), b) {
			t.Errorf("the error must quote the bullet %q, got: %v", b, err)
		}
		if !strings.Contains(err.Error(), "body has") {
			t.Errorf("the error must name the accepted forms, got: %v", err)
		}
	}
}

// The owner's lock: only bullets that LOOK like an assertion are policed; everything else is prose.
func TestParseExpectPlane_ProseStaysProse(t *testing.T) {
	for _, b := range []string{
		"all steps pass; one correlation id threaded across all steps",
		"the responder must answer within the timeout",
		"the library list contains the new library",
		"see References for the probe date",
	} {
		want, err := parseExpectPlane([]string{"result.isError == false", b})
		if err != nil {
			t.Errorf("prose %q must not be refused: %v", b, err)
			continue
		}
		if want.ErrorPlane != mcp.PlaneNone || firstContains(want) != "" || firstMatches(want) != "" {
			t.Errorf("prose %q must not change the expectation: %+v", b, want)
		}
	}
}

// VR10-S2-7 / acceptance 6: an uncompilable `matching` regex is an authoring error.
func TestParseExpectPlane_UncompilableMatchingRegexIsAnAuthoringError(t *testing.T) {
	_, err := parseExpectPlane([]string{"body has text matching ("})
	if err == nil || !strings.Contains(err.Error(), "regex") {
		t.Fatalf("a regex that does not compile must be an error naming the regex, got: %v", err)
	}
}

// ⛔ THE expectList TESTS STOOD HERE AND ARE DELETED BY V31-002 (R2).
//
// expectList read a chain step's legacy in-JSON `expect` — a string or a list, never split on `;`.
// V31-002 removes that key from the struct, from the runner and from the validate path, and refuses
// it BY NAME when a scenario is written, so the function has no caller and no subject. The rule it
// enforced lives on where the claims now are: a `;`-joined bullet is refused by the one classifier
// (TestGate2_WholeAnswerFormIsNeverSwallowedByAPlanePhrase, expect_gate2_test.go).

// Acceptance 4 + 9 on a CHAIN step: several claims reach ONE mcp.Expect with the plane and the body.
//
// ⛔ REWRITTEN BY V31-002 (R2). It used to build the step's in-JSON `expect` — a list, then a string —
// and pass nil claims, "the LEGACY in-JSON path, still supported at run time". That path is gone, so
// the same property is asserted where the claims actually live: one bullet per assertion under
// `### Runnable`, named by step.
func TestChainStepExpect_ListReachesTheExpectStruct(t *testing.T) {
	steps, err := parseChainSpec(`{"steps":[{"type":"mcp","name":"s","tool":"t","args":{}}]}`, "tr-1")
	if err != nil {
		t.Fatal(err)
	}
	want, err := chainStepExpect(steps[0], []string{"result.isError == false", "body has marker containing xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if want.ErrorPlane != mcp.PlaneNone || firstContains(want) != "xyz" {
		t.Fatalf("several claims must fill plane + BodyContains, got %+v", want)
	}
	if want, err := chainStepExpect(steps[0], []string{"result.isError == true"}); err != nil || want.ErrorPlane != mcp.PlaneTool {
		t.Fatalf("one claim → tool plane, got %+v err=%v", want, err)
	}
}

// ── run-time surfaces ─────────────────────────────────────────────────────────────────────

// fakeMCPTextServer is a conformant Streamable-HTTP server whose tools/call answers with the
// given content text (isError:false).
func fakeMCPTextServer(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			out := text
			if out == "" { // echo the arguments back as the content text
				b, _ := json.Marshal(req.Params.Arguments)
				out = string(b)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": out}}, "isError": false}})
		}
	}
}

func chainCfg(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+baseURL+"\n    transport: streamable-http\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// Acceptance 1 (single-call form) + VR10-S2-4/5 + owner lock D: a body miss is `failed` — never
// `errored` — with the reality-only observed, the bullet in failure.expected (test hat), and the
// call recorded as a NEGATIVE response.
func TestRunMCPScenario_BodyMissIsFailedNeverErrored(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer(`{"results":[]}`))
	defer ts.Close()
	md := mcpScenarioMD() + "- body has results containing argus-race\n"
	res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(md), "tr-body")
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed (never errored: a measurement WAS obtained): %+v", res.Status, res.Failure)
	}
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "argus-race") {
		t.Fatalf("observed must never echo the asserted value (VR-C8): %+v", res.Failure)
	}
	if res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "body has results containing argus-race") {
		t.Errorf("failure.expected (test hat) must carry the assertion text: %+v", res.Failure)
	}
	if res.ReqFailed != 1 || res.ReqError != 0 {
		t.Errorf("a body miss is a negative RESPONSE (lock D): want failed=1 error=0, got failed=%d error=%d success=%d", res.ReqFailed, res.ReqError, res.ReqSuccess)
	}
}

// Acceptance 2: the same assertion against an answer that carries the marker passes.
func TestRunMCPScenario_BodyHitPasses(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer(`{"results":[{"id":"argus-race-1"}]}`))
	defer ts.Close()
	md := mcpScenarioMD() + "- body has results containing argus-race\n"
	if res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(md), "tr-hit"); res.Status != "passed" {
		t.Fatalf("a body hit must pass: %+v", res.Failure)
	}
}

// S2-b (run time): an unclassifiable bullet fails the scenario at PREFLIGHT with the bullet
// quoted; no request is fired; the status is failed, not errored.
func TestRunMCPScenario_UnclassifiableBulletFailsAtPreflight(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer())
	defer ts.Close()
	bullet := `content[0].text == "document not found"`
	md := mcpScenarioMD() + "- " + bullet + "\n"
	res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(md), "tr-pre")
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed: %+v", res.Status, res.Failure)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, bullet) || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("observed must quote the bullet and say preflight: %+v", res.Failure)
	}
	if res.ReqSuccess+res.ReqFailed+res.ReqError != 0 {
		t.Errorf("a preflight refusal fires no request, got success=%d failed=%d error=%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// Acceptance 10 on the real path: the `;`-joined string is refused at chain preflight, naming the
// step and the list form; nothing is called.
func TestRunChainScenario_SemicolonExpectIsRefusedAtPreflight(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer())
	defer ts.Close()
	// V31-002: the claim lives in `### Runnable`, named by step — the removed in-JSON `expect` is
	// refused by name at authoring time and is not read here at all.
	s := &scenario.Scenario{ID: "CHAIN-SEMI", Tags: []string{"chain"},
		ExpectRunnable: []string{"step search: result.isError == false; body contains argus-race"}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"search","tool":"t","args":{}}]}`
	res := runChainScenario(chainCfg(t, ts.URL), s, "tr-semi", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("must be a preflight failure: %+v", res)
	}
	obs := res.Failure.Observed
	// ⚠ V31-002: it used to demand the word "list" — expectList's advice for a `;`-joined string in
	// the step's in-JSON `expect`. Through the one classifier the advice is one bullet per assertion.
	if !strings.Contains(obs, "search") || !strings.Contains(obs, "one bullet per assertion") || !strings.Contains(obs, "preflight") {
		t.Fatalf("observed must name the step, point at the one-bullet form and say preflight: %q", obs)
	}
	if res.ReqSuccess+res.ReqFailed+res.ReqError != 0 {
		t.Errorf("a preflight refusal fires no request, got %d/%d/%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// Acceptance 1 (the RACE-002 shape) + acceptance 7 (the ENFORCED marker): a chain step with a
// list expect whose body assertion misses goes RED, the step records what it enforced, and the
// test hat gets the assertion in failure.expected.
func TestRunChainScenario_BodyMissGoesRedAndRecordsEnforcement(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer(`{"results":[]}`))
	defer ts.Close()
	s := &scenario.Scenario{ID: "RACE-002", Tags: []string{"chain"}, ExpectRunnable: []string{
		"step global-search-marker: result.isError == false",
		"step global-search-marker: body has results containing argus-race",
	}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"global-search-marker","tool":"memstore_global_search","args":{"query":"x"}}]}`
	res := runChainScenario(chainCfg(t, ts.URL), s, "tr-race", "testkit/ui")
	if res.Status != "failed" || len(res.Steps) != 1 || res.Steps[0].Status != "failed" {
		t.Fatalf("the step with the wrong marker must go red (failed, not error): %+v", res)
	}
	if len(res.Steps[0].AssertionsEnforced) != 1 || res.Steps[0].AssertionsEnforcedCount != 1 {
		t.Errorf("the step must record the ONE enforced content assertion: %+v", res.Steps[0])
	}
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "argus-race") {
		t.Errorf("observed must never echo the asserted value (VR-C8): %+v", res.Failure)
	}
	if res.Failure == nil || res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "argus-race") {
		t.Errorf("failure.expected (test hat) must carry the assertion text: %+v", res.Failure)
	}
}

// Acceptance 11 (the ${cid} half): the assertion's variable is resolved before judging, and the
// enforced record carries the RESOLVED value, never the placeholder.
func TestRunChainScenario_CidBindsIntoTheAssertion(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer("")) // echoes the args as the content text
	defer ts.Close()
	// ⚠ V31-002 moved this off the removed in-JSON `expect`. The property it pins is unchanged and
	// still load-bearing: V31-003 is what gives a `## EXPECT` claim the correlation id, so this and
	// TestChainRun_CorrelationIDInAClaimIsFilledIn now guard the same path from two directions.
	s := &scenario.Scenario{ID: "CHAIN-CID", Tags: []string{"chain"}, ExpectRunnable: []string{
		"step write: result.isError == false",
		"step write: body has name containing argus-race-${cid}",
	}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"write","tool":"t","args":{"name":"argus-race-${cid}"}}]}`
	res := runChainScenario(chainCfg(t, ts.URL), s, "tr-cid-77", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("the resolved marker is in the echoed answer — must pass: %+v", res.Failure)
	}
	if got := strings.Join(res.Steps[0].AssertionsEnforced, ";"); !strings.Contains(got, "argus-race-tr-cid-77") || strings.Contains(got, "${cid}") {
		t.Errorf("the enforced record must carry the resolved value, got %q", got)
	}
}

// ── the validate-time surface both validators call (VR10-S2-8) ────────────────────────────

func TestExpectProblems_ReportsMCPBulletsAndChainSteps(t *testing.T) {
	mcpS := scenario.Parse(mcpScenarioMD() + `- content[0].text == "x"` + "\n")
	ps := ExpectProblems(mcpS)
	if len(ps) != 1 || ps[0].Bullet != `content[0].text == "x"` || !strings.Contains(ps[0].Message, ps[0].Bullet) {
		t.Fatalf("an mcp scenario's bad bullet must be reported once, quoting it: %+v", ps)
	}
	// V31-002: the claims are read from `### Runnable`, the only home.
	chain := &scenario.Scenario{ID: "C-1", Tags: []string{"chain"},
		ExpectRunnable: []string{"step search: result.isError == false; body contains x"}}
	chain.Trigger.Payload = `{"steps":[{"type":"mcp","name":"search","tool":"t"}]}`
	ps = ExpectProblems(chain)
	if len(ps) != 1 || ps[0].Step != "search" || !strings.Contains(ps[0].Message, "one bullet per assertion") {
		t.Fatalf("a chain claim's `;` string must be reported naming the step: %+v", ps)
	}
	chain.ExpectRunnable = []string{
		"step ok: result.isError == false",
		"step ok: body has id containing ${saved.docId}",
	}
	chain.Trigger.Payload = `{"steps":[{"type":"mcp","name":"ok","tool":"t"}]}`
	if ps = ExpectProblems(chain); len(ps) != 0 {
		t.Fatalf("a clean chain (with a run-time ${saved.…}) has no problems: %+v", ps)
	}
	// an HTTP-layer scenario is not this parser's business (its `body has` goes to the JSR223 path)
	httpS := scenario.Parse("# Scenario: h\n\n## Metadata\n- **ID**: H-1\n- **Layer**: HTTP Ingestion\n\n## TRIGGER\nPOST `${URL}/x`\n\n## EXPECT\n- status=400\n- body has error containing items\n")
	if ps = ExpectProblems(httpS); len(ps) != 0 {
		t.Fatalf("a non-mcp scenario must not be policed here: %+v", ps)
	}
}

// VR12-E8 helpers — the judge now carries EVERY assertion in Expect.Body instead of one scalar of
// each kind. These read the first assertion of a kind so the existing cases stay readable; the
// "every bullet is wired" contract itself is covered by TestParseBodyAsserts.
func firstContains(e mcp.Expect) string {
	for _, a := range e.Body {
		if a.Op == mcp.BodyContainsOp {
			return a.Value
		}
	}
	return ""
}

func firstMatches(e mcp.Expect) string {
	for _, a := range e.Body {
		if a.Op == mcp.BodyMatchesOp {
			return a.Value
		}
	}
	return ""
}
