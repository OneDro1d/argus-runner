package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// What a BUILDER sees of a failed mcp step that reads a gateway's answer. The claim is the test's
// expected value, the pattern is the author's, and the answer is the system's own data: the product hat gets none of
// them, and the relayed report (which has no field for any of them) carries none either.

func gatewayServer(text string, isError bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}})
		}
	}))
}

const (
	gwClaimed = "CLAIMED-VALUE-PAID"
	gwRowVal  = "ROW-VALUE-PENDING-7731"
	gwPattern = "PATTERN-SENTINEL-UNAVAIL"
	gwHubRx   = `(?s)<untrusted-data-[0-9a-f-]+>(\[.*\])</untrusted-data-`
)

var resultPath = "result"

func TestFailedMCPGatewayStep_ProductHatAndRelayCarryNoAnswerNoPatternNoClaim(t *testing.T) {
	hub := "sentence <untrusted-data-ab12>[{\"secret_status\":\"" + gwRowVal + "\"}]</untrusted-data-ab12> sentence"
	cases := map[string]struct {
		answer  string
		isError bool
		opt     chain.MCPOptions
		claim   string
		want    string // the builder-visible sentence it still gets
	}{
		"a claim that missed": {hub, false, chain.MCPOptions{Extract: gwHubRx, Unavailable: gwPattern},
			"body extracted has 0.secret_status equals " + gwClaimed, "content assertion"},
		"no match for the extract": {"ANSWER-SENTINEL-NOSHAPE " + gwRowVal, false, chain.MCPOptions{Extract: gwHubRx},
			"body extracted has 0.secret_status equals " + gwClaimed, chain.MCPNoExtractObserved},
		"path leads nowhere": {"ANSWER-SENTINEL-NOSHAPE " + gwRowVal, false, chain.MCPOptions{Extract: gwHubRx, ExtractPath: &resultPath},
			"body extracted has 0.secret_status equals " + gwClaimed, chain.MCPNoExtractPathObserved},
		"unavailable": {gwPattern + " " + gwRowVal, true, chain.MCPOptions{Extract: gwHubRx, Unavailable: gwPattern},
			"body extracted has 0.secret_status equals " + gwClaimed, chain.MCPUnavailableObserved},
		"poll ran out": {hub, false, chain.MCPOptions{Extract: gwHubRx, Poll: &chain.Poll{Timeout: 30 * time.Millisecond, Interval: 5 * time.Millisecond}, Idempotent: true},
			"body extracted has 0.secret_status equals " + gwClaimed, "poll timed out"},
		"unavailable until the poll ran out": {gwPattern + " " + gwRowVal, true, chain.MCPOptions{Extract: gwHubRx, Unavailable: gwPattern,
			Poll: &chain.Poll{Timeout: 30 * time.Millisecond, Interval: 5 * time.Millisecond}, Idempotent: true},
			"body extracted has 0.secret_status equals " + gwClaimed, "still unavailable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := gatewayServer(tc.answer, tc.isError)
			defer ts.Close()
			asserts, errs := scenario.ParseMCPStepBodyAsserts([]string{tc.claim})
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable}
			step := chain.MCPStepWith("q", cl, "execute_sql", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: asserts}, nil, tc.opt)
			res := chain.Run("tr-1", []chain.Step{step})
			res.ID = "S-1"
			if res.Status == "passed" {
				t.Fatalf("setup: the step must not pass: %+v", res)
			}
			author, _ := json.Marshal(res)
			// control: the author hat DOES carry the answer or the claim somewhere, so the checks below can fail
			if !strings.Contains(string(author), gwRowVal) && !strings.Contains(string(author), gwClaimed) {
				t.Fatalf("control: the author's report carries the row or the claim: %s", author)
			}

			rep := &report.Report{Mode: "build", RunID: "r1"}
			rep.Summary.Failed, rep.Summary.Total = 1, 1
			rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{res}}}
			relayed, _ := json.Marshal(reduceReport(rep, ""))
			toolcore.RedactExpected(rep, role.Product)
			product, _ := json.Marshal(rep)
			for doc, text := range map[string]string{"product-hat report": string(product), "relayed report": string(relayed)} {
				for _, leak := range []string{gwClaimed, gwRowVal, gwPattern, "ANSWER-SENTINEL-NOSHAPE", "secret_status", "untrusted-data"} {
					if strings.Contains(text, leak) {
						t.Errorf("%s carries %q: %s", doc, leak, text)
					}
				}
			}
			if tc.want != "" && !strings.Contains(string(product), tc.want) {
				t.Errorf("the builder keeps the reality-only sentence %q: %s", tc.want, product)
			}
		})
	}
}
