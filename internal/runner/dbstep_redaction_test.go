package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// What a BUILDER sees of a failed db step. The claim is the test's expected value, the query is
// the author's SQL, and a row is the system's own data: the product hat gets none of them, and the relayed report
// (which has no field for any of them) carries none either.

type redactionDB struct{ rows chain.DBRows }

func (d *redactionDB) Query(context.Context, string, []string, time.Duration, int) (chain.DBRows, error) {
	return d.rows, nil
}
func (d *redactionDB) Close() error { return nil }

func TestFailedDBStep_ProductHatAndRelayCarryNoClaimNoQueryNoRowValue(t *testing.T) {
	const (
		claimed = "CLAIMED-VALUE-PAID"
		rowVal  = "ROW-VALUE-PENDING-7731"
		query   = "SELECT secret_status_col FROM hidden_orders_table WHERE id = $1"
		param   = "PARAM-SENTINEL-42"
	)
	db := &redactionDB{rows: chain.DBRows{Columns: []string{"secret_status_col"}, Rows: [][]any{{rowVal}}}}
	asserts, err := scenario.DBStepClaims([]string{"body has rows.0.secret_status_col equals " + claimed})
	if err != nil {
		t.Fatal(err)
	}
	step := chain.DBStep("q", chain.DBSpec{Query: query, Params: []string{param}},
		asserts, nil, nil, false, func(context.Context, chain.DBConn) (chain.DBQuerier, error) { return db, nil })
	res := chain.Run("tr-1", []chain.Step{step})
	res.ID = "S-1"
	if res.Status != "failed" {
		t.Fatalf("setup: the step must fail on its claim, got %q", res.Status)
	}
	// the author hat DOES see the claim and the row value (that is the point of failed_claims) - a control that
	// the sentinels are really in the report before the redaction runs
	if b, _ := json.Marshal(res); !strings.Contains(string(b), claimed) || !strings.Contains(string(b), rowVal) {
		t.Fatalf("control: the author's report must carry the claim and the observed row value: %s", b)
	}

	rep := &report.Report{Mode: "build", RunID: "r1"}
	rep.Summary.Failed, rep.Summary.Total = 1, 1
	rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{res}}}

	// the relayed projection first: it never copies a step at all
	relayed, _ := json.Marshal(reduceReport(rep, ""))

	toolcore.RedactExpected(rep, role.Product)
	product, _ := json.Marshal(rep)
	for name, doc := range map[string]string{"product-hat report": string(product), "relayed report": string(relayed)} {
		for _, leak := range []string{claimed, rowVal, "secret_status_col", "hidden_orders_table", "SELECT", param} {
			if strings.Contains(doc, leak) {
				t.Errorf("%s carries %q: %s", name, leak, doc)
			}
		}
	}
	// what the builder is still told: the step failed, and how many rows came back
	if !strings.Contains(string(product), "db query returned 1 row(s)") {
		t.Errorf("the builder keeps the reality-only Observed (the row count): %s", product)
	}
}

type refusingDB struct{ err error }

func (d *refusingDB) Query(context.Context, string, []string, time.Duration, int) (chain.DBRows, error) {
	return chain.DBRows{}, d.err
}
func (d *refusingDB) Close() error { return nil }

// A server REFUSAL: PostgreSQL's message quotes the query's table and can quote a bound parameter, so the
// builder-visible Observed carries the SQLSTATE only. The author hat reads the message (BodyExcerpt); the product
// hat and the relayed report carry neither the message nor a name from it.
func TestFailedDBStep_ServerRefusalMessageReachesTheAuthorOnly(t *testing.T) {
	const (
		table   = "hidden_orders_v2_table"
		param   = "PARAM-SENTINEL-77"
		message = `relation "` + table + `" does not exist, invalid input syntax for type uuid: "` + param + `"`
	)
	db := &refusingDB{err: &chain.DBServerError{SQLState: "42P01", Message: message}}
	asserts, err := scenario.DBStepClaims([]string{"body has row_count >= 1"})
	if err != nil {
		t.Fatal(err)
	}
	step := chain.DBStep("q", chain.DBSpec{Query: "SELECT id FROM " + table + " WHERE id = $1", Params: []string{param}},
		asserts, nil, nil, false, func(context.Context, chain.DBConn) (chain.DBQuerier, error) { return db, nil })
	res := chain.Run("tr-1", []chain.Step{step})
	res.ID = "S-1"
	if res.Status != "failed" {
		t.Fatalf("setup: a server refusal is failed, got %q", res.Status)
	}
	// control: the author-hat report carries the message
	author, _ := json.Marshal(res)
	if !strings.Contains(string(author), table) {
		t.Fatalf("control: the author's report must carry the database's message: %s", author)
	}

	rep := &report.Report{Mode: "build", RunID: "r1"}
	rep.Summary.Failed, rep.Summary.Total = 1, 1
	rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{res}}}
	relayed, _ := json.Marshal(reduceReport(rep, ""))

	toolcore.RedactExpected(rep, role.Product)
	product, _ := json.Marshal(rep)
	for name, doc := range map[string]string{"product-hat report": string(product), "relayed report": string(relayed)} {
		for _, leak := range []string{table, "does not exist", "invalid input syntax", param, `relation "`} {
			if strings.Contains(doc, leak) {
				t.Errorf("%s carries %q: %s", name, leak, doc)
			}
		}
	}
	if !strings.Contains(string(product), "SQLSTATE 42P01") {
		t.Errorf("the builder keeps the SQLSTATE: %s", product)
	}
	// AC-F2.1: and in its own field, like elapsed_ms (redactExpected leaves it); the relayed report copies no step.
	if !strings.Contains(string(product), `"observed_sqlstate":"42P01"`) {
		t.Errorf("the builder keeps observed_sqlstate: %s", product)
	}
	if strings.Contains(string(relayed), "observed_sqlstate") {
		t.Errorf("the relayed report copies no step, so no observed_sqlstate: %s", relayed)
	}
}
