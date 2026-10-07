package scenario

import (
	"strings"
	"testing"
)

const bt = "\x60" // backtick — avoids the triple-backtick-in-raw-string problem

// validMD returns a fully-valid scenario (ORD-003 style) with a References section.
func validMD() string {
	lines := []string{
		"# Scenario: order idempotency honored",
		"",
		"## Metadata",
		"- **ID**: ORD-003",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, critical, idempotency",
		"",
		"## TRIGGER",
		"POST " + bt + "${INGESTION_URL}/api/v1/orders" + bt,
		"",
		bt + bt + bt + "json",
		`{"customer_id":"c1","items":[{"sku":"s1","qty":1}],"currency":"USD"}`,
		bt + bt + bt,
		"",
		"## VERIFY",
		"N/A — a status layer judges by response code.",
		"",
		"## EXPECT",
		// VR12-E1: a bullet directly under ## EXPECT is refused — position decides whether a
		// bullet is a claim, never its wording. The shared fixture carries the split shape.
		"### Runnable",
		"- status=202",
		"- body has order_id matching ^01[A-Z0-9]+$",
		"",
		"## TIMEOUT",
		"30s",
		"",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		"## CLEANUP",
		bt + bt + bt + "sql",
		"DELETE FROM orders WHERE customer_id='c1';",
		bt + bt + bt,
		"",
		"## References",
		"- JIRA-123",
		"- https://confluence/x",
	}
	return strings.Join(lines, "\n")
}

func TestParse_Full(t *testing.T) {
	s := Parse(validMD())
	if s.ID != "ORD-003" {
		t.Errorf("ID = %q", s.ID)
	}
	if len(s.Layers) != 1 || s.Layers[0] != "HTTP Ingestion" {
		t.Errorf("Layers = %v", s.Layers)
	}
	// ⛔ VR12-M2 (V30-003): `Priority` is GONE from the parser. It was parsed, carried into
	// AsParsedMap, surfaced in exactly one place and read by NOTHING that decides anything. This
	// case used to assert it round-tripped; it now asserts the FIELD no longer exists on the type,
	// which the compiler enforces, and that the rest of Metadata is unaffected.
	if len(s.Tags) != 3 || s.Tags[0] != "http" || s.Tags[1] != "critical" || s.Tags[2] != "idempotency" {
		t.Errorf("Tags = %v — the dispatch tag rides in **Tags** with the rest (VR12-M3)", s.Tags)
	}
	if s.Trigger.Method != "POST" || !strings.Contains(s.Trigger.URL, "/api/v1/orders") {
		t.Errorf("Trigger = %+v", s.Trigger)
	}
	if !strings.Contains(s.Trigger.Payload, "customer_id") {
		t.Errorf("Payload = %q", s.Trigger.Payload)
	}
	if len(s.Expect) != 2 {
		t.Errorf("Expect = %v", s.Expect)
	}
	if s.Timeout != "30s" {
		t.Errorf("Timeout = %q", s.Timeout)
	}
	if !strings.Contains(s.Cleanup.SQL, "DELETE FROM orders") {
		t.Errorf("Cleanup.SQL = %q", s.Cleanup.SQL)
	}
	if len(s.References) != 2 || s.References[0] != "JIRA-123" {
		t.Errorf("References = %v", s.References)
	}
}

func TestParse_MultiLayerChain(t *testing.T) {
	md := strings.Replace(validMD(), "- **Layer**: HTTP Ingestion", "- **Layer**: HTTP Ingestion -> Database State", 1)
	s := Parse(md)
	if len(s.Layers) != 2 || s.Layers[0] != "HTTP Ingestion" || s.Layers[1] != "Database State" {
		t.Errorf("Layers = %v", s.Layers)
	}
}

func TestParse_TimeoutDefault(t *testing.T) {
	md := strings.Replace(validMD(), "## TIMEOUT\n30s\n\n", "", 1)
	if s := Parse(md); s.Timeout != "10s" {
		t.Errorf("default Timeout = %q, want 10s", s.Timeout)
	}
}

func TestValidate_Valid(t *testing.T) {
	if _, errs := Validate(validMD()); len(errs) != 0 {
		t.Errorf("valid scenario reported errors: %v", errs)
	}
}

func TestValidate_Malformed(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(string) string
		wantSubst string
	}{
		{"missing ID", func(m string) string { return strings.Replace(m, "- **ID**: ORD-003\n", "", 1) }, "missing required Metadata **ID**"},
		{"bad ID format", func(m string) string { return strings.Replace(m, "ORD-003", "ord 3", 1) }, "is not allowed"}, // VR10-S4: "ord_3" is now VALID; a space is not
		{"missing Layer", func(m string) string { return strings.Replace(m, "- **Layer**: HTTP Ingestion\n", "", 1) }, "missing required Metadata **Layer**"},
		{"unknown Layer", func(m string) string { return strings.Replace(m, "HTTP Ingestion", "Teleportation", 1) }, "unknown layer"},
		// ⛔ VR12-M2 (V30-003): a Priority VALUE is no longer a thing to get wrong, because the KEY is
		// gone. The case is kept and re-pointed: declaring it at all is now the error, and the message
		// says it was REMOVED rather than mistyped — 119 shipped scenarios carried it, so an author
		// will hit this and deserves to know which of the two happened.
		{"Priority is removed", func(m string) string {
			return strings.Replace(m, "## Metadata\n", "## Metadata\n- **Priority**: High\n", 1)
		}, "was REMOVED in 0.3.31"},
		{"unknown Metadata key", func(m string) string {
			return strings.Replace(m, "## Metadata\n", "## Metadata\n- **Owner**: me\n", 1)
		}, "is not a defined Metadata key"},
		{"missing EXPECT", func(m string) string {
			// VR12-E1: boundary-based, so it keeps removing the section as the fixture evolves.
			return cutExpect(m, "")
		}, "## EXPECT must have at least one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Validate(c.mutate(validMD()))
			if len(errs) == 0 {
				t.Fatalf("%s: expected an error, got none", c.name)
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.Message, c.wantSubst) {
					found = true
					if e.Line < 1 {
						t.Errorf("%s: error has no valid line number: %+v", c.name, e)
					}
				}
			}
			if !found {
				t.Errorf("%s: no error contained %q; got %v", c.name, c.wantSubst, errs)
			}
		})
	}
}

// The established multi-segment MCP ID convention (ENG-MCP-001, SOC-LIVE-002) must VALIDATE —
// the prior single-segment regex wrongly rejected the project's own committed scenarios. VR10-S4
// relaxed the rule further: lowercase and underscores are valid too, so "eng_mcp_1" now PASSES —
// the shapes that are still refused live in id_rule_test.go.
func TestValidate_MultiSegmentID(t *testing.T) {
	for _, id := range []string{"ENG-MCP-001", "SOC-LIVE-002", "ORD-001", "eng_mcp_1"} {
		md := strings.Replace(validMD(), "ORD-003", id, 1)
		_, errs := Validate(md)
		for _, e := range errs {
			if strings.HasPrefix(e.Message, "ID ") {
				t.Errorf("ID %q should be VALID, got format error: %s", id, e.Message)
			}
		}
	}
}

func TestValidate_MissingTrigger(t *testing.T) {
	// Drop the whole TRIGGER section (heading + body up to ## VERIFY).
	md := validMD()
	i := strings.Index(md, "## TRIGGER")
	j := strings.Index(md, "## VERIFY")
	md = md[:i] + md[j:]
	_, errs := Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "## TRIGGER") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing-TRIGGER error, got %v", errs)
	}
}

// FINDING-7/FINDING-9 (2026-06-19): the structured DB/content EXPECT grammar the
// database-state / message-flow templates execute — column==value with PROSE synonyms
// (equals/is/=/IS NULL) + alternation, row_count, negative/zero-row — replacing the
// corr-id-substring proxy, and WARNING (never silent fall-back) on forms it can't run.
func TestParseDBExpect(t *testing.T) {
	// == and the keyword form still work, multiple per bullet
	d := ParseDBExpect([]string{"customer_id == cust-db-002, currency == EUR", "row_count == 1"})
	if !d.HasRows || d.RowCount != 1 {
		t.Fatalf("has_rows/row_count: %+v", d)
	}
	if d.Props()["expect.columns"] != "currency=EUR;customer_id=cust-db-002" {
		t.Errorf("columns prop = %q", d.Props()["expect.columns"])
	}

	// FINDING-9: PROSE synonyms — equals / is / IS NULL / alternation (none use ==)
	p := ParseDBExpect([]string{
		"customer_id equals cust-db-002",
		"status is notified or pending",
		"chain_id IS NULL",
	})
	if p.Columns["customer_id"] != "cust-db-002" {
		t.Errorf("'equals' synonym: %+v", p.Columns)
	}
	if p.Columns["status"] != "notified|pending" {
		t.Errorf("'is A or B' alternation: %+v", p.Columns)
	}
	if p.Columns["chain_id"] != "__NULL__" {
		t.Errorf("'IS NULL': %+v", p.Columns)
	}
	if len(p.Uncoverable) != 0 {
		t.Errorf("clean prose must NOT warn: %v", p.Uncoverable)
	}

	// "N row(s) with ..." prefix is stripped so the column clauses still parse
	rp := ParseDBExpect([]string{"1 row with customer_id equals cust-db-002, currency equals EUR"})
	if rp.RowCount != 1 || rp.Columns["customer_id"] != "cust-db-002" || rp.Columns["currency"] != "EUR" {
		t.Errorf("'N row with ...' prefix: %+v", rp)
	}

	// negative (DB-008): a DECLARED negative => HasRows=false, no column/count carry-over.
	// VR12-E9: the declared forms are unchanged; what changed is that one must BE the clause.
	neg := ParseDBExpect([]string{"no rows", "customer_id == x"})
	if neg.HasRows || len(neg.Columns) != 0 || neg.RowCount != 0 || neg.Props()["expect.has_rows"] != "false" {
		t.Fatalf("negative must be HasRows=false with no column/count: %+v", neg)
	}

	// ⛔ VR12-E9 REGRESSION GUARD. This case used to read
	//     "no row is persisted (the poison order is rejected)"
	// and passed because the UNANCHORED regex found "rejected" inside the sentence. A sentence must
	// never invert a database assertion — measured, that accident fired on seven shipped bullets
	// whose only crime was quoting a Neo4j error ("… no rows in result set") or explaining that "an
	// unknown tool is rejected on the protocol plane".
	prose := ParseDBExpect([]string{"row_count == 1", "no row is persisted (the poison order is rejected)"})
	if !prose.HasRows {
		t.Error("a SENTENCE containing 'rejected' must not set HasRows=false — it must be the clause")
	}

	// uncoverable forms => WARN (signal, not silent): range / jsonb / unsupported operator /
	// a column-looking bullet whose value is prose we can't execute.
	unc := ParseDBExpect([]string{
		"created_at within ±2s of now",         // range
		"items->0->>'sku' == ABC",              // jsonb
		"status matches notified",              // unsupported operator
		"note equals some long human sentence", // multi-word value
	})
	if len(unc.Uncoverable) < 4 {
		t.Errorf("each unsupported form should warn (got %d): %v", len(unc.Uncoverable), unc.Uncoverable)
	}
	if len(unc.Columns) != 0 {
		t.Errorf("unsupported forms must NOT become silent column checks: %+v", unc.Columns)
	}

	// prose over-match guard: a non-assertion sentence must NOT yield a spurious column
	if pm := ParseDBExpect([]string{"the order is created and the webhook fires"}); len(pm.Columns) != 0 {
		t.Errorf("prose must not produce a spurious column: %+v", pm.Columns)
	}
}

// FINDING-7: validate-scenario warns (advisory, not invalid) when a content-layer
// scenario's EXPECT carries an assertion the layer template cannot execute.
func TestValidateScenario_WarnsOnUncoverableExpect(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: DB-005", "", "## Metadata", "- **ID**: DB-005", "- **Layer**: Database State", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT", "### Runnable", "- created_at within ±2s of now", "",
		"### Non-runnable", "- the row is written within two seconds", "",
	}, "\n")
	w := Warnings(md)
	if len(w) == 0 {
		t.Fatalf("a timestamp-range EXPECT on Database State should warn, got none")
	}
	// a fully-coverable DB scenario must NOT warn
	ok := strings.Join([]string{
		"# Scenario: DB-002", "", "## Metadata", "- **ID**: DB-002", "- **Layer**: Database State", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT", "### Runnable", "- customer_id == cust-db-002", "- row_count == 1", "",
		"### Non-runnable", "- exactly one order row is written", "",
	}, "\n")
	// ⚠ V31-002: assert on the SUBSTANCE of the warning this test owns, never on len(). Other rules
	// (VR12-E3's missing-sub-section warnings) legitimately add their own, and a count assertion
	// fails for their reason while reading as a regression in this one.
	if w2 := Warnings(ok); findWarning(w2, "not run-time-verified") {
		t.Errorf("coverable DB EXPECT should not warn, got %v", w2)
	}
	// a status-layer scenario with the same prose must NOT warn (grammar only applies to content layers)
	httpMd := strings.Replace(md, "Database State", "HTTP Ingestion", 1)
	if w3 := Warnings(httpMd); findWarning(w3, "not run-time-verified") {
		t.Errorf("HTTP layer should not get DB EXPECT warnings, got %v", w3)
	}
}

// UC-83: a Web UI scenario verifies rendered-DOM + "no backend 4xx/5xx"; an EXPECT that
// reaches past that (a DB row / saga / HTTP status code) WARNS (advisory, not invalid).
//
// ⛔ V31-006 gave both fixtures their `### Runnable` / `### Non-runnable` sub-sections. They were
// flat, and a flat file's RunnableExpect() is every bullet — so this test would have kept passing
// whatever the warning read, and it would stop meaning anything the day V31-002's R1 lands. With the
// split written out, it asserts what it claims to: the warning is about RUNNABLE lines.
func TestValidateScenario_WarnsOnUIExpectExceedingLayer(t *testing.T) {
	base := []string{
		"# Scenario: UI-009", "", "## Metadata", "- **ID**: UI-009", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
	}
	exceed := strings.Join(append(append([]string{}, base...),
		"## EXPECT", "### Runnable", "- the dashboard renders", "- a control_action saga is emitted",
		"- objects table has 1 row", "- status=200", "", "### Non-runnable", "- the flow is described in the spec file", ""), "\n")
	w := Warnings(exceed)
	if len(w) < 3 {
		t.Fatalf("a UI EXPECT asserting saga / DB row / HTTP status should warn (>=3), got %d: %v", len(w), w)
	}
	// a UI scenario whose EXPECT stays within the layer must NOT warn
	ok := strings.Join(append(append([]string{}, base...),
		"## EXPECT", "### Runnable", "- the dashboard renders", "- no backend 4xx/5xx during the flow", "",
		"### Non-runnable", "- the flow is described in the spec file", ""), "\n")
	if w2 := Warnings(ok); len(w2) != 0 {
		t.Errorf("an in-layer UI EXPECT should not warn, got %v", w2)
	}
}

// V31-006 (VR13-RB) test d: the Web UI warning reads `### Runnable` ONLY. It used to fire on a line
// the author had already moved to `### Non-runnable` — telling them to move it where it already was.
//
// ⚠ Never assert on len(w): E3 and E5 add their own warnings to these fixtures. Assert on the
// SUBSTANCE of the warning this rule owns.
func TestValidateScenario_UIWarningIgnoresNonRunnable(t *testing.T) {
	base := []string{
		"# Scenario: UI-009", "", "## Metadata", "- **ID**: UI-009", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
	}
	const needle = "exceeds the Web UI layer"
	count := func(ws []string) int {
		n := 0
		for _, w := range ws {
			if strings.Contains(w, needle) {
				n++
			}
		}
		return n
	}

	// the saga line is DOCUMENTATION — it must not warn
	documented := strings.Join(append(append([]string{}, base...),
		"## EXPECT", "### Runnable", "- the dashboard renders", "",
		"### Non-runnable", "- a control_action saga is emitted", ""), "\n")
	if n := count(Warnings(documented)); n != 0 {
		t.Errorf("a `### Non-runnable` saga line warned %d time(s) — it is documentation, and the warning told the author to move it where it already is", n)
	}

	// the mirror: the saga line as a CLAIM — it must warn, exactly once
	claimed := strings.Join(append(append([]string{}, base...),
		"## EXPECT", "### Runnable", "- a control_action saga is emitted", "",
		"### Non-runnable", "- the dashboard renders", ""), "\n")
	if n := count(Warnings(claimed)); n != 1 {
		t.Errorf("a `### Runnable` saga line warned %d time(s), want exactly 1", n)
	}
}

// findWarning reports whether any warning carries needle — the substance test every warning
// assertion in this package should use, rather than a count that other rules can move (V31-002).
func findWarning(ws []string, needle string) bool {
	for _, w := range ws {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
