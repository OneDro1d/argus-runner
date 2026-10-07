package scenario

import (
	"strings"
	"testing"
)

// VF5b — the `standing-state` tag opts a VERIFY query OUT of VF5 (the `${correlation_id}`
// run-scoping requirement), for the class of check that measures a computed aggregate over the
// SUT's STANDING state ("no trade intent stuck for over an hour", "the price feed wrote in the
// last 90 minutes") rather than a row this run created. Another run's rows cannot make an
// aggregate falsely green, so VF5's failure mode does not apply — but the tag must not become a
// quieter version of the "paste an unused `${correlation_id}`" hack it replaces. The guard
// (StandingStateExpectErrors) refuses a tagged scenario whose EXPECT still boils down to "some
// row exists": it requires an asserted COLUMN VALUE and refuses row-existence mode (`no rows` /
// an exact `row_count`).

const stuckIntentsQuery = "SELECT count(*)::text AS stuck FROM trade_intents WHERE status NOT IN " +
	"('done','failed') AND created_at < now() - interval '1 hour'"

// standingScenario builds a minimal valid Database State scenario, parameterized on the Tags line,
// the VERIFY body and the EXPECT bullets — same shape as ro09_test.go's dbScenario, widened so the
// standing-state cases can vary tags and EXPECT independently.
func standingScenario(tags, verify string, expect []string) string {
	lines := []string{
		"# Scenario: SS-001",
		"## Metadata",
		"- **ID**: SS-001",
		"- **Layer**: Database State",
		"- **Tags**: " + tags,
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
		"## VERIFY",
		"```sql",
		verify,
		"```",
		"## EXPECT",
		"### Runnable",
	}
	for _, e := range expect {
		lines = append(lines, "- "+e)
	}
	lines = append(lines,
		"## TIMEOUT",
		"30s",
		"## CLEANUP",
		"N/A — a unit-test fixture; it creates nothing.",
	)
	return strings.Join(lines, "\n")
}

func hasRuleTag(msgs []string, tag string) bool {
	for _, m := range msgs {
		if strings.Contains(m, "("+tag+")") {
			return true
		}
	}
	return false
}

// ── unit level: VerifySQLErrors + StandingStateExpectErrors directly ──────────────────────────

// a tagged aggregate query (no ${correlation_id}) is accepted by VF5, and a matching EXPECT shape
// (an asserted column value, no row-existence mode) is accepted by VF5b.
func TestVerifySQL_StandingStateAggregatePasses(t *testing.T) {
	if got := VerifySQLErrors(stuckIntentsQuery, []string{"stuck"}, true); len(got) > 0 {
		t.Fatalf("a standing-state aggregate query with no ${correlation_id} must be ACCEPTED by VF5, got %v", got)
	}
	dbx := DBExpect{HasRows: true, RowCount: -1, Columns: map[string]string{"stuck": "0"}}
	if got := StandingStateExpectErrors(dbx); len(got) > 0 {
		t.Fatalf("a standing-state EXPECT with a column assertion and no row-existence mode must be ACCEPTED, got %v", got)
	}
}

// the IDENTICAL query, untagged, still fails VF5 — with the ORIGINAL message, unchanged.
func TestVerifySQL_UntaggedIdenticalQueryStillFailsVF5(t *testing.T) {
	got := VerifySQLErrors(stuckIntentsQuery, []string{"stuck"}, false)
	want := "the VERIFY query is not scoped to this run — it must contain `${correlation_id}`. " +
		"An unscoped query matches OTHER runs' rows, so it goes green on somebody else's data, " +
		"and it gets worse the more concurrently the suite runs (VF5)"
	var found bool
	for _, g := range got {
		if g == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("an untagged query must still fail VF5 with the UNCHANGED message; got %v", got)
	}
}

// tagged + `no rows` (HasRows == false) is refused — that is row-existence in its negative form.
func TestStandingStateExpectErrors_RefusesNoRows(t *testing.T) {
	dbx := DBExpect{HasRows: false, RowCount: 0, Columns: map[string]string{}}
	got := StandingStateExpectErrors(dbx)
	if len(got) == 0 {
		t.Fatal("a standing-state EXPECT asserting `no rows` must be REFUSED")
	}
	if !hasRuleTag(got, "VF5b") {
		t.Errorf("the refusal must cite VF5b; got %v", got)
	}
}

// tagged + an exact `row_count == N` is refused — a row COUNT is still judged by row existence,
// not by an asserted value.
func TestStandingStateExpectErrors_RefusesExactRowCount(t *testing.T) {
	dbx := DBExpect{HasRows: true, RowCount: 1, Columns: map[string]string{"stuck": "5"}}
	got := StandingStateExpectErrors(dbx)
	if len(got) == 0 {
		t.Fatal("a standing-state EXPECT asserting `row_count == 1` must be REFUSED, even with a column assertion present")
	}
	if !hasRuleTag(got, "VF5b") {
		t.Errorf("the refusal must cite VF5b; got %v", got)
	}
}

// tagged + no column assertion at all is refused — with nothing else to judge it by, the verdict
// would come from row existence alone, exactly the proxy `standing-state` must not reintroduce.
func TestStandingStateExpectErrors_RefusesNoColumnAssertion(t *testing.T) {
	dbx := DBExpect{HasRows: true, RowCount: -1, Columns: map[string]string{}}
	got := StandingStateExpectErrors(dbx)
	if len(got) == 0 {
		t.Fatal("a standing-state EXPECT with no column assertion must be REFUSED")
	}
	if !hasRuleTag(got, "VF5b") {
		t.Errorf("the refusal must cite VF5b; got %v", got)
	}
}

// tagged + a mutation is STILL refused by VF4 — the tag opts a query out of VF5 ONLY.
func TestVerifySQL_StandingStateMutationStillRefusedByVF4(t *testing.T) {
	got := VerifySQLErrors("DELETE FROM trade_intents WHERE status = 'stuck'", nil, true)
	if !hasRuleTag(got, "VF4") {
		t.Fatalf("a standing-state scenario's VERIFY must still be refused by VF4 (read-only); got %v", got)
	}
}

// ── end-to-end: through Validate() / Warnings(), a full scenario markdown ─────────────────────

// the aggregate scenario, tagged and correctly shaped, validates clean end-to-end.
func TestValidate_StandingStateEndToEndPasses(t *testing.T) {
	md := standingScenario("http, standing-state", stuckIntentsQuery, []string{"stuck == 0"})
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("a correctly-shaped standing-state scenario must validate clean, got %+v", errs)
	}
}

// the identical scenario without the tag is refused by VF5, unchanged.
func TestValidate_StandingStateEndToEndUntaggedFailsVF5(t *testing.T) {
	md := standingScenario("http", stuckIntentsQuery, []string{"stuck == 0"})
	_, errs := Validate(md)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "(VF5)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the untagged scenario must be refused by VF5; got %+v", errs)
	}
}

// tagged + `no rows` EXPECT is refused end-to-end, citing VF5b.
func TestValidate_StandingStateEndToEndNoRowsRefused(t *testing.T) {
	md := standingScenario("http, standing-state", stuckIntentsQuery, []string{"no rows"})
	_, errs := Validate(md)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "(VF5b)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standing-state scenario asserting `no rows` must be refused citing VF5b; got %+v", errs)
	}
}

// tagged + `row_count == 1` EXPECT is refused end-to-end, citing VF5b.
func TestValidate_StandingStateEndToEndRowCountRefused(t *testing.T) {
	md := standingScenario("http, standing-state", stuckIntentsQuery, []string{"stuck == 5", "row_count == 1"})
	_, errs := Validate(md)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "(VF5b)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standing-state scenario asserting `row_count == 1` must be refused citing VF5b; got %+v", errs)
	}
}

// tagged + no column assertion is refused end-to-end, citing VF5b.
func TestValidate_StandingStateEndToEndNoColumnRefused(t *testing.T) {
	md := standingScenario("http, standing-state", stuckIntentsQuery, []string{"the count stays healthy"})
	_, errs := Validate(md)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "(VF5b)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standing-state scenario with no column assertion must be refused citing VF5b; got %+v", errs)
	}
}

// tagged + a mutation is still refused by VF4 end-to-end.
func TestValidate_StandingStateEndToEndMutationRefusedByVF4(t *testing.T) {
	md := standingScenario("http, standing-state", "DELETE FROM trade_intents WHERE status = 'stuck'", []string{"stuck == 0"})
	_, errs := Validate(md)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "(VF4)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standing-state scenario's mutating VERIFY must still be refused by VF4; got %+v", errs)
	}
}

// VF5b's warning half: a standing-state VERIFY that still carries ${correlation_id} scopes itself
// to this run anyway, so the tag is probably unnecessary — advisory only, never a refusal.
func TestWarnings_StandingStateWithCorrelationIDWarns(t *testing.T) {
	q := "SELECT status FROM orders WHERE correlation_id = '${correlation_id}'"
	md := standingScenario("http, standing-state", q, []string{"status == done"})
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("this scenario is correctly shaped and must validate clean, got %+v", errs)
	}
	var found bool
	for _, w := range Warnings(md) {
		if strings.Contains(w, "(VF5b)") && strings.Contains(w, "probably unnecessary") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standing-state VERIFY that still contains ${correlation_id} must WARN that the tag is probably unnecessary; got %v", Warnings(md))
	}
}
