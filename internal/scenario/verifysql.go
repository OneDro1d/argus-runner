package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// VR12-VF4..VF13 (V30-003 §3) — NINE WRITE-TIME RULES FOR A VERIFY QUERY. NO PARSER, NO DIALECT.
//
// ⛔ WHY NO SQL PARSER — considered, and rejected with the acceptance test named:
//
//   - The product is BYO-SUT and MULTI-DIALECT by its own code: internal/onboard/variant.go parses
//     `jdbc:oracle:thin:@host:1521:sid` alongside `jdbc:postgresql://`. And `Validate(text)` takes
//     MARKDOWN ALONE — it cannot detect the engine even if we wanted it to.
//   - TWO OF THE TWELVE SHIPPED QUERIES ARE POSTGRESQL-ONLY, and they are the acceptance test for
//     any parser choice: ORDE-007 uses the regex operator `idempotency_key ~ '^01[A-Z0-9]+$'`, and
//     DB-005 uses `now() - interval '2 minutes'`. A generic or MySQL-flavoured parser reds BOTH and
//     turns the example-suite gate red on day one.
//   - A vendored parser is a second unbudgeted dependency, and the obvious one (pg_query_go /
//     libpg_query) is CGO — which breaks the amd64+arm64 execution-plane publish.
//   - House precedent for exactly this trade: specs/20-over-inform-policy.md:233 — "Regex for v1 …
//     SQL parser later if the false-positive rate is too high."
//
// ⚠ THE COST, STATED HONESTLY. This buys the STRUCTURAL class, not full SQL validity. `WHERE (a = 1`
// is caught by the paren rule; `WHERE a === 1` is not, and still reaches the run. That is the
// accepted trade for zero new dependencies and zero dialect lock-in. If the false-NEGATIVE rate
// proves painful it is its own row in a later round — never folded in here.

// mutatingKeywords is VF4's ban list: a VERIFY measures, it does not change what it measures.
var mutatingKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "DROP", "ALTER", "CREATE", "GRANT",
}

var (
	// ⛔ VF12 — WORD BOUNDARIES, and the reason is measured, not theoretical: a naive
	// case-insensitive SUBSTRING scan falsely rejects TWO shipped queries — `create` matches
	// `created_at` in DB-005 and DB-006, `update` matches `updated_at` in DB-006. Word-boundary
	// matching finds ZERO across all twelve. A rule that refuses two correct scenarios on its first
	// day is worse than no rule.
	mutatingRe = regexp.MustCompile(`(?i)\b(` + strings.Join(mutatingKeywords, "|") + `)\b`)
	// A `${…}` placeholder, so VF11 can NAME the offending token rather than say "a placeholder".
	placeholderRe = regexp.MustCompile(`\$\{([^}]*)\}`)
	// The one substitution the product ever performs on a VERIFY query
	// (internal/argus/argus.go: strings.ReplaceAll(s.Verify.SQL, "${correlation_id}", corr)) —
	// that token, and nothing else.
	corrToken = "${correlation_id}"
	// standingStateTag opts a scenario's VERIFY OUT of VF5 (run-scoping). VF5 exists because an
	// unscoped query can go green on ANOTHER run's rows — but some legitimate checks measure a
	// STANDING invariant of the SUT ("no trade intent stuck for over an hour", "the price feed wrote
	// in the last 90 minutes") as a computed aggregate, not a row this run created. Another run's
	// rows cannot make an aggregate falsely green, so run-scoping is not merely optional for these,
	// it is the wrong shape of check. Without this tag the only way past VF5 was pasting an unused
	// `${correlation_id}` into the query — exactly the silent hack this tag exists to stop encouraging.
	// VF5b (StandingStateExpectErrors) is the abuse guard: it must not become a quieter way to pass
	// on "some row exists" than that hack was.
	standingStateTag = "standing-state"
	// A single-quoted string literal, and the two comment forms. Masked before any keyword scan:
	// a table called `orders_delete_log` is fine, and so is `-- delete the row manually`.
	sqlLiteralRe = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlLineCmtRe = regexp.MustCompile(`--[^\n]*`)
	sqlBlkCmtRe  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	selectHeadRe = regexp.MustCompile(`(?is)^\s*(SELECT|WITH)\b`)
	fromRe       = regexp.MustCompile(`(?i)\bFROM\b`)
	// The leading SELECT of a query that is not a CTE, for VF6 (outerSelectList reads the list itself).
	selectLeadRe = regexp.MustCompile(`(?is)^\s*SELECT\b`)
)

// maskSQL blanks every string literal and comment, preserving LENGTH so that any line/offset a
// caller computes on the masked text still points at the right place in the original.
//
// ⛔ THIS IS WHAT MAKES VF4 AND VF12 HONEST. Without it, `WHERE note = 'delete me'` is refused as a
// mutation and `-- update this when the schema changes` is refused as an UPDATE.
func maskSQL(q string) string {
	blank := func(m string) string { return strings.Repeat(" ", len(m)) }
	q = sqlBlkCmtRe.ReplaceAllStringFunc(q, blank)
	q = sqlLineCmtRe.ReplaceAllStringFunc(q, blank)
	return sqlLiteralRe.ReplaceAllStringFunc(q, blank)
}

// VerifySQLErrors applies VF4-VF12 to one authored VERIFY query. `columns` are the column names the
// scenario's EXPECT asserts on (VF6); pass nil when there are none. `standingState` is true when the
// scenario carries the `standing-state` tag (VF5b) — VF5's run-scoping requirement is skipped, and
// every other rule (VF4/VF6-VF9/VF11/VF12) still applies unchanged.
//
// It returns MESSAGES, not Errors: the caller owns the line number, because only it knows where the
// query sits in the file.
func VerifySQLErrors(query string, columns []string, standingState bool) []string {
	raw := strings.TrimSpace(query)
	if raw == "" {
		return nil
	}
	masked := maskSQL(raw)
	var errs []string

	// ── VF8 — ONE statement. It also keeps VF4 enforceable: without it, a write can be smuggled in
	// after a read and the "the query begins with SELECT" check still passes.
	body := strings.TrimSuffix(strings.TrimSpace(masked), ";")
	if strings.Contains(body, ";") {
		errs = append(errs, "the VERIFY query contains more than one statement — a `;`-separated "+
			"second statement is how a WRITE gets smuggled in behind a read. One SELECT, no `;` "+
			"except a single trailing one (VF8)")
	}

	// ── VF9 (2) — it must BEGIN with SELECT or WITH. A CTE is legitimate.
	if !selectHeadRe.MatchString(masked) {
		errs = append(errs, "a VERIFY query must begin with `SELECT` or `WITH` — it MEASURES the "+
			"SUT's state and must not change it (VF9)")
	}

	// ── VF7 — NO TAUTOLOGY. `SELECT 1` returns a row regardless of SUT state: the exact RO-09
	// failure mode, caught directly rather than by its symptom.
	if !fromRe.MatchString(masked) {
		errs = append(errs, "the VERIFY query has no `FROM` — a query that reads no table returns "+
			"its row regardless of what the SUT did, which is a tautology and a guaranteed false "+
			"green (VF7)")
	}

	// ── VF4 + VF12 — READ-ONLY, matched on WORD BOUNDARIES outside literals and comments.
	if m := mutatingRe.FindString(masked); m != "" {
		errs = append(errs, fmt.Sprintf("the VERIFY query contains `%s` — a VERIFY measures the SUT, "+
			"it never changes what it is measuring. Move the statement to `## CLEANUP`, which is "+
			"executed for exactly this purpose (VF4)", strings.ToUpper(m)))
	}

	// ── VF9 (5) — balanced parentheses and quotes.
	if n := strings.Count(masked, "("); n != strings.Count(masked, ")") {
		errs = append(errs, "the VERIFY query has unbalanced parentheses (VF9)")
	}
	if strings.Count(raw, "'")%2 != 0 {
		errs = append(errs, "the VERIFY query has an unterminated string literal (VF9)")
	}

	// ── VF5 + VF11 — `${correlation_id}`, and ONLY that placeholder.
	//
	// ⛔ VF5b (StandingStateExpectErrors, called by the caller alongside this) is what keeps this
	// skip from becoming the SAME silent hack VF5 exists to stop: a `standing-state` scenario must
	// prove its verdict comes from an asserted COLUMN VALUE, not from "some row exists".
	if !standingState && !strings.Contains(raw, corrToken) {
		errs = append(errs, "the VERIFY query is not scoped to this run — it must contain "+
			"`${correlation_id}`. An unscoped query matches OTHER runs' rows, so it goes green on "+
			"somebody else's data, and it gets worse the more concurrently the suite runs (VF5)")
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(raw, -1) {
		if m[0] == corrToken {
			continue
		}
		errs = append(errs, fmt.Sprintf("`%s` is not substituted in a VERIFY query — `${correlation_id}` "+
			"is the ONLY placeholder the runner replaces, so this reaches the database VERBATIM and "+
			"the query fails or, worse, matches nothing and reads as a SUT defect (VF11)", m[0]))
	}

	// ── VF6 — every asserted column appears in the SELECT list.
	errs = append(errs, missingColumns(masked, columns)...)
	return errs
}

// StandingStateExpectErrors is VF5b — the abuse guard on the `standing-state` opt-out from VF5.
// Call it alongside VerifySQLErrors, ONLY when the scenario carries the `standing-state` tag, with
// `dbx` the scenario's own `ParseDBExpect(s.RunnableExpect())`.
//
// ⛔ WHY THIS EXISTS. VF5 exists because an unscoped query can go green on another run's rows.
// Skipping it for a genuine standing-state check (a computed aggregate, immune to that failure mode
// by construction) is safe. Skipping it for a scenario whose EXPECT still boils down to "a row
// exists" is NOT — that is the same false-green VF5 refuses, reached through the tag instead of
// through an unused `${correlation_id}`. So the tag is refused unless the scenario's OWN EXPECT
// commits to a real value: no row-existence mode (`no rows` / an exact `row_count`) and at least one
// asserted column, because those are the only two shapes ParseDBExpect can judge that do NOT reduce
// to "some row exists".
func StandingStateExpectErrors(dbx DBExpect) []string {
	var errs []string
	if !dbx.HasRows || dbx.RowCount != -1 {
		errs = append(errs, "## VERIFY is tagged `standing-state`, so its EXPECT must not assert on ROW "+
			"EXISTENCE — `no rows` / `row_count == N` measure how many rows THIS RUN produced, which is "+
			"exactly the run-scoping `standing-state` opts the query out of. Assert a computed VALUE "+
			"instead (e.g. `stuck == 0` / `fresh == true`), not a row count (VF5b)")
	}
	if len(dbx.Columns) == 0 {
		errs = append(errs, "## VERIFY is tagged `standing-state` but ## EXPECT asserts no COLUMN VALUE "+
			"— with nothing but row existence left to judge it, the tag becomes a quieter way to pass on "+
			"\"some row exists\" than the unused `${correlation_id}` hack it exists to replace. Add a "+
			"`NAME == VALUE` assertion naming a column the query SELECTs (VF5b)")
	}
	return errs
}

// aggregateCallRe matches a select-list item that OPENS with an aggregate function call.
var aggregateCallRe = regexp.MustCompile(`(?i)^(?:count|sum|avg|min|max|bool_and|bool_or|every|array_agg|string_agg|json_agg|jsonb_agg|json_object_agg|jsonb_object_agg)\s*\(`)

// rowShapingWords change how many rows an aggregate query returns, so their presence at the top
// level means SingleRowAggregate cannot promise "exactly one row" and says false.
var rowShapingWords = []string{"GROUP", "HAVING", "OVER", "LIMIT", "OFFSET", "FETCH", "UNION", "INTERSECT", "EXCEPT"}

// SingleRowAggregate reports whether a VERIFY query is an aggregate with no grouping — one that
// returns EXACTLY ONE ROW whatever it counts (`SELECT count(*) AS n FROM orders WHERE …`).
//
// ⛔ WHY IT MATTERS (AC-D31, issue #172). The Database State template judges `row_count == N` and
// `no rows` by counting the rows of the answer (templates/database-state.jmx: dataRows = lines-1). On
// such a query that count is always 1, so `row_count == 1` can never fail and `no rows` can never
// pass. The count is in the VALUE (`n == 1`), not in the number of rows.
//
// Deliberately CONSERVATIVE, like missingColumns: a CTE, a query whose outer SELECT it cannot read, or
// any top-level word that can change the row count (GROUP BY, HAVING, a window OVER, LIMIT / OFFSET /
// FETCH, a set operator) yields false. A false "yes" would refuse a correct scenario; a false "no"
// leaves the author where they were before this rule. A select list that opens with an aggregate and
// has no GROUP BY cannot also carry a bare column — that is a SQL error — so one aggregate item is
// enough to know the shape.
func SingleRowAggregate(query string) bool {
	masked := strings.TrimSuffix(strings.TrimSpace(maskSQL(strings.TrimSpace(query))), ";")
	list, ok := outerSelectList(masked)
	if !ok {
		return false
	}
	for _, w := range rowShapingWords {
		if hasTopLevelWord(masked, w) {
			return false
		}
	}
	for _, item := range splitTopLevel(list, ',') {
		item = strings.TrimSpace(item)
		if strings.HasPrefix(strings.ToUpper(item), "DISTINCT ") {
			item = strings.TrimSpace(item[len("DISTINCT "):])
		}
		if aggregateCallRe.MatchString(item) {
			return true
		}
	}
	return false
}

// AggregateExpectErrors is VF14: an EXPECT that judges ROW EXISTENCE (`no rows` / `row_count == N`) on
// a VERIFY that always returns exactly one row (SingleRowAggregate) is refused, naming the value form.
func AggregateExpectErrors(query string, dbx DBExpect) []string {
	if !SingleRowAggregate(query) {
		return nil
	}
	if dbx.HasRows && dbx.RowCount < 0 {
		return nil
	}
	what := fmt.Sprintf("`row_count == %d` can never FAIL", dbx.RowCount)
	if !dbx.HasRows {
		what = "`no rows` can never PASS"
	}
	return []string{fmt.Sprintf("the VERIFY query is an aggregate (`count(*)`, `sum(…)`, …) with no GROUP BY, "+
		"so it returns EXACTLY ONE ROW whatever it counts — %s, because the row count is judged, not the "+
		"value. Assert the VALUE: alias it (`SELECT count(*) AS n …`) and write `n == 0` / `n == 1` under "+
		"`### Runnable` instead (VF14, AC-D31)", what)}
}

// splitTopLevel splits s on sep at parenthesis depth zero.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// hasTopLevelWord reports whether word occurs, on word boundaries, at parenthesis depth zero.
func hasTopLevelWord(masked, word string) bool {
	depth := 0
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && isWordAt(masked, i, word) {
			return true
		}
	}
	return false
}

// outerSelectList returns the SELECT list of the OUTERMOST SELECT: the text between the leading
// SELECT and the first FROM at parenthesis depth zero. A sub-select's own FROM sits inside
// parentheses and is skipped, so `(SELECT count(*) FROM t) AS n` contributes its alias `n`, not
// `count(*)`. The previous reader stopped at the FIRST FROM in the text, which for a query whose
// select list opens with a sub-select was the sub-select's FROM: the list it examined was the
// sub-select's head, and VF6 either refused a correct query (an alias it never reached) or said
// nothing because `count(*)` in that head looked like `SELECT *`. Both were measured on a shipped
// scenario, 2026-09-17. A query that does not begin with SELECT (a CTE) yields ok=false, and the
// caller says nothing, as before.
func outerSelectList(masked string) (string, bool) {
	loc := selectLeadRe.FindStringIndex(masked)
	if loc == nil {
		return "", false
	}
	start := loc[1]
	depth := 0
	for i := start; i < len(masked); i++ {
		switch masked[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && isWordAt(masked, i, "FROM") {
			return masked[start:i], true
		}
	}
	return "", false
}

// isWordAt reports whether the case-insensitive word sits at masked[i:] on word boundaries.
func isWordAt(masked string, i int, word string) bool {
	if i+len(word) > len(masked) || !strings.EqualFold(masked[i:i+len(word)], word) {
		return false
	}
	before := i == 0 || !isWordByte(masked[i-1])
	after := i+len(word) == len(masked) || !isWordByte(masked[i+len(word)])
	return before && after
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// selectsEverything reports whether the outer select list carries a bare `*` at depth zero —
// `SELECT *` or `SELECT o.*` — as opposed to a `*` inside a call such as `count(*)`, which
// selects one column, not every column.
func selectsEverything(list string) bool {
	depth := 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '*':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// missingColumns is VF6. It is deliberately conservative: `SELECT *`, a CTE, or any construct it
// cannot read yields NO error, because a false refusal here costs an author a correct scenario.
func missingColumns(masked string, columns []string) []string {
	if len(columns) == 0 {
		return nil
	}
	list, ok := outerSelectList(masked)
	if !ok {
		return nil // no readable outer SELECT…FROM (a CTE, say) — say nothing rather than guess
	}
	if selectsEverything(list) {
		return nil
	}
	var out []string
	for _, c := range columns {
		// A column counts as present if its NAME appears in the select list — as itself, as part of
		// a qualified name (`o.status`), or aliased (`count(*) AS n`). Matching the bare word is
		// enough, and anything cleverer starts guessing at SQL.
		if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(c) + `\b`).MatchString(list) {
			continue
		}
		out = append(out, fmt.Sprintf("## EXPECT asserts on the column `%s`, which the VERIFY query "+
			"does not SELECT — the assertion can never be evaluated, so it is a claim that silently "+
			"proves nothing (VF6)", c))
	}
	return out
}
