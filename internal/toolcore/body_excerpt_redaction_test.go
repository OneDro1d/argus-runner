package toolcore

// StepResult.BodyExcerpt is AUTHOR-ONLY and the builder-visible text of a
// status miss does not name the wanted status. Wanted status 299: no digit run of it collides with anything.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

func TestBodyExcerpt_AuthorReadsItBuilderDoesNot(t *testing.T) {
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"SENTINEL-MESSAGE-9Z2"}`))
	}))
	defer sut.Close()
	step := chain.HTTPStep("check", "GET", sut.URL+"/b", nil, "", 299, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	res := chain.Run("tr-x", []chain.Step{step})
	if res.Status != "failed" {
		t.Fatalf("want a failed chain, got %+v", res)
	}
	// Mode "build": with none the product hat is withheld every scenario and an absence proves nothing.
	rep := &report.Report{RunID: "run_1", Mode: "build", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "Chain", Scenarios: []report.ScenarioResult{res}}}}
	e := fcEnv(t, rep)

	author := fcGet(t, e, role.Test, "run_1")
	if !strings.Contains(author, `"body_excerpt":`) || !strings.Contains(author, "SENTINEL-MESSAGE-9Z2") {
		t.Fatalf("the author's report must carry body_excerpt with the message: %s", author)
	}
	if !strings.Contains(author, "status = 299") {
		t.Errorf("the author keeps the wanted status in the failed claim: %s", author)
	}
	builder := fcGet(t, e, role.Product, "run_1")
	// POSITIVE CONTROL first: the builder reads the step and its observed status.
	if !strings.Contains(builder, `"name":"check"`) || !strings.Contains(builder, "422") {
		t.Fatalf("the builder must read the step and the observed 422, or the absences below prove nothing: %s", builder)
	}
	for _, needle := range []string{"body_excerpt", "SENTINEL-MESSAGE-9Z2", "299"} {
		if strings.Contains(builder, needle) {
			t.Errorf("the builder's report carries %q: %s", needle, builder)
		}
	}

	// the exported RedactExpected (the build record's path), on the raw struct
	cp := *rep
	b, _ := json.Marshal(rep)
	_ = json.Unmarshal(b, &cp)
	RedactExpected(&cp, role.Product)
	out, _ := json.Marshal(cp)
	if strings.Contains(string(out), "body_excerpt") || strings.Contains(string(out), "SENTINEL-MESSAGE-9Z2") {
		t.Errorf("RedactExpected(product) left the excerpt: %s", out)
	}
}
