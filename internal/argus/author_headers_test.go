package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-T3 (V29-018) — EVERY AUTHOR-DECLARED HEADER IS SENT, ON ALL THREE PATHS.
//
// The parse side always existed: `Trigger.Headers` was populated for every `Name: value` line, and
// exactly ONE key was consumed downstream. The map was fully populated and then all but one key was
// dropped between the parser and the wire.

func headerScenario(t *testing.T, layer, tags string, headers ...string) *scenario.Scenario {
	t.Helper()
	lines := []string{
		"# Scenario: h", "",
		"## Metadata",
		"- **ID**: HDR-001",
		"- **Layer**: " + layer,
		"- **Tags**: http, " + tags, "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
	}
	lines = append(lines, headers...)
	lines = append(lines,
		"", "```json", `{"x":1}`, "```", "",
		"## EXPECT", "### Runnable", "- status=202", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return scenario.Parse(strings.Join(lines, "\n"))
}

// PATH 1 — DeriveProps emits the numbered set, and a scenario with no author headers emits NOTHING,
// so its bytes on the wire are identical to before this change.
func TestVR12T3_DerivePropsEmitsTheNumberedHeaderSet(t *testing.T) {
	s := headerScenario(t, "HTTP Ingestion", "order",
		"Accept: application/json, text/event-stream",
		"X-Tenant: acme")
	p := mustPropsFor(t, s)

	if p["trigger.header_n"] != "2" {
		t.Fatalf("trigger.header_n = %q, want 2 — every declared header is propagated", p["trigger.header_n"])
	}
	// Sorted by canonical name, so two identical runs are identical on the wire.
	if p["trigger.header_1_name"] != "Accept" || p["trigger.header_1_value"] != "application/json, text/event-stream" {
		t.Errorf("header 1 = %q: %q", p["trigger.header_1_name"], p["trigger.header_1_value"])
	}
	if p["trigger.header_2_name"] != "X-Tenant" || p["trigger.header_2_value"] != "acme" {
		t.Errorf("header 2 = %q: %q", p["trigger.header_2_name"], p["trigger.header_2_value"])
	}

	// ⚠ Authorization is NOT in the set — it keeps its own `auth.header` path, including the 4.8
	// present-but-empty override that permission scenarios rely on. Two mechanisms for one header
	// would be two answers to one question.
	a := mustPropsFor(t, headerScenario(t, "HTTP Ingestion", "order", "Authorization: Bearer ${TOKEN}"))
	if a["trigger.header_n"] != "" {
		t.Errorf("Authorization must not enter the numbered set: %v", a)
	}
	if a["auth.header"] == "" {
		t.Error("…and it must still take its own path")
	}

	// No headers ⇒ nothing emitted ⇒ byte-identical to today.
	none := mustPropsFor(t, headerScenario(t, "HTTP Ingestion", "order"))
	for k := range none {
		if strings.HasPrefix(k, "trigger.header") {
			t.Errorf("a scenario with no author headers must emit no header property, got %q", k)
		}
	}
}

// T3.d — `${cid}` and `${VAR}` resolve in a header VALUE, by the same mechanism the token uses.
func TestVR12T3_HeaderValuesResolve(t *testing.T) {
	s := headerScenario(t, "HTTP Ingestion", "order", "X-Run: ${cid}")
	p := mustPropsFor(t, s)
	if p["trigger.header_1_value"] != "tr-test-0001" {
		t.Errorf("${cid} must resolve in a header value, got %q", p["trigger.header_1_value"])
	}
}

// ⛔ THE Go↔JMX CONTRACT. DeriveProps computes these in Go and Groovy consumes them; nothing checked
// that the two agreed, and this build has already seen that gap cost twice (V30-004, and the body
// set). Asserted on BOTH sides, on all SIX request templates.
func TestVR12T3_TheSixTemplatesReadWhatDeriveEmits(t *testing.T) {
	for _, name := range []string{
		"http-ingestion.jmx", "http-idempotency.jmx", "database-state.jmx",
		"message-flow.jmx", "external-delivery.jmx", "saga-presence.jmx",
	} {
		jmx := readTemplate(t, name)
		for _, key := range []string{
			`trigger.header_n`,
			`"trigger.header_" + i + "_name"`,
			`"trigger.header_" + i + "_value"`,
		} {
			if !strings.Contains(jmx, key) {
				t.Errorf("%s does not read %q — DeriveProps would emit a property nothing consumes, "+
					"which is exactly the defect this round keeps finding", name, key)
			}
		}
		// An author header REPLACES the runner's for the same name: remove, then add.
		if !strings.Contains(jmx, "removeHeaderNamed") {
			t.Errorf("%s must REPLACE a same-named runner header, not add a duplicate", name)
		}
	}
}

// ⭐ THE IDEMPOTENCY REPLAY SENDS THEM TOO. http-idempotency fires the SAME request twice; a header
// applied to only the first would make the two requests differ in exactly the way the scenario is
// testing they do not.
func TestVR12T3_TheIdempotencyReplayCarriesTheHeadersToo(t *testing.T) {
	jmx := readTemplate(t, "http-idempotency.jmx")
	if n := strings.Count(jmx, "apply author headers (VR12-T3)"); n != 2 {
		t.Fatalf("http-idempotency.jmx applies the author headers to %d of its 2 samplers", n)
	}
}

// PATH 2 — the single-MCP client actually puts them on the wire.
func TestVR12T3_MCPPathSendsAuthorHeaders(t *testing.T) {
	var got http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got == nil {
			got = r.Header.Clone()
		}
		fakeMCPTextServer(`{"ok":true}`)(w, r)
	}))
	defer ts.Close()

	md := strings.Join([]string{
		"# Scenario: h", "",
		"## Metadata",
		"- **ID**: HDR-MCP",
		"- **Layer**: Permissions",
		"- **Tags**: mcp", "",
		"## TRIGGER",
		"POST `" + ts.URL + "`",
		"X-Tenant: acme", "",
		"```json",
		`{"transport":"streamable-http","tool":"t","args":{}}`,
		"```", "",
		"## EXPECT", "### Runnable", "- result.isError == false", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	runMCPScenario(chainCfg(t, ts.URL), scenario.Parse(md), "tr-hdr-1")

	if got == nil {
		t.Fatal("the fake server was never called")
	}
	if got.Get("X-Tenant") != "acme" {
		t.Fatalf("the author's header did not reach the wire on the mcp path; sent: %v", headerNames(got))
	}
}

func headerNames(h http.Header) []string {
	var out []string
	for k := range h {
		out = append(out, k) // ⛔ NAMES ONLY — a value is the likeliest place to hold a credential
	}
	return out
}

// mustPropsFor is propsFor for an already-parsed scenario (propsFor takes markdown).
func mustPropsFor(t *testing.T, s *scenario.Scenario) map[string]string {
	t.Helper()
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.invalid:5432/t", Username: "u"}
	p, err := DeriveProps(c, s, "tr-test-0001")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	return p
}
