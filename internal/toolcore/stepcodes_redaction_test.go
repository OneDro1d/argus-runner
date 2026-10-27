package toolcore

// AC-F2.1 — a chain step's observed_status (http) and observed_sqlstate (db) are the SYSTEM'S OWN BEHAVIOUR, like
// elapsed_ms: BOTH hats read them (redactExpected leaves them). The builder's read of a failed run carries both, and
// still none of the expected values; the author's read of the same file carries both too.

import (
	"context"
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

type refusingQuerier struct{}

func (refusingQuerier) Query(context.Context, string, []string, time.Duration, int) (chain.DBRows, error) {
	return chain.DBRows{}, &chain.DBServerError{SQLState: "42P01", Message: `relation "hidden_tbl_acf21" does not exist`}
}
func (refusingQuerier) Close() error { return nil }

func TestStepCodes_BothHatsReadTheStatusAndTheSQLSTATE(t *testing.T) {
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer sut.Close()
	httpStep := chain.HTTPStep("call", "GET", sut.URL+"/b", nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	httpRes := chain.Run("tr-h", []chain.Step{httpStep})
	asserts, err := scenario.DBStepClaims([]string{"body has row_count >= 1"})
	if err != nil {
		t.Fatal(err)
	}
	dbStep := chain.DBStep("q", chain.DBSpec{Query: "SELECT n FROM hidden_tbl_acf21"}, asserts, nil, nil, false,
		func(context.Context, chain.DBConn) (chain.DBQuerier, error) { return refusingQuerier{}, nil })
	dbRes := chain.Run("tr-d", []chain.Step{dbStep})
	httpRes.ID, dbRes.ID = "S-HTTP", "S-DB"
	if httpRes.Steps[0].ObservedStatus != 500 || dbRes.Steps[0].ObservedSQLState != "42P01" {
		t.Fatalf("setup: the steps must carry 500 and 42P01: %+v / %+v", httpRes.Steps[0], dbRes.Steps[0])
	}
	// Mode "build": a certifying run withholds every scenario from a builder (the checks below would pass vacuously).
	rep := &report.Report{RunID: "run_1", Mode: "build", Summary: report.Summary{Total: 2, Failed: 2},
		Layers: []report.Layer{{Layer: "Chain", Scenarios: []report.ScenarioResult{httpRes, dbRes}}}}
	e := fcEnv(t, rep)

	for hat, doc := range map[string]string{"builder": fcGet(t, e, role.Product, "run_1"), "author": fcGet(t, e, role.Test, "run_1")} {
		for _, want := range []string{`"observed_status":500`, `"observed_sqlstate":"42P01"`} {
			if !strings.Contains(doc, want) {
				t.Errorf("the %s's report must carry %s: %s", hat, want, doc)
			}
		}
	}
	builder := fcGet(t, e, role.Product, "run_1")
	// the builder still gets no expected value and no database message
	for _, leak := range []string{`"failed_claims":`, `"assertions_enforced":`, `"expected":`, "status = 200", "hidden_tbl_acf21", "does not exist"} {
		if strings.Contains(builder, leak) {
			t.Errorf("the builder's report carries %q: %s", leak, builder)
		}
	}
}
