package toolcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// Spec 26 P1 (A1, observe only): wiring the sandbox evidence reader into the one run funnel.

func TestEvidenceFor_NilWhenBlockAbsent(t *testing.T) {
	if ev := evidenceFor(Env{Loki: "http://loki:3100"}, &config.Config{}); ev != nil {
		t.Fatalf("no observability.openshell block: want a nil reader (an untyped nil interface), got %#v", ev)
	}
	if ev := evidenceFor(Env{Loki: "http://loki:3100"}, nil); ev != nil {
		t.Fatalf("no config: want a nil reader, got %#v", ev)
	}
}

// The reader is the SAME Loki client get_tail_logs uses (lokiFor): the executor's --loki, its tenant
// (T3.3), and the credential rule of — not observability.loki.url, which is the
// promtail push target (spec 26 §9 G1).
func TestEvidenceFor_ThreadsLokiURLAndTenant(t *testing.T) {
	c := &config.Config{}
	c.Observability.OpenShell = &config.OpenShellObs{Source: "loki", Selector: `{job="openshell-gateway"}`, Sandbox: "sb-1", WindowPad: "5s"}
	ev := evidenceFor(Env{Loki: "http://loki.inst-a:3100", LokiTenant: "inst-a", ConfigPath: "does-not-exist.yaml"}, c)
	l, ok := ev.(*obsquery.Loki)
	if !ok || l == nil {
		t.Fatalf("want a *obsquery.Loki reader, got %#v", ev)
	}
	if l.BaseURL != "http://loki.inst-a:3100" || l.Tenant != "inst-a" {
		t.Fatalf("reader = BaseURL %q Tenant %q; want the executor's --loki and --loki-tenant", l.BaseURL, l.Tenant)
	}
}

// ── (finding A9): sandbox_policy follows the run's custody ──────────────────────────
//
// On an ORDINARY run (build, or the builder's own local "ci" run) the events are the SUT agent's own
// traffic — observed reality, not the test's expected values — so the product hat reads the block as
// designed, like Failure.Observed: redactExpected leaves it alone. On a CERTIFICATION run (final,
// scheduled, rehearsal, or a mode that is unknown or empty) the #417 rule applies unchanged (the operator,
// 2026-10-02): the builder gets the verdict only — no block, no event, no count, no coverage word —
// on every route a builder reads. The author hat reads the whole block on every run.

func intPtr(n int) *int { return &n }

// sandboxCanary is the event target of every fixture here, so a leak is one Contains away.
const sandboxCanary = "sandbox-target-canary.example"

// reportWithSandboxBlock is a two-row report: one row with a measured denial, one `unavailable`.
func reportWithSandboxBlock(mode string) *report.Report {
	return &report.Report{
		Project: "p", RunID: "run-SB", Mode: mode, Note: "n",
		Summary: report.Summary{Total: 2, Passed: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{
			{ID: "SB-001", Status: "failed", CorrelationID: "tr-run-SB-SB-001-ab", Failure: &report.Failure{},
				SandboxPolicy: &report.SandboxPolicy{Source: "loki", Sandbox: "sb-canary-1", Coverage: report.CoverageComplete,
					CoverageReason: "read 2 lines from the sandbox", DeniedCount: intPtr(1),
					Events: []report.SandboxPolicyEvent{{Class: "NET:OPEN", Action: "Denied", Target: sandboxCanary + ":443", Reason: "no matching policy"}}}},
			{ID: "SB-002", Status: "passed", CorrelationID: "tr-run-SB-SB-002-ab",
				SandboxPolicy: &report.SandboxPolicy{Source: "loki", Sandbox: "sb-canary-1", Coverage: report.CoverageUnavailable,
					CoverageReason: "Loki answered HTTP 401", Events: []report.SandboxPolicyEvent{}}},
		}}},
	}
}

// sandboxLeaks names every trace of a sandbox_policy block in b: its key and fields, the three
// coverage words, the canary event, the sandbox id and the reasons.
func sandboxLeaks(b []byte) []string {
	var out []string
	for _, w := range []string{"sandbox_policy", "denied_count", "coverage", "events_omitted", "shared_with",
		report.CoverageComplete, report.CoverageLossy, report.CoverageUnavailable,
		sandboxCanary, "sb-canary-1", "sb-1", "no matching policy", "HTTP 401", "read 2 lines"} {
		if strings.Contains(string(b), w) {
			out = append(out, w)
		}
	}
	return out
}

// The report a builder reads (runner__get_report in-env, `argus get-report`, and the relayed
// get_report, which reads the file through this same function): on an ordinary run the block is
// there for both hats, untouched; on a certification run the product hat gets the verdict and the
// tallies and nothing of the block, through the per-run file and the latest file alike, while the
// author still reads it whole. (This replaces TestRedactExpected_KeepsSandboxPolicyInP1, which
// pinned only the ordinary-run half.)
func TestGetReport_SandboxPolicyFollowsTheRunsCustody(t *testing.T) {
	for _, viaRunFile := range []bool{true, false} {
		read := func(t *testing.T, mode string, hat role.Role) (*report.Report, []byte) {
			t.Helper()
			e := Env{Instance: "local", ResultsRoot: t.TempDir()}
			path, runArg := e.reportPath(), ""
			if viaRunFile {
				path, runArg = runReportPath(e.resultsDir(), "run-SB"), "run-SB"
			}
			if err := writeReport(path, reportWithSandboxBlock(mode)); err != nil {
				t.Fatal(err)
			}
			got, _, err := GetReport(e, hat, runArg)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got)
			rep, ok := got.(*report.Report)
			if !ok {
				t.Fatalf("mode %q: want a *report.Report, got %s", mode, b)
			}
			return rep, b
		}
		for _, mode := range []string{"build", "ci"} {
			for _, hat := range []role.Role{role.Product, role.Test} {
				rep, b := read(t, mode, hat)
				p := rep.Layers[0].Scenarios[0].SandboxPolicy
				if p == nil || p.DeniedCount == nil || *p.DeniedCount != 1 || len(p.Events) != 1 || p.Events[0].Target != sandboxCanary+":443" {
					t.Errorf("per-run file %v, ordinary run %q, %v hat: the block must arrive as designed; got %s", viaRunFile, mode, hat, b)
				}
				if q := rep.Layers[0].Scenarios[1].SandboxPolicy; q == nil || q.Coverage != report.CoverageUnavailable || q.DeniedCount != nil {
					t.Errorf("per-run file %v, ordinary run %q, %v hat: the unavailable row must keep its block; got %s", viaRunFile, mode, hat, b)
				}
			}
		}
		for _, mode := range []string{"final", "scheduled", "rehearsal", "unknown", "", "mystery"} {
			rep, b := read(t, mode, role.Product)
			if leaks := sandboxLeaks(b); len(leaks) > 0 {
				t.Errorf("per-run file %v, certification run %q: the builder's report carries %q: %s", viaRunFile, mode, leaks, b)
			}
			if rep.Summary.Total != 2 || rep.Summary.Failed != 1 || rep.Summary.Passed != 1 {
				t.Errorf("per-run file %v, certification run %q: the verdict and tallies must survive: %s", viaRunFile, mode, b)
			}
			full, fb := read(t, mode, role.Test)
			if len(full.Layers) != 1 || full.Layers[0].Scenarios[0].SandboxPolicy == nil || !strings.Contains(string(fb), sandboxCanary) {
				t.Errorf("per-run file %v, certification run %q: the author must still read the block whole: %s", viaRunFile, mode, fb)
			}
		}
	}
}

// sandboxLoki is a fake --loki for a run with the block declared. answer "denied": the sandbox read
// returns one denied CONNECT to the canary inside the window; "401": the sandbox read is refused.
// The pushes (request events, metrics) are accepted.
func sandboxLoki(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if answer == "401" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		at := time.Unix(0, start).Add(time.Millisecond)
		line := fmt.Sprintf(`{"class_uid":4001,"activity_name":"Open","action_id":2,"action":"Denied","disposition":"Blocked","status_detail":"no matching policy","time":%d,"metadata":{"version":"1.8.0"},"dst_endpoint":{"domain":"%s","port":443},"actor":{"process":{"name":"/usr/bin/curl"}},"firewall_rule":{"name":"-"},"container":{"uid":"sb-1"}}`, at.UnixMilli(), sandboxCanary)
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"result": []any{
			map[string]any{"stream": map[string]string{"job": "openshell-gateway"}, "values": [][2]string{{strconv.FormatInt(at.UnixNano(), 10), line}}},
		}}})
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// sandboxRunEnv is an Env for one MCP scenario (MCP-001) with observability.openshell declared,
// reading the given fake Loki.
func sandboxRunEnv(t *testing.T, lokiURL string) Env {
	t.Helper()
	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n"+
		"observability:\n  openshell:\n    source: loki\n    selector: '{job=\"openshell-gateway\"}'\n    sandbox: sb-1\n    window_pad: 10ms\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = lokiURL
	e.Pushgateway = lokiURL
	return e
}

// captureSlog sends the default logger to a buffer for the test and restores it afterwards.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// End to end through the one funnel (toolcore.Run, as the executor runs a federated assignment with
// Env.RunMode set): a certification run records the block for the author, and a builder finds none
// of it — not in the report it reads (per-run or latest), not in Run's own answer, and not in the
// executor's log. The log matters because the executor's stdout is shipped into the same Loki
// runner__get_tail_logs reads (compose: the executor runs in the instance's compose project, which
// the rendered promtail keeps), and get_tail_logs filters by substring, so "tr-<run_id>" (the run id
// is in the builder's verdict) would match a line naming the scenario's correlation id.
func TestRun_CertificationRunGivesTheBuilderTheVerdictOnly(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	for _, mode := range []string{"final", "scheduled", "rehearsal", "unknown"} {
		for _, answer := range []string{"denied", "401"} {
			t.Run(mode+"/"+answer, func(t *testing.T) {
				buf := captureSlog(t)
				e := sandboxRunEnv(t, sandboxLoki(t, answer).URL)
				e.RunMode = mode
				out, _, err := Run(e, "run-cert-sb", "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				// The author's evidence is intact: the run did read the sandbox and record the block.
				full, _, err := GetReport(e, role.Test, "run-cert-sb")
				if err != nil {
					t.Fatal(err)
				}
				fb, _ := json.Marshal(full)
				wantInFull := sandboxCanary
				if answer == "401" {
					wantInFull = "HTTP 401"
				}
				if !strings.Contains(string(fb), `"sandbox_policy"`) || !strings.Contains(string(fb), wantInFull) {
					t.Fatalf("the author's report must carry the block (%q): %s", wantInFull, fb)
				}
				for _, runArg := range []string{"run-cert-sb", ""} {
					got, _, err := GetReport(e, role.Product, runArg)
					if err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(got)
					if leaks := sandboxLeaks(b); len(leaks) > 0 {
						t.Errorf("get_report(run_id=%q) as the builder carries %q: %s", runArg, leaks, b)
					}
				}
				ob, _ := json.Marshal(out)
				if leaks := sandboxLeaks(ob); len(leaks) > 0 {
					t.Errorf("Run's own answer carries %q: %s", leaks, ob)
				}
				for _, line := range strings.Split(buf.String(), "\n") {
					low := strings.ToLower(line)
					if strings.Contains(low, "sandbox") || strings.Contains(low, "coverage") || strings.Contains(line, "MCP-001") {
						t.Errorf("a certification run must log no sandbox line (a builder can read the executor's log): %s", line)
					}
				}
			})
		}
	}
}

// The other half, end to end: on an ordinary run (a build assignment, or a direct run with no mode,
// stamped "ci") the builder reads the block as designed, and an incomplete window is still logged.
func TestRun_OrdinaryRunShowsTheBuilderTheSandboxPolicy(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	for _, mode := range []string{"build", ""} {
		for _, answer := range []string{"denied", "401"} {
			t.Run("mode="+mode+"/"+answer, func(t *testing.T) {
				buf := captureSlog(t)
				e := sandboxRunEnv(t, sandboxLoki(t, answer).URL)
				e.RunMode = mode
				if _, _, err := Run(e, "run-own-sb", "", "", ""); err != nil {
					t.Fatal(err)
				}
				got, _, err := GetReport(e, role.Product, "run-own-sb")
				if err != nil {
					t.Fatal(err)
				}
				rep, ok := got.(*report.Report)
				if !ok || len(rep.Layers) != 1 {
					t.Fatalf("want the builder's own report whole, got %#v", got)
				}
				p := rep.Layers[0].Scenarios[0].SandboxPolicy
				switch answer {
				case "denied":
					if p == nil || p.Coverage != report.CoverageComplete || p.DeniedCount == nil || *p.DeniedCount != 1 || p.Events[0].Target != sandboxCanary+":443" {
						t.Errorf("the builder must read the measured block of its own run, got %+v", p)
					}
				case "401":
					if p == nil || p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "HTTP 401") {
						t.Errorf("the builder must read the unavailable block of its own run, got %+v", p)
					}
					if !strings.Contains(buf.String(), "sandbox policy evidence is not complete") {
						t.Errorf("an incomplete window of an ordinary run is still logged; log was:\n%s", buf.String())
					}
				}
			})
		}
	}
}

// gapsReport is a four-row report of the given mode: complete, lossy, unavailable, and no block.
func gapsReport(mode string) *report.Report {
	ev := []report.SandboxPolicyEvent{{Class: "NET:OPEN", Action: "Denied", Target: "event-target-canary.example:443"}}
	return &report.Report{Mode: mode, Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{
		{ID: "S-OK", CorrelationID: "tr-ok", SandboxPolicy: &report.SandboxPolicy{Coverage: report.CoverageComplete, CoverageReason: "read 3 lines", Events: ev}},
		{ID: "S-LOSSY", CorrelationID: "tr-lossy", SandboxPolicy: &report.SandboxPolicy{Coverage: report.CoverageLossy, CoverageReason: "the query returned its limit", Events: ev}},
		{ID: "S-DOWN", CorrelationID: "tr-down", SandboxPolicy: &report.SandboxPolicy{Coverage: report.CoverageUnavailable, CoverageReason: "Loki answered HTTP 401", Events: []report.SandboxPolicyEvent{}}},
		{ID: "S-OFF", CorrelationID: "tr-off"},
	}}}}
}

// Every `unavailable` or `lossy` block of an ORDINARY run (build, or the builder's own local "ci"
// run) is also a structured log line with the correlation id (the operator sees it without opening
// report.json). Events are never logged.
func TestLogSandboxPolicyGaps_NamesRowAndReason(t *testing.T) {
	for _, mode := range []string{"ci", "build"} {
		buf := captureSlog(t)
		logSandboxPolicyGaps("run-26", gapsReport(mode))

		var lines []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if l == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("not a structured line: %q", l)
			}
			lines = append(lines, m)
		}
		if len(lines) != 2 {
			t.Fatalf("mode %q: want 2 lines (lossy + unavailable), got %d: %s", mode, len(lines), buf.String())
		}
		want := map[string][2]string{"tr-lossy": {"lossy", "the query returned its limit"}, "tr-down": {"unavailable", "Loki answered HTTP 401"}}
		for _, m := range lines {
			w, ok := want[m["correlation_id"].(string)]
			if !ok || m["run_id"] != "run-26" || m["coverage"] != w[0] || m["reason"] != w[1] || m["level"] != "WARN" || m["scenario_id"] == "" {
				t.Errorf("mode %q: line = %v", mode, m)
			}
		}
		if strings.Contains(buf.String(), "event-target-canary") {
			t.Errorf("mode %q: events must never be logged: %s", mode, buf.String())
		}
	}
}

// a run whose scenarios are withheld from the builder (a certification run, or a mode
// that is unknown or empty) logs nothing about its sandbox evidence: no scenario id, no correlation
// id, no coverage word, no reason. The author reads all of it in the report.
func TestLogSandboxPolicyGaps_CertificationRunLogsNothing(t *testing.T) {
	for _, mode := range []string{"final", "scheduled", "rehearsal", "unknown", "", "mystery"} {
		buf := captureSlog(t)
		logSandboxPolicyGaps("run-26", gapsReport(mode))
		if buf.Len() != 0 {
			t.Errorf("mode %q: a certification run must log no sandbox line, got:\n%s", mode, buf.String())
		}
	}
}

// End to end through the one funnel: Run reads the sandbox evidence from the executor's --loki with
// its tenant, and the row in the written report.json carries the block. Mutation guard for the wiring
// line in Run (pass nil instead of evidenceFor: the read never happens).
func TestRun_ReadsSandboxEvidenceThroughTheExecutorsLoki(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	var mu sync.Mutex
	var sandboxQueries, tenants []string
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			w.WriteHeader(http.StatusNoContent) // the request-event push
			return
		}
		q := r.URL.Query()
		mu.Lock()
		sandboxQueries = append(sandboxQueries, q.Get("query"))
		tenants = append(tenants, r.Header.Get("X-Scope-OrgID"))
		mu.Unlock()
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		at := time.Unix(0, start).Add(time.Millisecond)
		// The denied CONNECT of observability/ocsf-json-export.mdx:120-150 (OpenShell v0.1.2), with
		// time, metadata and container added — a doc example, not a capture.
		line := fmt.Sprintf(`{"class_uid":4001,"activity_name":"Open","action_id":2,"action":"Denied","disposition":"Blocked","status_detail":"no matching policy","time":%d,"metadata":{"version":"1.8.0"},"dst_endpoint":{"domain":"httpbin.org","port":443},"actor":{"process":{"name":"/usr/bin/curl"}},"firewall_rule":{"name":"-"},"container":{"uid":"sb-1"}}`, at.UnixMilli())
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"result": []any{
			map[string]any{"stream": map[string]string{"job": "openshell-gateway"}, "values": [][2]string{{strconv.FormatInt(at.UnixNano(), 10), line}}},
		}}})
		_, _ = w.Write(body)
	}))
	defer loki.Close()

	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n"+
		"observability:\n  openshell:\n    source: loki\n    selector: '{job=\"openshell-gateway\"}'\n    sandbox: sb-1\n    window_pad: 10ms\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = loki.URL
	e.Pushgateway = loki.URL
	e.LokiTenant = "inst-a"
	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sandboxQueries) != 1 || sandboxQueries[0] != `{job="openshell-gateway"} |= "sb-1"` || tenants[0] != "inst-a" {
		t.Fatalf("want one sandbox read on --loki with the tenant, got queries %q tenants %q", sandboxQueries, tenants)
	}
	b, err := os.ReadFile(filepath.Join(e.ResultsRoot, "local", "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep report.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	row := rep.Layers[0].Scenarios[0]
	if p := row.SandboxPolicy; p == nil || p.DeniedCount == nil || *p.DeniedCount != 1 || p.Coverage != report.CoverageComplete || p.Events[0].Target != "httpbin.org:443" {
		t.Fatalf("report.json row must carry the block with the denial, got %+v", p)
	}
}

// The log line is written by Run itself (the wiring), not only by the helper: a Loki that refuses the
// sandbox read leaves the row `unavailable` AND a WARN line with the row's correlation id.
func TestRun_LogsAnUnavailableSandboxWindow(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/query_range" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loki.Close()
	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n"+
		"observability:\n  openshell:\n    source: loki\n    selector: '{job=\"openshell-gateway\"}'\n    sandbox: sb-1\n    window_pad: 10ms\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = loki.URL
	e.Pushgateway = loki.URL
	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "sandbox policy evidence is not complete") {
			found = strings.Contains(l, `"coverage":"unavailable"`) && strings.Contains(l, "HTTP 401") &&
				strings.Contains(l, `"correlation_id":"tr-`) && strings.Contains(l, `"scenario_id":"MCP-001"`)
		}
	}
	if !found {
		t.Fatalf("Run must log the unavailable window with its reason and correlation id; log was:\n%s", buf.String())
	}
}
