package scenario

import (
	"strings"
	"testing"
)

// VF14 (AC-D31, issue #172): the Database State template judges `row_count == N` / `no rows` by
// COUNTING the rows of the answer, and an ungrouped aggregate returns exactly one row whatever it
// counts. So `row_count == 1` over `SELECT count(*)` can never fail, and `no rows` can never pass.

func TestSingleRowAggregate(t *testing.T) {
	for _, c := range []struct {
		q    string
		want bool
	}{
		// ⭐ the two shipped shapes that motivated the rule
		{"SELECT count(*) AS n\nFROM order_service.orders\nWHERE correlation_id = '${correlation_id}';", true},
		{"SELECT count(*) AS row_count FROM order_service.orders WHERE correlation_id = '${correlation_id}'", true},
		{"select COUNT(*) from orders where correlation_id = '${correlation_id}'", true},
		{"SELECT sum(qty) AS total, max(created_at) AS last FROM items WHERE correlation_id = '${correlation_id}'", true},
		{"SELECT count(*)::text AS stuck FROM trade_intents WHERE status = 'pending'", true},
		{"SELECT count(DISTINCT sku) AS n FROM items WHERE correlation_id = '${correlation_id}'", true},

		// NOT one row: each of these can return zero or many rows, so row_count is meaningful
		{"SELECT id FROM orders WHERE correlation_id = '${correlation_id}'", false},
		{"SELECT status, count(*) AS n FROM orders WHERE correlation_id = '${correlation_id}' GROUP BY status", false},
		{"SELECT count(*) AS n FROM orders WHERE correlation_id = '${correlation_id}' HAVING count(*) > 0", false},
		{"SELECT count(*) OVER () AS n FROM orders WHERE correlation_id = '${correlation_id}'", false},
		{"SELECT count(*) AS n FROM orders WHERE correlation_id = '${correlation_id}' LIMIT 0", false},
		{"SELECT count(*) AS n FROM a UNION ALL SELECT count(*) FROM b", false},
		{"SELECT o.id FROM orders o WHERE o.total = (SELECT max(total) FROM orders)", false}, // aggregate only in a sub-select
		{"WITH x AS (SELECT count(*) AS n FROM orders) SELECT n FROM x", false},              // a CTE: not read, so no claim
		{"SELECT id FROM orders WHERE note = 'count(*) from somewhere'", false},              // inside a literal
		{"SELECT counter FROM orders WHERE correlation_id = '${correlation_id}'", false},     // a column named like one
		{"", false},
	} {
		if got := SingleRowAggregate(c.q); got != c.want {
			t.Errorf("SingleRowAggregate(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestAggregateExpectErrors(t *testing.T) {
	agg := "SELECT count(*) AS n FROM orders WHERE correlation_id = '${correlation_id}'"
	rows := "SELECT id FROM orders WHERE correlation_id = '${correlation_id}'"

	for _, c := range []struct {
		name   string
		q      string
		expect []string
		want   string // "" = no error
	}{
		{"row_count on an aggregate", agg, []string{"row_count == 1"}, "can never FAIL"},
		{"no rows on an aggregate", agg, []string{"no rows"}, "can never PASS"},
		{"the value form on an aggregate", agg, []string{"n == 1"}, ""},
		{"the value form, zero", agg, []string{"n == 0"}, ""},
		{"row_count on a row query", rows, []string{"row_count == 1"}, ""},
		{"no rows on a row query", rows, []string{"no rows"}, ""},
	} {
		got := AggregateExpectErrors(c.q, ParseDBExpect(c.expect))
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: refused a correct scenario: %v", c.name, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want) || !strings.Contains(got[0], "n == ")):
			t.Errorf("%s: want one refusal containing %q and naming the value form, got %v", c.name, c.want, got)
		}
	}
}

// Through Validate — the author path. The pre-fix ORDE-013 shape is refused at the ```sql line; the
// fixed shape passes; a standing-state scenario reports its row-existence EXPECT ONCE (VF5b), not twice.
func TestVF14_ThroughValidate(t *testing.T) {
	doc := func(tags, sql, expect string) string {
		return strings.Join([]string{
			"# Scenario: t", "",
			"## Metadata",
			"- **ID**: ORDE-913",
			"- **Layer**: HTTP Ingestion -> Database State",
			"- **Tags**: http, " + tags, "",
			"## TRIGGER",
			"POST `${INGESTION_URL}/api/v1/orders`", "",
			"```json", `{"a": 1}`, "```", "",
			"## VERIFY",
			"```sql", sql, "```", "",
			"## EXPECT", "", "### Runnable", "- status=202", expect, "",
			"### Non-runnable", "- one order row", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
	}
	vf14 := func(_ *Scenario, errs []Error) (n int) {
		for _, e := range errs {
			if strings.Contains(e.Message, "VF14") {
				n++
			}
		}
		return n
	}
	agg := "SELECT count(*) AS n FROM order_service.orders WHERE correlation_id = '${correlation_id}';"

	if n := vf14(Validate(doc("order", agg, "- row_count == 1"))); n != 1 {
		t.Errorf("the pre-fix ORDE-013 shape must be refused once by VF14, got %d", n)
	}
	if n := vf14(Validate(doc("order", agg, "- n == 1"))); n != 0 {
		t.Errorf("the fixed ORDE-013 shape must pass VF14, got %d refusals", n)
	}
	standing := "SELECT count(*) AS n FROM order_service.orders WHERE status = 'stuck';"
	if n := vf14(Validate(doc("order, standing-state", standing, "- no rows"))); n != 0 {
		t.Errorf("standing-state: VF5b owns this refusal, VF14 must not repeat it (got %d)", n)
	}
}
