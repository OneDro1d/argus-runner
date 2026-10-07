package scenario

import (
	"strings"
	"testing"
)

// V30-003 — THE SECTION CONTRACT. One case per rule, plus the two properties that make the rules
// worth having: the message NAMES the offending text, and it offers the nearest defined name.

func fullMD(mut func(string) string) string {
	md := strings.Join([]string{
		"# Scenario: s", "",
		"## Metadata",
		"- **ID**: SEC-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
		"```json",
		`{"customer_id":"c1"}`,
		"```", "",
		"## VERIFY",
		"N/A — a status layer judges by response code.", "",
		"## EXPECT",
		"### Runnable",
		"- status=202", "",
		"## TIMEOUT", "15s", "",
		"## CLEANUP", "N/A — this scenario creates nothing.", "",
	}, "\n")
	if mut != nil {
		return mut(md)
	}
	return md
}

func TestV30003_TheContract(t *testing.T) {
	// The control. If this ever fails, every refusal below is meaningless.
	if _, errs := Validate(fullMD(nil)); len(errs) > 0 {
		t.Fatalf("the control scenario must be VALID: %v", errs)
	}

	cases := []struct {
		rule, name string
		mut        func(string) string
		want       string
	}{
		// ── S1 — only the seven `## ` names ───────────────────────────────────────────────────
		{"S1", "an undefined section", func(m string) string {
			return m + "\n## Notes\nsomething\n"
		}, "## Notes is not a defined section"},
		{"S1", "⭐ a TYPO in a defined name is caught AND named", func(m string) string {
			return strings.Replace(m, "## TIMEOUT", "## TIEMOUT", 1)
		}, "Did you mean `## TIMEOUT`?"},
		{"S1", "…and the typo'd section is REPORTED MISSING too, which is the real damage", func(m string) string {
			return strings.Replace(m, "## TIMEOUT", "## TIEMOUT", 1)
		}, "## TIMEOUT is required"},

		// ── M1 / M2 — the closed Metadata list ────────────────────────────────────────────────
		{"M1", "an unknown Metadata key", func(m string) string {
			return strings.Replace(m, "## Metadata\n", "## Metadata\n- **Owner**: me\n", 1)
		}, "`**Owner**` is not a defined Metadata key"},
		{"M1", "a misspelt key is named with its nearest match", func(m string) string {
			return strings.Replace(m, "- **Layer**:", "- **Layr**:", 1)
		}, "Did you mean `**Layer**`?"},
		{"M2", "Priority says it was REMOVED, not mistyped", func(m string) string {
			return strings.Replace(m, "## Metadata\n", "## Metadata\n- **Priority**: High\n", 1)
		}, "was REMOVED in 0.3.31"},

		// ── M3 / M4 — exactly one dispatch tag ────────────────────────────────────────────────
		{"M3", "no dispatch tag", func(m string) string {
			return strings.Replace(m, "- **Tags**: http, order", "- **Tags**: order", 1)
		}, "must carry exactly ONE dispatch tag"},
		{"M3", "⭐ a MISTYPED dispatch tag is named — it routes to the WRONG engine in silence", func(m string) string {
			return strings.Replace(m, "- **Tags**: http, order", "- **Tags**: chian, order", 1)
		}, "`chian` looks like `chain`"},
		{"M3", "two dispatch tags", func(m string) string {
			return strings.Replace(m, "- **Tags**: http, order", "- **Tags**: chain, mcp", 1)
		}, "carries 2 dispatch tags"},
		{"M4", "`http` is a legal dispatch tag", func(m string) string { return m }, ""},

		// ── T1 / T2 / T4 — the TRIGGER ────────────────────────────────────────────────────────
		{"T1", "an unknown METHOD verb", func(m string) string {
			return strings.Replace(m, "POST `${INGESTION_URL}", "FETCH `${INGESTION_URL}", 1)
		}, "is not a legal METHOD"},
		{"T2", "a prose line before the fence would be SENT as a header", func(m string) string {
			return strings.Replace(m, "POST `${INGESTION_URL}/api/v1/orders`\n",
				"POST `${INGESTION_URL}/api/v1/orders`\nNote: this endpoint is slow\n", 1)
		}, "is not a plausible header name"},
		{"T2", "…but a REAL header is untouched", func(m string) string {
			return strings.Replace(m, "POST `${INGESTION_URL}/api/v1/orders`\n",
				"POST `${INGESTION_URL}/api/v1/orders`\nX-Tenant: acme\nAccept: application/json\n", 1)
		}, ""},
		{"T4", "a ```json payload that does not parse", func(m string) string {
			return strings.Replace(m, `{"customer_id":"c1"}`, `{"customer_id":"c1"`, 1)
		}, "does not parse as JSON"},
		{"T4", "⭐ …and a ```text payload DECLARES that it is deliberately not JSON", func(m string) string {
			return strings.Replace(m, "```json\n{\"customer_id\":\"c1\"}\n```",
				"```text\n{ \"customer_id\": \"poison\"\n```", 1)
		}, ""},
		{"T4", "a ${VAR} in a payload is not a parse error", func(m string) string {
			return strings.Replace(m, `{"customer_id":"c1"}`, `{"customer_id":"${CUSTOMER}"}`, 1)
		}, ""},

		// ── VF1 / VF3 / VF10 — the VERIFY ─────────────────────────────────────────────────────
		{"VF1", "a VERIFY on an mcp scenario", func(m string) string {
			return strings.Replace(m, "- **Tags**: http, order", "- **Tags**: mcp, order", 1)
		}, "has no meaning on a `mcp` scenario"},
		{"VF3", "a prose-only VERIFY", func(m string) string {
			return strings.Replace(m, "N/A — a status layer judges by response code.", "We check the response.", 1)
		}, "## VERIFY is prose only"},
		{"VF10", "a bare fence in VERIFY", func(m string) string {
			return strings.Replace(m, "N/A — a status layer judges by response code.",
				"```\nGET ${WEBHOOK_URL}/received\n```", 1)
		}, "a bare or ```http fence is not a defined form"},

		// ── TO1 / TO2 / TO3 — the TIMEOUT ─────────────────────────────────────────────────────
		{"TO1", "no TIMEOUT section", func(m string) string {
			return strings.Replace(m, "## TIMEOUT\n15s\n\n", "", 1)
		}, "## TIMEOUT is required"},
		{"TO2", "a TIMEOUT that is not a duration", func(m string) string {
			return strings.Replace(m, "## TIMEOUT\n15s", "## TIMEOUT\nquick", 1)
		}, "does not match"},
		{"TO3", "a TIMEOUT over the single-hop ceiling", func(m string) string {
			return strings.Replace(m, "## TIMEOUT\n15s", "## TIMEOUT\n45s", 1)
		}, "exceeds the ceiling of 30s"},
	}

	for _, c := range cases {
		t.Run(c.rule+": "+c.name, func(t *testing.T) {
			_, errs := Validate(c.mut(fullMD(nil)))
			if c.want == "" {
				for _, e := range errs {
					t.Errorf("must be ACCEPTED, got: %s", e.Message)
				}
				return
			}
			if !find(errs, c.want) {
				t.Fatalf("want a refusal containing %q; got %v", c.want, errs)
			}
		})
	}
}

// ⭐ TO3 — THE CEILING FOLLOWS THE SCENARIO'S SHAPE, and that is a DELIBERATE amendment to
// specs/06 NFR-5's flat 30s. Measured on the shipped catalogue, a flat 30s refused 41 of 119
// scenarios — 36 chains at 120s and 5 chained-layer scenarios at 45s — which would have made them
// red for a reason nobody chose. The numbers below are the estate's own measured values.
func TestV30003_TimeoutCeilingFollowsTheShape(t *testing.T) {
	cases := []struct {
		name, tags, layer, timeout string
		ok                         bool
	}{
		{"a single hop at 15s", "http", "HTTP Ingestion", "15s", true},
		{"a single hop at 45s", "http", "HTTP Ingestion", "45s", false},
		{"a chained layer at 45s", "http", "HTTP Ingestion -> Database State", "45s", true},
		{"a chained layer at 90s", "http", "HTTP Ingestion -> Database State", "90s", false},
		{"rate limiting at 60s", "http", "Rate Limiting", "60s", true},
		{"a chain at 120s", "chain", "Permissions", "120s", true},
		{"a chain at 300s", "chain", "Permissions", "300s", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := fullMD(func(m string) string {
				m = strings.Replace(m, "- **Tags**: http, order", "- **Tags**: "+c.tags+", order", 1)
				m = strings.Replace(m, "- **Layer**: HTTP Ingestion", "- **Layer**: "+c.layer, 1)
				return strings.Replace(m, "## TIMEOUT\n15s", "## TIMEOUT\n"+c.timeout, 1)
			})
			_, errs := Validate(md)
			if got := find(errs, "exceeds the ceiling"); got == c.ok {
				t.Fatalf("timeout %s on %q/%q: refused=%v, want refused=%v (%v)",
					c.timeout, c.tags, c.layer, got, !c.ok, errs)
			}
		})
	}
}

// VR12-S2 — THE LISTS COME FROM THE SCHEMA, not from a second copy in Go. A drift test that only
// compared two Go lists would prove nothing; this asserts the loaded values themselves.
func TestV30003_TheListsAreLoadedFromTheSchema(t *testing.T) {
	if got := strings.Join(SectionNames(), ","); got != "Metadata,TRIGGER,VERIFY,EXPECT,TIMEOUT,CLEANUP,References,LOAD,COMPARE" {
		// ARGUS-CMP-2: the ninth section. The ONLY existing assertion this PR changes.
		t.Errorf("the nine sections came back as %q", got)
	}
	if got := strings.Join(MetadataKeys(), ","); got != "ID,Layer,Tags,Target" {
		t.Errorf("the Metadata keys came back as %q — `Priority` must be gone (VR12-M2)", got)
	}
	if got := strings.Join(DispatchTags(), ","); got != "chain,mcp,ui,http" {
		t.Errorf("the dispatch tags came back as %q", got)
	}
	if got := strings.Join(Methods(), ","); got != "GET,POST,PUT,PATCH,DELETE" {
		t.Errorf("the METHOD enum came back as %q", got)
	}
	// The dispatch list the SCHEMA carries must be the one the RUNTIME keys on, or a validator
	// refuses scenarios the runner would have run — the 83-scenario false refusal this round's own
	// measurement caught.
	for _, tag := range []string{ChainTag, MCPTag, UITag} {
		if !in(tag, DispatchTags()) {
			t.Errorf("the runtime dispatches on %q and the schema does not list it", tag)
		}
	}
}
