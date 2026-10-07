package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

type mcpNopRunner struct{}

func (mcpNopRunner) Run(_, _ string, _ map[string]string, _ time.Duration) error { return nil }

// fakeMCPServer is a minimal conformant Streamable-HTTP MCP server.
func fakeMCPServer() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": `{"ok":true}`}}, "isError": false}})
		}
	}
}

func mcpScenarioMD() string {
	tick := "\x60"
	return strings.Join([]string{
		"# Scenario: MCP echo", "",
		"## Metadata",
		"- **ID**: MCP-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: mcp, critical", "",
		"## TRIGGER",
		"POST " + tick + "${MCP_URL}" + tick, "",
		tick + tick + tick + "json",
		`{"transport":"streamable-http","tool":"ok_tool","args":{"x":1}}`,
		tick + tick + tick, "",
		// V31-002 (R1): a flat `## EXPECT` declares no runnable check now, so the fixture says which
		// bullet it means to be executed. (mcpScenarioMDSplit is the twin that lets a test place a
		// bullet under either sub-section deliberately.)
		"## EXPECT",
		"### Runnable",
		"- result.isError == false", "",
	}, "\n")
}

// fakeMCPToolErrorServer is conformant at the transport/protocol layer but returns
// result.isError:true (the tool plane) — an error RESPONSE that a tool-plane scenario
// still PASSES on.
func fakeMCPToolErrorServer() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "boom"}}, "isError": true}})
		}
	}
}

func mcpScenarioMDExpect(expect string) string {
	return strings.Replace(mcpScenarioMD(), "- result.isError == false", "- "+expect, 1)
}

func mcpCfg(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+baseURL+"\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// Test-requests panel (r3): an MCP scenario fires exactly ONE request (the tool call), recorded
// as a timestamped sample by its RESPONSE outcome (3-way) — NOT the scenario's verdict.
func TestRunMCPScenario_RequestCounts(t *testing.T) {
	os.Unsetenv("MCP_URL")

	// success: a clean isError:false reply → ReqSuccess=1 + one sample.
	okTS := httptest.NewServer(fakeMCPServer())
	defer okTS.Close()
	ok := runMCPScenario(mcpCfg(t, okTS.URL), scenario.Parse(mcpScenarioMD()), "tr-ok")
	if ok.Status != "passed" || ok.ReqSuccess != 1 || ok.ReqFailed != 0 || ok.ReqError != 0 {
		t.Errorf("isError:false call → passed + success=1, got status=%s success=%d failed=%d error=%d", ok.Status, ok.ReqSuccess, ok.ReqFailed, ok.ReqError)
	}
	if len(ok.Requests) != 1 || ok.Requests[0].Outcome != report.OutcomeSuccess || ok.Requests[0].AtMs == 0 {
		t.Errorf("MCP success must record one timestamped success sample, got %+v", ok.Requests)
	}

	// HEADLINE (MCP): a tool-plane scenario that EXPECTs isError:true PASSES, yet the
	// isError:true RESPONSE is a negative response → recorded as FAILED (not error).
	errTS := httptest.NewServer(fakeMCPToolErrorServer())
	defer errTS.Close()
	te := runMCPScenario(mcpCfg(t, errTS.URL), scenario.Parse(mcpScenarioMDExpect("result.isError == true")), "tr-te")
	if te.Status != "passed" {
		t.Fatalf("the tool-error scenario must PASS (isError:true expected): %+v", te)
	}
	if te.ReqFailed != 1 || te.ReqSuccess != 0 || te.ReqError != 0 {
		t.Errorf("the isError:true RESPONSE must count ReqFailed=1 (negative response, NOT error) even though the scenario PASSED, got success=%d failed=%d error=%d", te.ReqSuccess, te.ReqFailed, te.ReqError)
	}

	// unreachable endpoint → no response → error (ReqError=1). The scenario is ERRORED, not
	// failed — see TestRunMCPScenario_UnreachableIsErroredNotFailed.
	un := runMCPScenario(mcpCfg(t, "http://127.0.0.1:1/mcp"), scenario.Parse(mcpScenarioMD()), "tr-un")
	if un.ReqError != 1 || un.ReqSuccess != 0 || un.ReqFailed != 0 {
		t.Errorf("unreachable call → ReqError=1, got success=%d failed=%d error=%d", un.ReqSuccess, un.ReqFailed, un.ReqError)
	}
}

// mcpRequestOutcome unit: the 3-way split, incl. the JSON-RPC protocol error → failed path
// (a negative RESPONSE), which the server-backed cases above don't exercise directly.
func TestMCPRequestOutcome(t *testing.T) {
	if o := mcpRequestOutcome(mcp.CallResult{}); o != report.OutcomeSuccess {
		t.Errorf("clean reply → success, got %s", o)
	}
	if o := mcpRequestOutcome(mcp.CallResult{IsError: true}); o != report.OutcomeFailed {
		t.Errorf("isError:true → failed, got %s", o)
	}
	if o := mcpRequestOutcome(mcp.CallResult{JSONRPCError: &mcp.RPCError{Code: -32601}}); o != report.OutcomeFailed {
		t.Errorf("JSON-RPC protocol error → failed, got %s", o)
	}
	if o := mcpRequestOutcome(mcp.CallResult{Unreachable: true}); o != report.OutcomeError {
		t.Errorf("unreachable → error, got %s", o)
	}
	if o := mcpRequestOutcome(mcp.CallResult{TransportErr: "handshake failed"}); o != report.OutcomeError {
		t.Errorf("transport error → error, got %s", o)
	}
}

func TestParseExpectPlane(t *testing.T) {
	// VR10-S2: parseExpectPlane now also returns an error (a bullet it cannot classify); the plane
	// bullets below are unchanged and classify without one.
	if p, err := parseExpectPlane([]string{"result.isError == false"}); err != nil || p.ErrorPlane != mcp.PlaneNone {
		t.Errorf("isError false -> none, got %+v err=%v", p, err)
	}
	if p, err := parseExpectPlane([]string{"result.isError == true"}); err != nil || p.ErrorPlane != mcp.PlaneTool {
		t.Errorf("isError true -> tool, got %+v err=%v", p, err)
	}
	if p, err := parseExpectPlane([]string{"jsonrpc error == -32601 (method not found)"}); err != nil || p.ErrorPlane != mcp.PlaneProtocol || p.ErrorCode != -32601 {
		t.Errorf("jsonrpc error -32601 -> protocol+code, got %+v err=%v", p, err)
	}
}

func TestResolveVars(t *testing.T) {
	os.Setenv("ARGUS_TEST_VAR", "VALUE")
	defer os.Unsetenv("ARGUS_TEST_VAR")
	got := resolveVars("${ARGUS_TEST_VAR}/x?cid=${cid}", "tr-9")
	if got != "VALUE/x?cid=tr-9" {
		t.Fatalf("resolveVars = %q", got)
	}
	if r := resolveVars("${UNSET_NOPE_VAR}", "tr-9"); !strings.Contains(r, "${") {
		t.Fatalf("an unset var must be left literal for preflight, got %q", r)
	}
}

// VR-J9 / UC-80: an mcp scenario runs through the STANDARD RunAll pipeline and lands
// in report.json with a per-scenario pass + the judged envelope captured (VR-J10).
func TestRunAll_MCPScenario(t *testing.T) {
	ts := httptest.NewServer(fakeMCPServer())
	defer ts.Close()
	os.Setenv("MCP_URL", ts.URL+"/mcp")
	defer os.Unsetenv("MCP_URL")

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MCP-001.md"), []byte(mcpScenarioMD()), 0o644)
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  http:\n    base_url: http://localhost:8080\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rr, err := RunAll(c, dir, t.TempDir(), "t", "", "", "", "", mcpNopRunner{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("MCP-001")
	if res == nil || res.Status != "passed" {
		t.Fatalf("mcp scenario should PASS through the standard pipeline: %+v", res)
	}
	if res.MCPEnvelope == nil {
		t.Error("mcp scenario should capture the judged JSON-RPC envelope (VR-J10)")
	}
	if rr.CorrelationID["MCP-001"] == "" {
		t.Error("mcp scenario should get a correlation id like any scenario")
	}
}

// UC-82: with NO endpoint anywhere (no env override, no targets.mcp) the run fails CLEARLY
// at preflight (never calls a literal ${VAR}).
func TestRunMCPScenario_NoEndpoint(t *testing.T) {
	os.Unsetenv("MCP_URL")
	s := scenario.Parse(mcpScenarioMD())
	bare := &config.Config{} // no targets.mcp
	res := runMCPScenario(bare, s, "tr-1")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("a scenario with no resolvable mcp endpoint must fail clearly at preflight: %+v", res)
	}
	if !strings.Contains(res.Failure.Observed, "targets.mcp.base_url") {
		t.Errorf("the preflight message should point at targets.mcp.base_url: %q", res.Failure.Observed)
	}
}

// Patch #3: the runner-native MCP path must record DurationMs so argus_scenario_duration_seconds
// (the "Slowest scenarios" panel) is non-zero for MCP SUTs. The fake server sleeps a few ms.
func TestRunMCPScenario_RecordsDuration(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(8 * time.Millisecond) // ensure a measurable end-to-end duration
		fakeMCPServer()(w, r)
	}))
	defer ts.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+ts.URL+"\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	res := runMCPScenario(c, scenario.Parse(mcpScenarioMD()), "tr-dur")
	if res.Status != "passed" {
		t.Fatalf("expected pass: %+v", res)
	}
	if res.DurationMs < 1 {
		t.Errorf("mcp scenario must record a non-zero DurationMs (Slowest panel feed), got %d", res.DurationMs)
	}
}

// CHANGE-1 done-when: an mcp scenario resolves its endpoint + token + transport PURELY
// from argus-config targets.mcp — no MCP_URL/MCP_TOKEN env. The fake server requires the
// declared bearer, proving the token came from config.
func TestRunMCPScenario_ResolvesFromConfig(t *testing.T) {
	os.Unsetenv("MCP_URL")
	os.Unsetenv("MCP_TOKEN")
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			gotAuth = a
		}
		fakeMCPServer()(w, r)
	}))
	defer ts.Close()

	// config carries targets.mcp only (NO targets.http, NO env) — the single onboarding artifact.
	cfgYAML := "project:\n  name: social\ntargets:\n  mcp:\n    base_url: " + ts.URL + "\n    transport: streamable-http\n    auth:\n      type: bearer\n      bearer_token: smcp_fromconfig\n"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte(cfgYAML), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s := scenario.Parse(mcpScenarioMD()) // TRIGGER is ${MCP_URL} (unset) → must fall back to config
	res := runMCPScenario(c, s, "tr-cfg")
	if res.Status != "passed" {
		t.Fatalf("scenario should PASS resolving endpoint from targets.mcp.base_url: %+v", res)
	}
	if gotAuth != "Bearer smcp_fromconfig" {
		t.Errorf("token must come from targets.mcp.auth.bearer_token, got Authorization=%q", gotAuth)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// V31-006 (VR13-RB) — every place that DECIDES a result reads `### Runnable` ONLY.
//
// The 0.3.31 contract is that POSITION decides whether a bullet is a claim (VR12-E1). Three readers
// of Scenario.Expect still read the WHOLE list, and the single-call MCP path is the one that decides
// verdicts with it: a `### Non-runnable` sentence could fail a scenario, change which error it
// expected, or stop it before it ran. These tests pin all three, plus the stranded-bullet ruling.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// mcpScenarioMDSplit is mcpScenarioMD's sub-headed twin: the caller supplies the whole ## EXPECT
// body, so a test can put a line under `### Runnable` or `### Non-runnable` deliberately.
// (V31-003 and V31-005 reuse it.)
func mcpScenarioMDSplit(args, expect string) string {
	tick := "\x60"
	return strings.Join([]string{
		"# Scenario: MCP split", "",
		"## Metadata",
		"- **ID**: MCP-SPLIT",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: mcp", "",
		"## TRIGGER",
		"POST " + tick + "${MCP_URL}" + tick, "",
		tick + tick + tick + "json",
		`{"transport":"streamable-http","tool":"ok_tool","args":` + args + `}`,
		tick + tick + tick, "",
		"## EXPECT",
		expect, "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// runnableThen builds a ## EXPECT whose `### Runnable` half is the success plane and whose
// `### Non-runnable` half is whatever the case is about.
func runnableThen(nonRunnable ...string) string {
	lines := []string{"### Runnable", "- result.isError == false", "", "### Non-runnable"}
	return strings.Join(append(lines, nonRunnable...), "\n")
}

// a. A `### Non-runnable` line is documentation. It must not be enforced, must not change the
// expected plane, and must not stop the scenario before it runs.
func TestRunMCPScenario_NonRunnableLineIsNeverEnforced(t *testing.T) {
	cases := []struct {
		name        string
		nonRunnable []string
	}{
		{"body", []string{"- body has id containing zzz"}},
		{"plane prose", []string{"- an unknown id is answered with error code -32602 by the server"}},
		{"preflight", []string{`- content[0].text == "document not found"`}},
		{"all three", []string{
			"- body has id containing zzz",
			"- an unknown id is answered with error code -32602 by the server",
			`- content[0].text == "document not found"`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("MCP_URL")
			ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
			defer ts.Close()
			s := scenario.Parse(mcpScenarioMDSplit("{}", runnableThen(tc.nonRunnable...)))
			res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-nr")
			if res.Status != "passed" {
				obs := ""
				if res.Failure != nil {
					obs = res.Failure.Observed
				}
				t.Fatalf("status = %q (observed %q), want passed — a `### Non-runnable` line decided the verdict", res.Status, obs)
			}
			if res.ReqSuccess != 1 {
				t.Fatalf("ReqSuccess = %d, want 1 — the scenario must still fire exactly one call", res.ReqSuccess)
			}
			if res.Failure != nil {
				t.Fatalf("Failure = %+v, want nil", res.Failure)
			}
		})
	}
}

// b. The sharpest case: a plane line under `### Non-runnable` must not set the expected plane —
// on 0.3.31 it silently turned a real tool error into a PASS.
func TestRunMCPScenario_NonRunnablePlaneLineNeverSetsThePlane(t *testing.T) {
	cases := []struct {
		name   string
		expect string
	}{
		{"the false green", runnableThen("- result.isError == true")},
		{"a plane line only under Non-runnable", strings.Join([]string{"### Non-runnable", "- result.isError == true"}, "\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("MCP_URL")
			ts := httptest.NewServer(fakeMCPToolErrorServer())
			defer ts.Close()
			s := scenario.Parse(mcpScenarioMDSplit("{}", tc.expect))
			res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-plane")
			if res.Status != "failed" {
				t.Fatalf("status = %q, want failed — the SUT returned isError:true and no `### Runnable` line expects it", res.Status)
			}
			if res.ReqFailed != 1 {
				t.Fatalf("ReqFailed = %d, want 1", res.ReqFailed)
			}
			if res.Failure == nil || res.Failure.Observed != "responder returned result.isError:true" {
				t.Fatalf("Failure = %+v, want observed %q", res.Failure, "responder returned result.isError:true")
			}
		})
	}
}

// e. GUARD — VR12-E14 on all three engines that build a Failure: `Failure.Expected` keeps BOTH
// sub-sections. Nothing pinned this before, which is why the first filing's step 4 looked safe.
func TestFailureExpectedKeepsBothSubSections(t *testing.T) {
	const runnableLine = "- body has id containing zzz"
	const nonRunnableLine = "- the id is minted by the server"

	t.Run("mcp", func(t *testing.T) {
		os.Unsetenv("MCP_URL")
		ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
		defer ts.Close()
		expect := strings.Join([]string{
			"### Runnable", "- result.isError == false", runnableLine, "", "### Non-runnable", nonRunnableLine,
		}, "\n")
		res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMDSplit("{}", expect)), "tr-e14")
		assertExpectedCarriesBoth(t, &res, runnableLine, nonRunnableLine)
	})

	t.Run("http", func(t *testing.T) {
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		md := strings.Join([]string{
			"# Scenario: GATE-002", "",
			"## Metadata",
			"- **ID**: GATE-002",
			"- **Layer**: HTTP Ingestion",
			"- **Tags**: http", "",
			"## TRIGGER",
			"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT",
			"### Runnable",
			"- status=202", "",
			"### Non-runnable",
			"- the order is visible", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		writeScenarioMD(t, scDir, "http-ingestion", "GATE-002", md)
		fr := &fakeRunner{pass: map[string]bool{}}
		rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		res, _ := rr.Report.Find("GATE-002")
		if res == nil {
			t.Fatal("GATE-002 not in the report")
		}
		assertExpectedCarriesBoth(t, res, "- status=202", "- the order is visible")
	})

	t.Run("ui", func(t *testing.T) {
		orig := uiRun
		t.Cleanup(func() { uiRun = orig })
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, true, ""
		}
		tick := "\x60"
		md := strings.Join([]string{
			"# Scenario: UI-E14", "",
			"## Metadata",
			"- **ID**: UI-E14",
			"- **Layer**: Web UI",
			"- **Tags**: ui", "",
			"## TRIGGER",
			"POST " + tick + "tests/live/x.spec.ts" + tick, "",
			tick + tick + tick + "json",
			`{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`,
			tick + tick + tick, "",
			"## EXPECT",
			"### Runnable",
			"- the dashboard renders", "",
			"### Non-runnable",
			"- the flow is described in the spec file", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		res := runUIScenario(scenario.Parse(md), "tr-ui", "testkit/ui")
		assertExpectedCarriesBoth(t, &res, "- the dashboard renders", "- the flow is described in the spec file")
	})
}

func assertExpectedCarriesBoth(t *testing.T, res *report.ScenarioResult, runnable, nonRunnable string) {
	t.Helper()
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed (the fixture is built to fail so a Failure exists)", res.Status)
	}
	if res.Failure == nil || res.Failure.Expected == nil {
		t.Fatalf("Failure/Expected is nil, want both sub-sections (VR12-E14)")
	}
	exp := *res.Failure.Expected
	if !strings.Contains(exp, strings.TrimPrefix(runnable, "- ")) {
		t.Errorf("Failure.Expected %q does not carry the RUNNABLE line %q", exp, runnable)
	}
	if !strings.Contains(exp, strings.TrimPrefix(nonRunnable, "- ")) {
		t.Errorf("Failure.Expected %q does not carry the NON-RUNNABLE line %q (VR12-E14)", exp, nonRunnable)
	}
}

// f. Fix step 4, the owner's item m: a bullet ABOVE the first `### ` heading, or under an undefined
// one, is enforced by NO engine. The validator refuses such a file; the run path does not special-case
// it here (V31-002's run-time report is what closes it for every engine).
func TestRunMCPScenario_StrandedBulletIsNotEnforced(t *testing.T) {
	cases := []struct{ name, expect string }{
		{"stranded", strings.Join([]string{"- body has id containing zzz", "", "### Runnable", "- result.isError == false"}, "\n")},
		{"a misspelled heading", strings.Join([]string{"### Runable", "- body has id containing zzz", "", "### Runnable", "- result.isError == false"}, "\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("MCP_URL")
			ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
			defer ts.Close()
			res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMDSplit("{}", tc.expect)), "tr-stranded")
			if res.Status != "passed" {
				obs := ""
				if res.Failure != nil {
					obs = res.Failure.Observed
				}
				t.Fatalf("status = %q (observed %q), want passed — a bullet outside `### Runnable` is enforced by no engine", res.Status, obs)
			}
			if res.ReqSuccess != 1 || res.Failure != nil {
				t.Fatalf("ReqSuccess = %d, Failure = %+v; want 1 and nil", res.ReqSuccess, res.Failure)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// V31-005 (VR13-EE) — the SINGLE-CALL MCP path, end to end.
//
// No code changes here (mcp_scenario.go already maps a PlaneBody miss to a failed response and
// builds failure.expected from the file's EXPECT), so these prove the judge's new behaviour reaches
// a real scenario run — and that `### Runnable` is still what decides, on an error scenario too.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

func TestRunMCPScenario_ExpectedErrorWithABodyMissIsFailed(t *testing.T) {
	// fakeMCPToolErrorServer answers result.isError:true with the text "boom".
	expectWith := func(bullet string) string {
		return strings.Join([]string{
			"### Runnable",
			"- result.isError == true",
			"- " + bullet, "",
			"### Non-runnable",
			"- the tool refuses the call",
		}, "\n")
	}

	t.Run("a check that misses on the expected error FAILS the scenario", func(t *testing.T) {
		os.Unsetenv("MCP_URL")
		ts := httptest.NewServer(fakeMCPToolErrorServer())
		defer ts.Close()
		s := scenario.Parse(mcpScenarioMDSplit("{}", expectWith("body contains not-boom")))
		res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-ee")

		if res.Status != "failed" {
			t.Fatalf("status = %q, want failed — the error came back but the scenario's own check did not hold", res.Status)
		}
		if res.ReqFailed != 1 || res.ReqError != 0 {
			t.Errorf("req failed/error = %d/%d, want 1/0 — the SUT ANSWERED; this is not an execution error",
				res.ReqFailed, res.ReqError)
		}
		if res.Failure == nil {
			t.Fatal("Failure is nil")
		}
		if res.Failure.Observed != mcp.BodyAssertObservedToolError {
			t.Errorf("Observed = %q, want BodyAssertObservedToolError", res.Failure.Observed)
		}
		// the test hat still reads what was asserted, from the file (VR12-E14)
		if res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "body contains not-boom") {
			t.Errorf("Expected = %v, want it to quote the scenario's own bullet", res.Failure.Expected)
		}
	})

	t.Run("the same scenario with a check that holds passes", func(t *testing.T) {
		os.Unsetenv("MCP_URL")
		ts := httptest.NewServer(fakeMCPToolErrorServer())
		defer ts.Close()
		s := scenario.Parse(mcpScenarioMDSplit("{}", expectWith("body contains boom")))
		if res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-ee2"); res.Status != "passed" {
			obs := ""
			if res.Failure != nil {
				obs = res.Failure.Observed
			}
			t.Fatalf("status = %q (observed %q), want passed", res.Status, obs)
		}
	})
}

// GUARD — V31-006 and V31-005 together: a body line under `### Non-runnable` is documentation even
// on an expected error. Without this, the new evaluation would quietly extend to lines the author
// marked as prose, which is the defect the previous unit removed.
func TestRunMCPScenario_NonRunnableBodyLineOnAnExpectedErrorIsNotJudged(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPToolErrorServer())
	defer ts.Close()
	expect := strings.Join([]string{
		"### Runnable",
		"- result.isError == true", "",
		"### Non-runnable",
		"- body contains not-boom",
	}, "\n")
	s := scenario.Parse(mcpScenarioMDSplit("{}", expect))
	if res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-ee3"); res.Status != "passed" {
		obs := ""
		if res.Failure != nil {
			obs = res.Failure.Observed
		}
		t.Fatalf("status = %q (observed %q), want passed — a `### Non-runnable` line is never judged", res.Status, obs)
	}
}

// V31-003 (VR13-CID) — `${correlation_id}` is the SECOND name for the run's own id, and resolveVars
// never knew it: it expanded `${cid}` and an environment `${VAR}` and left `${correlation_id}`
// literal everywhere except a VERIFY query, which had its own explicit substitution.
func TestResolveVars_FillsCorrelationIDToo(t *testing.T) {
	t.Setenv("ARGUS_TEST_VAR", "VALUE")
	const want = "VALUE/x?c=tr-9&d=tr-9"
	if got := resolveVars("${ARGUS_TEST_VAR}/x?c=${correlation_id}&d=${cid}", "tr-9"); got != want {
		t.Fatalf("resolveVars = %q, want %q", got, want)
	}
}

// V31-003 (VR13-CID) — a check on the run's own id, on the single-call MCP path.
func TestRunMCPScenario_CorrelationIDInACheckIsFilledIn(t *testing.T) {
	const corr = "tr-cid-5"
	run := func(t *testing.T, bullet string) report.ScenarioResult {
		t.Helper()
		os.Unsetenv("MCP_URL")
		ts := httptest.NewServer(fakeMCPTextServer(""))
		defer ts.Close()
		expect := strings.Join([]string{
			"### Runnable",
			"- result.isError == false",
			"- " + bullet, "",
			"### Non-runnable",
			"- the answer echoes the arguments",
		}, "\n")
		s := scenario.Parse(mcpScenarioMDSplit(`{"owner":"o-${cid}","ref":"r-${correlation_id}"}`, expect))
		return runMCPScenario(mcpCfg(t, ts.URL), s, corr)
	}

	t.Run("a check naming the run's id passes", func(t *testing.T) {
		res := run(t, "body has owner containing o-"+"${cid}")
		if res.Status != "passed" {
			obs := ""
			if res.Failure != nil {
				obs = res.Failure.Observed
			}
			t.Fatalf("status = %q (observed %q), want passed — the id was never filled into the check", res.Status, obs)
		}
	})

	t.Run("and a check naming something else still fails, with the bullet AS WRITTEN", func(t *testing.T) {
		res := run(t, "body has owner containing zz-"+"${cid}")
		if res.Status != "failed" {
			t.Fatalf("status = %q, want failed", res.Status)
		}
		// VR12-E14: failure.expected quotes the author's own text, placeholder unfilled, so the
		// pair reads as the author wrote it.
		if res.Failure == nil || res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "zz-${cid}") {
			t.Errorf("failure.expected must keep `${cid}` UNFILLED, got %v", res.Failure)
		}
	})
}

// GUARD (decision c) — THE CORRELATION ID MUST NEVER CHANGE WHAT A BULLET MEANS.
//
// The id embeds the scenario id, an id may contain `_`, and the classifier reads any `a_b` token as
// a field name. So filling in BEFORE classification could turn prose into a "bullet not understood"
// refusal that the validator — which reads the raw text — never saw. It passes before the change
// too; its value is that it fails the day someone fills in first.
func TestRunMCPScenario_CorrelationIDNeverChangesWhatABulletMeans(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer(""))
	defer ts.Close()
	expect := strings.Join([]string{
		"### Runnable",
		"- result.isError == false",
		"- the answer contains ${cid}", "",
		"### Non-runnable",
		"- this bullet is prose, and must stay prose",
	}, "\n")
	s := scenario.Parse(mcpScenarioMDSplit("{}", expect))
	res := runMCPScenario(mcpCfg(t, ts.URL), s, "tr-1-RACE_002-ab12")
	if res.Status != "passed" {
		obs := ""
		if res.Failure != nil {
			obs = res.Failure.Observed
		}
		t.Fatalf("status = %q (observed %q) — filling the id in before classification turned prose into a refusal", res.Status, obs)
	}
	if res.ReqSuccess != 1 {
		t.Errorf("ReqSuccess = %d, want 1", res.ReqSuccess)
	}
}
