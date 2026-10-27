package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	argusauth "github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// AC-D70: the stub EXECUTOR for the router's tests. It is the executor's own protocol code — a REAL mcpserver with the
// executor's real tools (mcpserver.DefaultTools) and its real static-token auth (the product token gives the product hat,
// the test token the test hat, anything else HTTP 401 + -32001) — behind its real HTTP handler, with a recorder in front
// (every request: method, tool, bearer, host, arguments) and switchable failure modes. Only the run tool's work is
// replaced: its handler records the run id the executor's dispatcher minted instead of starting JMeter. get_report is the
// real one, reading a real results folder, so the product hat's redaction is the executor's own.

// sentinelExpected is the expected value in every stub's fixture report: the test hat sees it, the product hat must not.
const sentinelExpected = "SENTINEL-EXPECTED-AC-D70"

type stubExec struct {
	t       *testing.T
	ts      *httptest.Server
	env     toolcore.Env
	mu      sync.Mutex
	srv     *mcpserver.Server
	h       http.Handler
	mode    string // "" | "hang" | "bodyhang" | "drop" | "calldrop" | "status:<n>" | "callstatus:<n>" | "rpcerr" | "badanswer" | "rawlist" | "unknowntool"
	rawList string // the tools/list result for mode "rawlist"
	calls   []stubCall
	minted  []string      // run ids the executor's dispatcher minted (RunBeginner)
	ran     []string      // run ids the run tool's handler received
	hold    chan struct{} // when set, the run tool's handler waits on it (the run lock stays held)

	// AC-D70.2: the body of a "status:<n>" / "callstatus:<n>" answer ("" = the proxy page), and the JSON-RPC message of
	// an "rpcerr" list answer ("" = "internal").
	statusBody, rpcMsg string
}

// holdRuns makes every run wait until the returned func is called — so a second run meets the executor's real
// "run already in progress" refusal.
func (e *stubExec) holdRuns() (release func()) {
	ch := make(chan struct{})
	e.mu.Lock()
	e.hold = ch
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// newStubExec starts an executor for one instance, accepting exactly the two given tokens.
func newStubExec(t *testing.T, version, productTok, testTok string) *stubExec {
	t.Helper()
	e := &stubExec{t: t, env: fixtureEnv(t)}
	e.serve(version, productTok, testTok, nil)
	e.ts = httptest.NewServer(e)
	t.Cleanup(e.ts.Close)
	return e
}

// serve (re)builds the executor's server: its version and tools — the NEW executor's real tools when tools is nil.
func (e *stubExec) serve(version, productTok, testTok string, tools []mcpserver.Tool) {
	if tools == nil {
		tools = e.newTools()
	}
	srv, err := mcpserver.NewServer(argusauth.Config{RunnerToken: productTok, AuthorToken: testTok}, tools...)
	if err != nil {
		e.t.Fatal(err)
	}
	srv.Version = version
	srv.RunBeginner = func(runID, _ string) error {
		e.mu.Lock()
		e.minted = append(e.minted, runID)
		e.mu.Unlock()
		return nil
	}
	e.mu.Lock()
	e.srv, e.h = srv, mcpserver.NewHTTPHandler(srv)
	e.mu.Unlock()
}

// newTools is the executor's real tool set, with the run tool's work replaced by a recorder.
func (e *stubExec) newTools() []mcpserver.Tool {
	tools := mcpserver.DefaultTools(e.env)
	for i := range tools {
		if tools[i].Name == "runner_run" {
			tools[i].Handler = func(args json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
				var a struct {
					RunID string `json:"run_id"`
				}
				_ = json.Unmarshal(args, &a)
				e.mu.Lock()
				e.ran = append(e.ran, a.RunID)
				hold := e.hold
				e.mu.Unlock()
				if hold != nil {
					<-hold
				}
				return mcpserver.Ok(map[string]any{"ran": a.RunID})
			}
		}
	}
	return tools
}

// oldTools is an executor from before the rename: the same tools under their two-underscore names, with the old texts,
// and no aliases — what 0.3.71 and older serve. (Its tools/list, byte for byte, is testdata/executor-tools-list-f88b9f13.json.)
func (e *stubExec) oldTools() []mcpserver.Tool {
	tools := e.newTools()
	for i := range tools {
		tools[i].Name = strings.Replace(tools[i].Name, "runner_", "runner__", 1)
		tools[i].Aliases = nil
		tools[i].Description = strings.ReplaceAll(tools[i].Description, "runner_", "runner__")
	}
	return tools
}

func (e *stubExec) setMode(mode string) {
	e.mu.Lock()
	e.mode = mode
	e.mu.Unlock()
}

func (e *stubExec) setStatusBody(body string) {
	e.mu.Lock()
	e.statusBody = body
	e.mu.Unlock()
}

func (e *stubExec) setRPCMessage(msg string) {
	e.mu.Lock()
	e.rpcMsg = msg
	e.mu.Unlock()
}

func (e *stubExec) setRawList(raw string) {
	e.mu.Lock()
	e.mode, e.rawList = "rawlist", raw
	e.mu.Unlock()
}

// URL is the executor's MCP endpoint, as onboarding records it.
func (e *stubExec) URL() string { return e.ts.URL + "/mcp" }

func (e *stubExec) count(method string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// toolCalls is every tools/call the executor received, in order.
func (e *stubExec) toolCalls() []stubCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []stubCall
	for _, c := range e.calls {
		if c.Method == "tools/call" {
			out = append(out, c)
		}
	}
	return out
}

func (e *stubExec) lastCall() stubCall {
	if c := e.toolCalls(); len(c) > 0 {
		return c[len(c)-1]
	}
	return stubCall{}
}

func (e *stubExec) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	e.mu.Lock()
	e.calls = append(e.calls, stubCall{Method: req.Method, Tool: req.Params.Name, Bearer: r.Header.Get("Authorization"),
		Host: r.Host, Args: req.Params.Arguments})
	mode, raw, h, statusBody, rpcMsg := e.mode, e.rawList, e.h, e.statusBody, e.rpcMsg
	e.mu.Unlock()
	if statusBody == "" {
		statusBody = "<html>upstream proxy page</html>"
	}
	if rpcMsg == "" {
		rpcMsg = "internal"
	}

	w.Header().Set("Content-Type", "application/json")
	switch {
	case mode == "hang":
		<-r.Context().Done()
		return
	case mode == "bodyhang" && req.Method == "tools/call": // the headers come, the body never does
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	case mode == "drop" || (mode == "calldrop" && req.Method == "tools/call"):
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		e.t.Error("drop: the connection could not be hijacked")
		return
	case strings.HasPrefix(mode, "status:"), strings.HasPrefix(mode, "callstatus:") && req.Method == "tools/call":
		var code int
		fmt.Sscanf(mode[strings.Index(mode, ":")+1:], "%d", &code)
		w.WriteHeader(code)
		io.WriteString(w, statusBody)
		return
	case mode == "rpcerr" && req.Method == "tools/list":
		m, _ := json.Marshal(rpcMsg)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":%s}}`, req.ID, m)
		return
	case mode == "badanswer" && req.Method == "tools/list":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, req.ID)
		return
	case mode == "rawlist" && req.Method == "tools/list":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, raw)
		return
	case mode == "unknowntool" && req.Method == "tools/call":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unknown tool %s"}}`, req.ID, req.Params.Name)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.ServeHTTP(w, r)
}

// direct drives the executor's own server, as an agent wired STRAIGHT to it would: the reference for "the same answer".
func (e *stubExec) direct(t *testing.T, tok string, id int, method string, params any) []byte {
	t.Helper()
	e.mu.Lock()
	srv := e.srv
	e.mu.Unlock()
	srv.AdoptSession("direct-" + tok)
	raw, _ := srv.Dispatch("direct-"+tok, tok, rpcReq(id, method, params))
	return raw
}

// fixtureEnv is a results folder holding one FAILED scenario whose expected value is the sentinel.
func fixtureEnv(t *testing.T) toolcore.Env {
	t.Helper()
	tmp := t.TempDir()
	writeReport(t, filepath.Join(tmp, "local", "report.json"), "last-run")
	return toolcore.Env{Instance: "local", ResultsRoot: tmp, ScenariosDir: tmp, Grafana: "http://grafana", Loki: "http://127.0.0.1:9"}
}

// writeReport writes a report for runID with the sentinel expected value at path.
func writeReport(t *testing.T, path, runID string) {
	t.Helper()
	exp := sentinelExpected
	rep := report.Report{Project: "fixture", RunID: runID, Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{{ID: "ORD-X", Status: "failed",
			CorrelationID: "tr-" + runID + "-ORD-X-1a2b3c", Failure: &report.Failure{Expected: &exp, Observed: "status 200"}}}}}}
	b, _ := json.MarshalIndent(&rep, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// closedURL is an executor address nothing listens on.
func closedURL() string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	a := ln.Addr().String()
	ln.Close()
	return "http://" + a + "/mcp"
}

// toolErrText reads the text of a tool error answer (the router's refusals are {"error": text}).
func toolErrorText(t *testing.T, raw []byte) string {
	t.Helper()
	var r struct {
		Result mcpserver.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || !r.Result.IsError || len(r.Result.Content) != 1 {
		t.Fatalf("not a tool error: %s", raw)
	}
	var p struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &p); err != nil {
		t.Fatalf("a tool error whose text is not {\"error\":…}: %s", raw)
	}
	return p.Error
}

// waitUntil polls cond for up to d.
func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
