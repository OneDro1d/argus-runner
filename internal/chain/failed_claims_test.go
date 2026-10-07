package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// / — the AUTHOR report of a failed chain step names each claim that
// did not hold (as written) with the value observed for it on the LAST attempt. These tests read the
// field through the step's JSON (`failed_claims`), which is what the author's report carries, so they
// compile and fail by assertion on a tree that has no such field.

type fcEntry struct {
	Claim    string `json:"claim"`
	Observed string `json:"observed"`
}

func failedClaimsOf(t *testing.T, st report.StepResult) []fcEntry {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	raw, ok := m["failed_claims"]
	if !ok {
		return nil
	}
	var out []fcEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func httpStepWith(url string, status int, poll *Poll, want ...mcp.BodyAssert) Step {
	return HTTPStep("check", "GET", url+"/b", nil, "", status, want, nil, poll, false, nil, &scenario.MoneySpendLedger{})
}

func TestFailedClaims_HTTP_NamesTheClaimThatMissedAndOmitsTheOneThatHeld(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 3, "name": "ok"}`}})
	missed := mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "5"}
	held := mcp.BodyAssert{Field: "name", Op: mcp.BodyEqualsOp, Value: "ok"}
	st := httpStepWith(srv.URL, 200, nil, held, missed).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("want a failed step, got %+v", st)
	}
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field count > 5" || got[0].Observed != "3" {
		t.Fatalf("want exactly [field count > 5 observed 3], got %+v", got)
	}
	for _, e := range got {
		if strings.Contains(e.Claim, "name") {
			t.Errorf("a claim that held must not be listed: %+v", e)
		}
	}
}

func TestFailedClaims_HTTP_PolledStepReportsTheLastAttempt(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 1}`, `{"count": 2}`, `{"count": 3}`}})
	poll := &Poll{Timeout: 150 * time.Millisecond, Interval: 5 * time.Millisecond}
	st := httpStepWith(srv.URL, 200, poll, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "99"}).Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.Contains(st.Observed, "poll timed out") {
		t.Fatalf("want a poll timeout, got %+v", st)
	}
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Observed != "3" {
		t.Fatalf("a polled step must report the LAST attempt's value (3, not the first 1): %+v", got)
	}
}

func TestFailedClaims_HTTP_SavedClaimShowsThePlaceholderNeverTheValue(t *testing.T) {
	// n is saved as secretN; the SUT answers 3, so `count > ${saved.n}` misses.
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 3}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"}).
		Run("cid", map[string]string{"n": secretN})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field count > ${saved.n}" || got[0].Observed != "3" {
		t.Fatalf("want the claim AS WRITTEN with the observed 3, got %+v", got)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), secretN) {
		t.Errorf("the saved value %q is printed: %s", secretN, b)
	}
}

// ( the former "observed equal to the saved value is not printed" test is replaced by
// TestFailedClaims_HTTP_NumericSavedClaim_LongEqualNumberShowsTheNumber — a numeric threshold is
// validated finite, so it is not a credential.)

func TestFailedClaims_HTTP_AnySavedValueEchoedInTheBodyIsNotPrinted(t *testing.T) {
	// An EARLIER step saved a token; this step's unscoped claim does not reference it, misses, and the
	// SUT's answer echoes the token. The observed text is the whole answer, so without a chain-wide
	// scrub the token is printed. A short saved value (n = 3) is left alone: replacing every "3" in an
	// answer would garble it, and a value that short is not a credential.
	const token = "tok-ABCDEF-123456"
	srv := newSeqServer(t, map[string][]string{"/b": {`{"echo":"` + token + `","count":3}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Op: mcp.BodyContainsOp, Value: "zzz"}).
		Run("cid", map[string]string{"tok": token, "n": "3"})
	got := failedClaimsOf(t, st)
	if len(got) != 1 {
		t.Fatalf("want one failed claim, got %+v", got)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), token) {
		t.Errorf("a saved value the claim does not reference is printed: %s", b)
	}
	if !strings.Contains(got[0].Observed, "${saved.tok}") || !strings.Contains(got[0].Observed, `"count":3`) {
		t.Errorf("want the token as its placeholder and the short value untouched, got %q", got[0].Observed)
	}
}

func TestFailedClaims_HTTP_StatusClaimIsListedWithTheObservedStatus(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{}) // every path: 404
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "1"}).Run("cid", map[string]string{})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "status = 200" || got[0].Observed != "404" {
		t.Fatalf("want [status = 200 observed 404] (the body was never evaluated), got %+v", got)
	}
}

func TestFailedClaims_HTTP_BoundedInCountAndLength(t *testing.T) {
	long := strings.Repeat("x", 2000)
	srv := newSeqServer(t, map[string][]string{"/b": {long}})
	var want []mcp.BodyAssert
	for i := 0; i < 12; i++ {
		want = append(want, mcp.BodyAssert{Op: mcp.BodyContainsOp, Value: "absent-" + string(rune('a'+i))})
	}
	st := httpStepWith(srv.URL, 200, nil, want...).Run("cid", map[string]string{})
	got := failedClaimsOf(t, st)
	// The documented bounds (report.StepResult.FailedClaims): 10 entries, 200 bytes of observed
	// value, then the suffix below. Pinned here as literals so changing either is a visible decision.
	const maxEntries, maxObserved, suffix = 10, 200, "... (truncated)"
	if len(got) != maxEntries {
		t.Fatalf("want exactly %d entries (12 missed), got %d", maxEntries, len(got))
	}
	for _, e := range got {
		if len(e.Observed) > maxObserved+len(suffix) {
			t.Fatalf("an observed value is %d bytes, over the documented %d: %q", len(e.Observed), maxObserved, e.Observed[:40])
		}
		if !strings.HasSuffix(e.Observed, suffix) {
			t.Fatalf("a cut observed value must say so: %q", e.Observed[len(e.Observed)-30:])
		}
	}
	if got[0].Claim != `content contains "absent-a"` {
		t.Errorf("entries keep the order the claims were written in, got first %q", got[0].Claim)
	}
}

func TestFailedClaims_HTTP_TextPointsAtTheRealField(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 3}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "5"}).Run("cid", map[string]string{})
	if strings.Contains(st.Observed, "failure record") {
		t.Errorf("the text still points at a record that does not exist: %q", st.Observed)
	}
	if !strings.Contains(st.Observed, "failed_claims") {
		t.Errorf("the text must name the real field, failed_claims: %q", st.Observed)
	}
}

func TestFailedClaims_HTTP_APassingStepCarriesNone(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 9}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "5"}).Run("cid", map[string]string{})
	if st.Status != "passed" || len(failedClaimsOf(t, st)) != 0 {
		t.Fatalf("a passing step lists no failed claims: %+v", st)
	}
}

// ── mcp ──

func mcpServerReturning(text string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
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
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}})
		}
	}))
}

func TestFailedClaims_MCP_NamesTheBodyClaimThatMissed(t *testing.T) {
	ts := mcpServerReturning(`{"count": 3, "name": "ok"}`)
	defer ts.Close()
	cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: "x"}
	expect := mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{
		{Field: "name", Op: mcp.BodyEqualsOp, Value: "ok"},
		{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"},
	}}
	st := MCPStep("m", cl, "t", `{}`, expect, nil).Run("cid", map[string]string{"n": secretN})
	if st.Status != "failed" {
		t.Fatalf("want failed, got %+v", st)
	}
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field count > ${saved.n}" || got[0].Observed != "3" {
		t.Fatalf("want [field count > ${saved.n} observed 3], got %+v", got)
	}
	if strings.Contains(st.Observed, "failure record") || !strings.Contains(st.Observed, "failed_claims") {
		t.Errorf("the text must point at failed_claims, not a failure record: %q", st.Observed)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), secretN) {
		t.Errorf("the saved value is printed: %s", b)
	}
}

// ── amqp ──

func TestFailedClaims_AMQP_ConsumeBodyClaim(t *testing.T) {
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"count": 3, "tag": "v7"}`)}
	dial, _ := dialerFor(f)
	want := scenario.AMQPStepWant{Broker: accepts, Body: []mcp.BodyAssert{
		{Field: "tag", Op: mcp.BodyEqualsOp, Value: "v7"},
		{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"},
	}}
	st := AMQPStep("take", spec, want, dial).Run("cid", map[string]string{"n": secretN})
	if st.Status != "failed" {
		t.Fatalf("want failed, got %+v", st)
	}
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field count > ${saved.n}" || got[0].Observed != "3" {
		t.Fatalf("want [field count > ${saved.n} observed 3], got %+v", got)
	}
	if strings.Contains(st.Observed, "failure record") || !strings.Contains(st.Observed, "failed_claims") {
		t.Errorf("the text must point at failed_claims: %q", st.Observed)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), secretN) {
		t.Errorf("the saved value is printed: %s", b)
	}
}

func TestFailedClaims_AMQP_QueueIsEmptyClaim(t *testing.T) {
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`hello-there`)}
	dial, _ := dialerFor(f)
	st := AMQPStep("take", spec, scenario.AMQPStepWant{Empty: true}, dial).Run("cid", map[string]string{})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "queue is empty" || !strings.Contains(got[0].Observed, "hello-there") {
		t.Fatalf("want [queue is empty, observed the message that arrived], got %+v", got)
	}
}

// ── the chain result keeps it on the step ──

func TestFailedClaims_ChainRunKeepsItOnTheFailedStep(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 3}`}})
	res := Run("cid", []Step{httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "5"})})
	if res.Status != "failed" || len(res.Steps) != 1 {
		t.Fatalf("want one failed step, got %+v", res)
	}
	if got := failedClaimsOf(t, res.Steps[0]); len(got) != 1 || got[0].Observed != "3" {
		t.Fatalf("the step inside the chain result must carry it: %+v", got)
	}
}
