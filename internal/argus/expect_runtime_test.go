package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E7 / VR12-E9 — THE TWO PLACES WHERE PROSE SECRETLY DROVE THE RUNNER.
//
// V29-020 points 6 and 7. Both are the same defect at two addresses: a sentence written for a human
// changed what the product DID, silently.
//
//  1. VR12-E7 — extractStatuses scanned EVERY EXPECT bullet for "status"/"code" plus a 3-digit
//     number. TWO hits made DeriveProps set expect.status2 AND idempotency.key, which makes the
//     runner FIRE THE REQUEST A SECOND TIME with a shared Idempotency-Key.
//
//     ⛔ THIS WAS NOT THEORETICAL. Measured on the shipped catalogue 2026-09-10:
//     PERM-001-unauth-order-401 carries three bullets — "(a) no Authorization header at all:
//     status=401", "(b) … missing the \"Bearer \" prefix: status=401", "(c) wrong / garbage token:
//     status=401" — describing three VARIANTS of one unauthorized request. The scrape found three
//     401s and the runner has been sending that request TWICE, with an Idempotency-Key, on every
//     run. Nobody noticed. The row predicted the hazard in the other direction; the catalogue was
//     already suffering it in this one.
//
//     The fix is a DECLARED form: `status=<code>` and `status2=<code>`. "Send it twice" is now
//     stated on purpose instead of inferred by counting numbers in sentences.
//
//  2. VR12-E9 — dbNegativeRe was UNANCHORED, so the word "rejected" / "no rows" / "not persisted"
//     anywhere in a sentence flipped expect.has_rows to false, inverting what the scenario asserts
//     about the database. Its neighbour dbNullRe was already anchored, with a comment saying exactly
//     why ("this avoids matching mid-prose") — one of the two was fixed and the other was not.
//
// ⛔ BOTH conversions land in the SAME COMMIT as the filtering that makes them necessary. Filtering
// first would turn ORDE-013 into a one-request test that still went green.
//
// The acceptances below assert on the DERIVED PROPERTY SET — what the runner will actually send —
// never on a summary line.

func propsFor(t *testing.T, md string) map[string]string {
	t.Helper()
	s := scenario.Parse(md)
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.invalid:5432/t", Username: "u"}
	p, err := DeriveProps(c, s, "tr-test-0001")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	return p
}

func scen(layer, expectBody string) string {
	return strings.Join([]string{
		"# Scenario: t", "",
		"## Metadata",
		"- **ID**: T-001",
		"- **Layer**: " + layer,
		"- **Tags**: http, t", "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT",
		expectBody,
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func TestVR12E7_IdempotencyIsDeclared(t *testing.T) {
	t.Run("status2= declares the replay", func(t *testing.T) {
		p := propsFor(t, scen("HTTP Ingestion", "### Runnable\n- status=202\n- status2=409\n"))
		if p["expect.status"] != "202" {
			t.Errorf("expect.status = %q, want 202", p["expect.status"])
		}
		if p["expect.status2"] != "409" {
			t.Errorf("expect.status2 = %q, want 409 — status2= is the declared replay", p["expect.status2"])
		}
		if p["idempotency.key"] == "" {
			t.Error("declaring status2= must set idempotency.key — that is what makes the runner fire twice")
		}
	})

	t.Run("⛔ PERM-001's OLD shape: three prose variants must NOT fire a second request", func(t *testing.T) {
		// The real bullets from the shipped scenario, verbatim — the shape that has been sending
		// that request TWICE with an Idempotency-Key on every run.
		p := propsFor(t, scen("Permissions", "### Runnable\n"+
			"- (a) no Authorization header at all: status=401\n"+
			"- (b) Authorization header missing the \"Bearer \" prefix: status=401\n"+
			"- (c) wrong / garbage token: status=401\n"))
		if _, ok := p["expect.status2"]; ok {
			t.Errorf("THREE prose variants of one request must NOT become a replay; expect.status2 = %q", p["expect.status2"])
		}
		if _, ok := p["idempotency.key"]; ok {
			t.Error("no idempotency.key without a DECLARED status2= — this is the live PERM-001 defect")
		}
		// ⚠ And it declares no status either: an unanchored MENTION is not a declaration.
		//
		// ⛔ UPDATED BY V29-016. This case used to assert `expect.status == "202"` — it LOCKED IN the
		// fabrication, on the very scenario that produced the row. There is no default any more: the
		// property is simply ABSENT, and a scenario in this shape is refused at authoring time
		// (VR12-E6) and reported rather than judged if it predates the rule.
		if v, ok := p["expect.status"]; ok {
			t.Errorf("no status was DECLARED, so no expect.status may be emitted — got %q (V29-016 site 1)", v)
		}
	})

	t.Run("PERM-001 MIGRATED: one declared status, variants as prose, no replay", func(t *testing.T) {
		p := propsFor(t, scen("Permissions", "### Runnable\n- status=401\n\n"+
			"### Non-runnable\n"+
			"- (a) no Authorization header at all\n"+
			"- (b) Authorization header missing the \"Bearer \" prefix\n"+
			"- (c) wrong / garbage token\n"))
		if p["expect.status"] != "401" {
			t.Errorf("expect.status = %q, want 401", p["expect.status"])
		}
		if _, ok := p["idempotency.key"]; ok {
			t.Error("the migrated form must send ONE request")
		}
	})

	t.Run("a status code in ### Non-runnable is NEVER read", func(t *testing.T) {
		p := propsFor(t, scen("HTTP Ingestion", "### Runnable\n- status=202\n\n"+
			"### Non-runnable\n- a retry would be answered 409 by the idempotency layer\n"))
		if _, ok := p["expect.status2"]; ok {
			t.Errorf("prose must not drive the runner; expect.status2 = %q", p["expect.status2"])
		}
		if _, ok := p["idempotency.key"]; ok {
			t.Error("prose must not make the runner send a second request")
		}
	})
}

func TestVR12E9_NegativeDBAssertionIsAnchored(t *testing.T) {
	t.Run("a declared negative still works", func(t *testing.T) {
		p := propsFor(t, scen("Database State", "### Runnable\n- no rows\n"))
		if p["expect.has_rows"] != "false" {
			t.Errorf("expect.has_rows = %q, want false — 'no rows' is a real declared negative", p["expect.has_rows"])
		}
	})

	t.Run("⛔ 'rejected' mid-prose must NOT invert the database assertion", func(t *testing.T) {
		p := propsFor(t, scen("Database State", "### Runnable\n- row_count == 1\n\n"+
			"### Non-runnable\n- the malformed variant is rejected by the API before it reaches the database\n"))
		if p["expect.has_rows"] == "false" {
			t.Error("a NON-RUNNABLE sentence containing 'rejected' inverted the database assertion")
		}
		if p["expect.row_count"] != "1" {
			t.Errorf("expect.row_count = %q, want 1", p["expect.row_count"])
		}
	})

	t.Run("⛔ even inside ### Runnable, 'rejected' mid-sentence is not a negative", func(t *testing.T) {
		// The anchor is what makes this true: a negative form must BEGIN the clause, exactly as
		// dbNullRe already required.
		p := propsFor(t, scen("Database State", "### Runnable\n- row_count == 1\n- status=400\n"))
		if p["expect.has_rows"] == "false" {
			t.Error("no declared negative present, yet has_rows was inverted")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// V31-003 (VR13-CID) — THE CORRELATION ID REACHES THE REQUEST AND THE CHECKS.
//
// A scenario names the run's own id with a placeholder — `${cid}` or `${correlation_id}`. Argus
// filled it into SOME request fields and into NO check, so a check on a run-scoped value was
// compared against the literal text `…${cid}` and could never pass. Measured on 0.3.31 on every
// engine; the owner's RACE-002 and RACE-004 are the live casualties, and the skill TEACHES the form
// they used (`marker-${cid}`).
//
// ⚠ An environment `${VAR}` is deliberately NOT filled into a check: a check's value is printed in
// the report (assertions_enforced, failure.expected), so a secret in one would be published.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

func TestDeriveProps_CorrelationIDIsFilledIntoTheRequestAndTheChecks(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: t", "",
		"## Metadata",
		"- **ID**: T-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http", "",
		"## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders/ord-${cid}\x60",
		"```json",
		`{"order_id":"ord-${cid}","note":"n-${correlation_id}"}`,
		"```", "",
		"## EXPECT",
		"### Runnable",
		"- status=202",
		"- body has order_id containing ord-${cid}",
		"- body contains n-${correlation_id}", "",
		"### Non-runnable",
		"- the order is created", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	p := propsFor(t, md)

	const corr = "tr-test-0001"
	for _, c := range []struct{ key, want string }{
		{"trigger.path", "/api/v1/orders/ord-" + corr},
		{"expect.body.1.value", "ord-" + corr},
		{"expect.body.2.value", "n-" + corr},
		{"expect.body_contains", "ord-" + corr},
	} {
		if got := p[c.key]; got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}
	if !strings.Contains(p["trigger.payload"], "ord-"+corr) || !strings.Contains(p["trigger.payload"], "n-"+corr) {
		t.Errorf("trigger.payload = %q — both placeholders must be filled", p["trigger.payload"])
	}
	// the sweep: nothing the runner sends may still carry a placeholder
	for k, v := range p {
		if strings.Contains(v, "${cid}") || strings.Contains(v, "${correlation_id}") {
			t.Errorf("%s still carries a placeholder: %q", k, v)
		}
	}
}

func TestDeriveProps_CorrelationIDIsFilledIntoAColumnCheck(t *testing.T) {
	expect := strings.Join([]string{
		"### Runnable",
		"- customer_id == cust-${correlation_id}",
		"- note == n-${cid}", "",
		"### Non-runnable",
		"- one row is written",
	}, "\n")
	for _, layer := range []string{"Database State", "Message Flow"} {
		t.Run(layer, func(t *testing.T) {
			p := propsFor(t, scen(layer, expect))
			const want = "customer_id=cust-tr-test-0001;note=n-tr-test-0001"
			if got := p["expect.columns"]; got != want {
				t.Errorf("expect.columns = %q, want %q", got, want)
			}
		})
	}
}

// V31-003 fix 1 — inside a `matching` pattern the correlation id is REGEX-QUOTED.
func TestDeriveProps_CorrelationIDInAPatternIsQuoted(t *testing.T) {
	expect := strings.Join([]string{
		"### Runnable",
		"- status=202",
		"- body matching ^ord-${cid}$", "",
		"### Non-runnable",
		"- the id is matched exactly",
	}, "\n")
	s := scenario.Parse(scen("HTTP Ingestion", expect))
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	// ⛔ an id carrying regex metacharacters: the run path never applies the scenario-id rule, so
	// this really can reach the correlation id.
	p, err := DeriveProps(c, s, "tr-1-a.b(c)-x")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	const want = `^ord-tr-1-a\.b\(c\)-x$`
	if got := p["expect.body.1.value"]; got != want {
		t.Errorf("expect.body.1.value = %q, want %q — unquoted, the `.` and `(` change what the author's pattern matches", got, want)
	}
}

// V31-003 fix 3a (the owner's "yes") — the Authorization header was the ONE header sent raw.
func TestDeriveProps_AuthorizationHeaderIsFilledLikeEveryOtherHeader(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: t", "",
		"## Metadata", "- **ID**: T-002", "- **Layer**: Permissions", "- **Tags**: http", "",
		"## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders\x60",
		"Authorization: Bearer tok-${cid}", "",
		"## EXPECT",
		"### Runnable", "- status=401", "",
		"### Non-runnable", "- the token names this run", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	p := propsFor(t, md)
	if got := p["auth.header"]; got != "Bearer tok-tr-test-0001" {
		t.Errorf("auth.header = %q, want %q — it was the one header sent raw", got, "Bearer tok-tr-test-0001")
	}
}
