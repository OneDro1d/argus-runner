package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// http_numeric_test.go — item 25(b): numeric comparisons (`>`, `>=`, `<`, `<=`) on a chain `http`
// step's JSON response body, end to end through chain.HTTPStep and mcp.Judge's shared grammar.

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
}

func TestHTTPStep_NumericComparison_GreaterThanPasses(t *testing.T) {
	ts := jsonServer(t, `{"latency_ms": 55}`)
	defer ts.Close()

	bodyWant := []mcp.BodyAssert{{Field: "latency_ms", Op: mcp.BodyGTOp, Value: "50"}}
	step := HTTPStep("check", "GET", ts.URL+"/x", nil, "", 200, bodyWant, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if res.Status != "passed" {
		t.Fatalf("55 > 50 must pass: %+v", res)
	}
	if !strings.Contains(strings.Join(res.AssertionsEnforced, "; "), "latency_ms > 50") {
		t.Errorf("assertions_enforced must record the numeric claim: %+v", res.AssertionsEnforced)
	}
}

func TestHTTPStep_NumericComparison_GreaterThanFails(t *testing.T) {
	ts := jsonServer(t, `{"latency_ms": 40}`)
	defer ts.Close()

	bodyWant := []mcp.BodyAssert{{Field: "latency_ms", Op: mcp.BodyGTOp, Value: "50"}}
	step := HTTPStep("check", "GET", ts.URL+"/x", nil, "", 200, bodyWant, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if res.Status != "failed" {
		t.Fatalf("40 > 50 must fail: %+v", res)
	}
	// item 25 holdout: the threshold (50) must never appear in Observed.
	if strings.Contains(res.Observed, "50") {
		t.Errorf("Observed must not echo the threshold: %q", res.Observed)
	}
}

func TestHTTPStep_NumericComparison_GTE_LTE_boundaries(t *testing.T) {
	cases := []struct {
		op   string
		val  string
		body string
		pass bool
	}{
		{mcp.BodyGTEOp, "50", `{"n": 50}`, true},
		{mcp.BodyGTEOp, "50", `{"n": 49}`, false},
		{mcp.BodyLTOp, "50", `{"n": 49}`, true},
		{mcp.BodyLTOp, "50", `{"n": 50}`, false},
		{mcp.BodyLTEOp, "50", `{"n": 50}`, true},
		{mcp.BodyLTEOp, "50", `{"n": 50.01}`, false},
	}
	for _, c := range cases {
		ts := jsonServer(t, c.body)
		bodyWant := []mcp.BodyAssert{{Field: "n", Op: c.op, Value: c.val}}
		step := HTTPStep("check", "GET", ts.URL+"/x", nil, "", 200, bodyWant, nil, nil, false,
			nil, &scenario.MoneySpendLedger{})
		res := step.Run("cid", map[string]string{})
		ts.Close()

		wantStatus := "failed"
		if c.pass {
			wantStatus = "passed"
		}
		if res.Status != wantStatus {
			t.Errorf("op=%s val=%s body=%s: status=%q, want %q (%+v)", c.op, c.val, c.body, res.Status, wantStatus, res)
		}
	}
}

// TestHTTPStep_NumericComparison_NonNumericObservedNamesTheReason: a non-numeric field value must
// fail with a reason that SAYS SO — distinct from an ordinary threshold miss — while never
// revealing the threshold or the field's actual (non-numeric) value.
func TestHTTPStep_NumericComparison_NonNumericObservedNamesTheReason(t *testing.T) {
	ts := jsonServer(t, `{"latency_ms": "fast"}`)
	defer ts.Close()

	bodyWant := []mcp.BodyAssert{{Field: "latency_ms", Op: mcp.BodyGTOp, Value: "50"}}
	step := HTTPStep("check", "GET", ts.URL+"/x", nil, "", 200, bodyWant, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if res.Status != "failed" {
		t.Fatalf("a non-numeric observed value must fail: %+v", res)
	}
	if !strings.Contains(res.Observed, "non-numeric") {
		t.Errorf("Observed must name the non-numeric reason: %q", res.Observed)
	}
	if strings.Contains(res.Observed, "50") || strings.Contains(res.Observed, "fast") {
		t.Errorf("Observed must not echo the threshold or the observed value: %q", res.Observed)
	}
}
