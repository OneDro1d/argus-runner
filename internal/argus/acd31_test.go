package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D31 (issue #172) on the run path: a `row_count == N` / `no rows` bullet over a VERIFY that always
// returns one row is RUN but judges nothing. Tier 3 names it (verdict untouched); the enforced list
// stops counting it as a check.

func dbScenario(sql, expect string) *scenario.Scenario {
	return scenario.Parse(strings.Join([]string{
		"# Scenario: t", "",
		"## Metadata",
		"- **ID**: T-031",
		"- **Layer**: HTTP Ingestion -> Database State",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`", "",
		"## VERIFY",
		"```sql", sql, "```", "",
		"## EXPECT", "", "### Runnable", "- status=202", expect, "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n"))
}

func TestACD31_Tier3NamesRowShapeChecksOnAnAggregate(t *testing.T) {
	agg := "SELECT count(*) AS n FROM order_service.orders WHERE correlation_id = '${correlation_id}';"
	rows := "SELECT id FROM order_service.orders WHERE correlation_id = '${correlation_id}';"

	for _, c := range []struct {
		name, sql, bullet string
		want              bool
	}{
		{"row_count over an aggregate", agg, "- row_count == 1", true},
		{"no rows over an aggregate", agg, "- no rows", true},
		{"the value form over an aggregate", agg, "- n == 1", false},
		{"row_count over a row query", rows, "- row_count == 1", false},
	} {
		got := UnexecutedRunnable(dbScenario(c.sql, c.bullet))
		named := false
		for _, u := range got {
			if strings.Contains(u.Reason, "AC-D31") {
				named = true
				if u.Bullet != strings.TrimPrefix(c.bullet, "- ") {
					t.Errorf("%s: the bullet must be quoted verbatim, got %q", c.name, u.Bullet)
				}
			}
		}
		if named != c.want {
			t.Errorf("%s: tier 3 named it = %v, want %v (%+v)", c.name, named, c.want, got)
		}
	}
}

func TestACD31_EnforcedListSkipsRowShapeOnAnAggregate(t *testing.T) {
	agg := "SELECT count(*) AS n FROM order_service.orders WHERE correlation_id = 'tr-x'"
	rows := "SELECT id FROM order_service.orders WHERE correlation_id = 'tr-x'"

	if got := enforcedContentChecks("database-state", map[string]string{
		"db.query": agg, "expect.has_rows": "true", "expect.row_count": "1"}); len(got) != 0 {
		t.Errorf("row_count over an aggregate judges nothing and must not be listed as enforced: %q", got)
	}
	if got := enforcedContentChecks("database-state", map[string]string{
		"db.query": agg, "expect.has_rows": "false", "expect.row_count": "0"}); len(got) != 0 {
		t.Errorf("no rows over an aggregate cannot pass and must not be listed as enforced: %q", got)
	}
	if got := enforcedContentChecks("database-state", map[string]string{
		"db.query": agg, "expect.has_rows": "true", "expect.columns": "n=1"}); strings.Join(got, "|") != "column n == 1" {
		t.Errorf("the value form over an aggregate IS enforced: got %q", got)
	}
	if got := enforcedContentChecks("database-state", map[string]string{
		"db.query": rows, "expect.has_rows": "true", "expect.row_count": "1"}); strings.Join(got, "|") != "row_count == 1" {
		t.Errorf("row_count over a row query is still enforced: got %q", got)
	}
}
