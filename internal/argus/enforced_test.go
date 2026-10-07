package argus

import (
	"strings"
	"testing"
)

// The count is exactly what the template was handed: a body check on a layer whose template does
// not read the body properties is not enforced, and is not counted; a status is never counted.
func TestEnforcedContentChecks_OnlyWhatTheTemplateReads(t *testing.T) {
	props := map[string]string{
		"expect.status": "200", "expect.status2": "409", "expect.body.count": "2",
		"expect.body.1.field": "", "expect.body.1.op": "contains", "expect.body.1.value": "ok",
		"expect.body.2.field": "id", "expect.body.2.op": "matches", "expect.body.2.value": "^a",
		"expect.columns": "currency=EUR;note=__NULL__;state=new|done", "expect.has_rows": "true",
	}
	got := enforcedContentChecks("http-ingestion", props)
	if want := []string{`content contains "ok"`, "field id matches /^a/"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("http-ingestion reads the body family only: got %q, want %q", got, want)
	}
	got = enforcedContentChecks("database-state", props)
	if want := []string{"column currency == EUR", "column note is null", "column state is one of new or done"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("database-state reads the columns only: got %q, want %q", got, want)
	}
	for _, base := range []string{"http-idempotency", "saga-presence"} {
		if got := enforcedContentChecks(base, props); len(got) != 0 {
			t.Errorf("%s reads no content property: got %q", base, got)
		}
	}
	if got := enforcedContentChecks("http-ingestion", map[string]string{"expect.status": "200"}); len(got) != 0 {
		t.Errorf("a status-only scenario enforces no content check: got %q", got)
	}
}

// A declared row count and a declared "no rows" are content checks the database templates evaluate:
// a scenario whose only check is `row_count == 1` enforced one check, not none. The implicit
// has_rows=true that every database expectation carries is not a declared check and is not counted.
func TestEnforcedContentChecks_RowCountAndNoRows(t *testing.T) {
	count := map[string]string{"expect.status": "202", "expect.has_rows": "true", "expect.row_count": "1"}
	if got := enforcedContentChecks("database-state", count); strings.Join(got, "|") != "row_count == 1" {
		t.Errorf("a lone row_count check is one enforced check: got %q", got)
	}
	none := map[string]string{"expect.status": "202", "expect.has_rows": "false", "expect.row_count": "0"}
	if got := enforcedContentChecks("database-state", none); strings.Join(got, "|") != "no rows" {
		t.Errorf("a declared 'no rows' is one enforced check: got %q", got)
	}
	implicit := map[string]string{"expect.status": "202", "expect.has_rows": "true"}
	if got := enforcedContentChecks("database-state", implicit); len(got) != 0 {
		t.Errorf("the implicit has_rows=true is not a declared check: got %q", got)
	}
	if got := enforcedContentChecks("http-ingestion", count); len(got) != 0 {
		t.Errorf("http-ingestion does not read the row properties: got %q", got)
	}
}
