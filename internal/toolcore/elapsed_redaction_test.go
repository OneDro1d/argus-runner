package toolcore

// AN `elapsed` BOUND IS AN EXPECTED VALUE. With the product hat a report whose step failed
// an elapsed claim carries the MEASURED duration (the system's own behaviour: step.elapsed_ms and the
// observed sentence) and the bound NOWHERE. The author's read of the same file carries the bound, in the
// failed-claim record, as the positive control.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

func TestElapsed_ProductHatSeesTheDurationAndNeverTheBound(t *testing.T) {
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer sut.Close()
	// A bound of 1 ms: the digits "1" would collide with a duration like 151, so the search below is for the
	// FORMS a bound is written in (`< 1 ms`, `<= 1 ms`, `bound 1 ms`, `1ms`), none of which a duration is.
	claim := scenario.ElapsedClaim{MaxMs: 1}
	step := chain.HTTPStep("check", "GET", sut.URL+"/b", nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{},
		chain.WithElapsed(&claim))
	res := chain.Run("tr-e", []chain.Step{step})
	if res.Status != "failed" {
		t.Fatalf("a 150 ms answer must fail `elapsed < 1 ms`, got %+v %+v", res.Status, res.Steps)
	}
	st := res.Steps[0]
	if st.Status != "failed" || st.ElapsedMs == nil || *st.ElapsedMs < 100 {
		t.Fatalf("want a failed step that recorded its duration, got %+v", st)
	}
	// Mode "build": a certifying run withholds every scenario from a builder, which would make the checks
	// below pass vacuously (the positive control — the duration — would fail, which is how this was caught).
	rep := &report.Report{RunID: "run_1", Mode: "build", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "Chain", Scenarios: []report.ScenarioResult{res}}}}
	e := fcEnv(t, rep)

	builder := fcGet(t, e, role.Product, "run_1")
	var tree any
	if err := json.Unmarshal([]byte(builder), &tree); err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"< 1 ms", "<= 1 ms", "<1ms", "<= 1ms", "bound 1", "bound 1", "elapsed <", "elapsed ="} {
		if strings.Contains(builder, form) {
			t.Errorf("the builder's report carries the bound in the form %q: %s", form, builder)
		}
	}
	for _, field := range []string{`"failed_claims":`, `"assertions_enforced":`, `"expected":`} {
		if strings.Contains(builder, field) {
			t.Errorf("the builder's report carries %s: %s", field, builder)
		}
	}
	// the measured duration IS builder-readable, twice: the field and the sentence
	if !strings.Contains(builder, `"elapsed_ms":`) || !strings.Contains(builder, "the response took ") ||
		!strings.Contains(builder, "a timing claim did not hold") {
		t.Errorf("the builder must read the duration and that a timing claim failed: %s", builder)
	}

	// positive control: the author's read of the same file carries the bound, in the failed-claim record
	author := fcGet(t, e, role.Test, "run_1")
	if !strings.Contains(author, "bound 1 ms") {
		t.Errorf("the author's report must carry the bound in failed_claims: %s", author)
	}
}
