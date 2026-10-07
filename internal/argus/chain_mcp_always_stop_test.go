package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D20 gate, mcp twin of TestHTTPStep_AlwaysStepStillFiresAfterTheSUTStopsAnswering.
// Every cleanup step the suite ships is an mcp step, and until this test `"always": true` on one was
// parsed, validated and then dropped — so the delete never fired after a VR12-CH2 stop, which is the
// break mode that leaked `race-s3-…` namespaces (internal/chain/chain.go rule 5). The namespace IS
// created, then the SUT stops answering; the `always` delete must still reach it, with the captured
// id, and a plain delete beside it must stay unfired.
func TestMCPStep_AlwaysStepStillFiresAfterTheSUTStopsAnswering(t *testing.T) {
	var alwaysDeletes, plainDeletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(text string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}})
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			switch req.Params.Name {
			case "create_namespace":
				reply(`{"id":"ns-9"}`)
			case "read_namespace":
				// no answer at all: drop the connection, so the client sees a transport error.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("httptest server must support hijacking")
				}
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			case "delete_namespace_plain":
				atomic.AddInt32(&plainDeletes, 1)
				reply(`{}`)
			case "delete_namespace":
				if req.Params.Arguments["id"] == "ns-9" {
					atomic.AddInt32(&alwaysDeletes, 1)
				}
				reply(`{}`)
			}
		}
	}))
	defer srv.Close()

	trig := `{"steps":[
		{"type":"mcp","transport":"streamable-http","server_url":"${BASE}","name":"create","tool":"create_namespace","args":{},"save":{"nsId":"id"}},
		{"type":"mcp","transport":"streamable-http","server_url":"${BASE}","name":"read","tool":"read_namespace","args":{"id":"${saved.nsId}"}},
		{"type":"mcp","transport":"streamable-http","server_url":"${BASE}","name":"plain-delete","tool":"delete_namespace_plain","args":{"id":"${saved.nsId}"}},
		{"type":"mcp","transport":"streamable-http","server_url":"${BASE}","name":"cleanup-delete","tool":"delete_namespace","args":{"id":"${saved.nsId}"},"always":true}
	]}`
	expect := "### Runnable\n- step create: result.isError == false\n- step read: result.isError == false\n" +
		"- step plain-delete: result.isError == false\n- step cleanup-delete: result.isError == false\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-mcp-always-stop", "testkit/ui")

	if got := atomic.LoadInt32(&alwaysDeletes); got != 1 {
		t.Fatalf("the always step must reach the SUT exactly once after the stop, with the captured id; delete count = %d; steps: %+v", got, res.Steps)
	}
	if got := atomic.LoadInt32(&plainDeletes); got != 0 {
		t.Errorf("a NON-always step after the stop must not fire, fired %d times", got)
	}
	if len(res.Steps) != 4 {
		t.Fatalf("want 4 steps, got %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[2].Status != report.StepNotMeasured {
		t.Errorf("a NON-always step after the stop must be not-measured, got %q", res.Steps[2].Status)
	}
	if res.Steps[3].Status != report.StepRanAfterFailureOK {
		t.Errorf("the always step must be reported as ran-after-failure, got %q", res.Steps[3].Status)
	}
	if res.Status == "passed" {
		t.Errorf("a chain whose SUT stopped answering must not pass, got %q", res.Status)
	}
}
