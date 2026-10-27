package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// What a BUILDER sees of a failed logs step. The lines are the system's own words, the claim is the
// test's expected value, and `contains` / `matching` are the author's filters: the product hat gets none of them,
// and the relayed report (which has no field for any of them) carries none either.

func TestFailedLogsStep_ProductHatAndRelayCarryNoLineNoClaimNoFilter(t *testing.T) {
	const (
		claimed  = "CLAIMED-LINE-TEXT"
		lineText = "LINE-FROM-THE-SYSTEM-7731"
		contains = "CONTAINS-FILTER-SENTINEL"
		matching = "MATCHING-FILTER-SENTINEL.*"
		label    = "secret-app-label"
	)
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"values":[["1000","` + lineText + `"]]}]}}`))
	}))
	defer loki.Close()
	asserts, err := scenario.LogsStepClaims([]string{"body has lines.0 equals " + claimed})
	if err != nil {
		t.Fatal(err)
	}
	step := chain.LogsStep("l", chain.LogsSpec{Scope: scenario.LogsScopeSource, Labels: map[string]string{"app": label},
		Contains: contains, Matching: matching, ChainStart: time.Now(), Sleep: func(time.Duration) {}},
		asserts, nil, nil, false, &obsquery.Loki{BaseURL: loki.URL})
	res := chain.Run("tr-1", []chain.Step{step})
	res.ID = "S-1"
	if res.Status != "failed" {
		t.Fatalf("setup: the step must fail on its claim, got %q (%+v)", res.Status, res.Steps)
	}
	// the author hat DOES see the claim and the observed line (that is the point of failed_claims) - a control that
	// the sentinels are really in the report before the redaction runs
	if b, _ := json.Marshal(res); !strings.Contains(string(b), claimed) || !strings.Contains(string(b), lineText) {
		t.Fatalf("control: the author's report must carry the claim and the observed line: %s", b)
	}

	rep := &report.Report{Mode: "build", RunID: "r1"}
	rep.Summary.Failed, rep.Summary.Total = 1, 1
	rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{res}}}

	// the relayed projection first: it never copies a step at all
	relayed, _ := json.Marshal(reduceReport(rep, ""))

	toolcore.RedactExpected(rep, role.Product)
	product, _ := json.Marshal(rep)
	t.Logf("PRODUCT-HAT REPORT OF THE FAILED LOGS STEP: %s", product)
	for name, doc := range map[string]string{"product-hat report": string(product), "relayed report": string(relayed)} {
		for _, leak := range []string{claimed, lineText, contains, matching, label, "{app="} {
			if strings.Contains(doc, leak) {
				t.Errorf("%s carries %q: %s", name, leak, doc)
			}
		}
	}
	// what the builder is still told: the step failed, and how many lines matched
	if !strings.Contains(string(product), "log query matched 1 line(s)") {
		t.Errorf("the builder keeps the reality-only Observed (the count): %s", product)
	}
}
