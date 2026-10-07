package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// singleCallRequestID: an id derived from the correlation id is made unique per call; a literal
// the author wrote is theirs.
func TestSingleCallRequestID(t *testing.T) {
	const corr = "tr-run1-SOC-001-0badf00d"
	for _, tc := range []struct {
		name, authored string
		wantPrefix     string // "" = must equal authored exactly
	}{
		{"none given", "", corr + "."},
		{"the payload's ${cid}", corr, corr + "."},
		{"built from ${cid}", "x-" + corr, "x-" + corr + "."},
		{"an author's literal", "fixed-id-7", ""},
	} {
		got := singleCallRequestID(tc.authored, corr)
		if tc.wantPrefix == "" {
			if got != tc.authored {
				t.Errorf("%s: sent %q, want the author's %q unchanged", tc.name, got, tc.authored)
			}
			continue
		}
		if !strings.HasPrefix(got, tc.wantPrefix) || len(got) != len(tc.wantPrefix)+8 {
			t.Errorf("%s: sent %q, want %s<8 hex>", tc.name, got, tc.wantPrefix)
		}
	}
}

// The run loop re-fires a throttled scenario ONCE with the same correlation id. Against a SUT that
// enforces request-id uniqueness the re-fire must not be refused as "request_id reused" — this drives
// the REAL runMCPScenario twice with one correlation id, the way the loop does, so a regression at the
// call site (sending the correlation id itself again) turns it red even if the helper is intact.
func TestRunMCPScenario_ReFireWithTheSameCorrelationIDIsNotAReusedRequestID(t *testing.T) {
	os.Unsetenv("MCP_URL")
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta struct {
					RequestID string `json:"request_id"`
				} `json:"_meta"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			mu.Lock()
			reused := false
			for _, s := range seen {
				reused = reused || s == req.Params.Meta.RequestID
			}
			seen = append(seen, req.Params.Meta.RequestID)
			mu.Unlock()
			text := "ok"
			if reused {
				text = "request_id reused"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": reused}})
		}
	}))
	defer ts.Close()

	const corr = "tr-run1-SOC-001-0badf00d"
	// The shipped social/memstore scenarios all write "request_id":"${cid}".
	s := scenario.Parse(mcpScenarioMDSplit(`{}`, "### Runnable\n- result.isError == false"))
	s.Trigger.Payload = strings.Replace(s.Trigger.Payload, `"args":{}`, `"args":{},"request_id":"${cid}"`, 1)
	if !strings.Contains(s.Trigger.Payload, `"request_id":"${cid}"`) {
		t.Fatalf("fixture did not take the request_id: %s", s.Trigger.Payload)
	}
	c := mcpCfg(t, ts.URL)
	for attempt := 1; attempt <= 2; attempt++ {
		if res := runMCPScenario(c, s, corr); res.Status != "passed" {
			t.Fatalf("attempt %d: status %q (%v) — the re-fire reused the request id", attempt, res.Status, res.Failure)
		}
	}
	if len(seen) != 2 || seen[0] == seen[1] || !strings.HasPrefix(seen[0], corr+".") || !strings.HasPrefix(seen[1], corr+".") {
		t.Fatalf("request ids sent = %v, want two distinct ids prefixed %q", seen, corr+".")
	}
}
