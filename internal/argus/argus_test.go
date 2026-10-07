package argus

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// run_id = a sortable UTC date-time prefix + a 6-hex random suffix (R7/ADR-10, CP-M3-116: unique
// per mint — the bare-second stamp let same-second runs merge in the cloud ledger), filename/
// regex-safe, and free of '-'/':' so it stays a clean segment inside the correlation id.
func TestNewRunID_Format(t *testing.T) {
	id := NewRunID()
	// 2026-07-22: milliseconds replaced R7's 6-hex random suffix (owner remark "Run Id is too
	// long"). Uniqueness now holds by construction — the W1 fence serialises runs per instance —
	// so the id can read end-to-end as a timestamp. See runid_test.go for the full rationale.
	if !regexp.MustCompile(`^\d{8}T\d{6}\d{3}$`).MatchString(id) {
		t.Fatalf("run id must be YYYYMMDDThhmmssSSS (18 chars, milliseconds), got %q", id)
	}
	if strings.ContainsAny(id, "-:/ ") {
		t.Errorf("run id must not contain -, :, /, or space (cid + filename safety): %q", id)
	}
}

// The per-scenario correlation id embeds run + scenario so Loki can be sliced by both via
// regex; it ends with an 8-hex tiebreaker and is never a Prometheus label (NFR-3).
func TestNewScenarioCorrelationID_Format(t *testing.T) {
	const pfx = "tr-20260101T000000-ENG-MCP-001-" // a scenario id WITH dashes embeds verbatim
	cid := newScenarioCorrelationID("20260101T000000", "ENG-MCP-001")
	if !strings.HasPrefix(cid, pfx) {
		t.Fatalf("cid must be tr-<run>-<scenario>-<hex>, got %q", cid)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(strings.TrimPrefix(cid, pfx)) {
		t.Errorf("cid must end with an 8-hex tiebreaker, got %q", cid)
	}
	if newScenarioCorrelationID("20260101T000000", "ENG-MCP-001") == cid {
		t.Error("two cids for the same run+scenario must differ (hex tiebreaker)")
	}
}

// RunAll stamps the run_id + scenario_id into every scenario's correlation id (the Loki join key).
func TestRunAll_CorrelationIDEmbedsRunAndScenario(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	fr := &fakeRunner{pass: map[string]bool{"ORD-001": true}}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "20260101T000000", fr)
	if err != nil {
		t.Fatal(err)
	}
	cid := rr.CorrelationID["ORD-001"]
	if !strings.HasPrefix(cid, "tr-20260101T000000-ORD-001-") {
		t.Fatalf("RunAll must embed run_id + scenario_id in the cid, got %q", cid)
	}
	if res, _ := rr.Report.Find("ORD-001"); res.CorrelationID != cid {
		t.Errorf("scenario result cid %q != run cid %q", res.CorrelationID, cid)
	}
}

// Even on a dead-rig short-circuit (allErrored / preflight failure), each scenario's cid must
// embed run_id + scenario_id so Loki run/scenario slicing works for an errored run too.
func TestRunAll_AllErrored_CorrelationIDEmbedsRun(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	r := healthFailRunner{&fakeRunner{pass: map[string]bool{"ORD-001": true}}}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "20260101T000000", r)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("ORD-001")
	if res == nil || res.Status != report.StatusError {
		t.Fatalf("expected an errored scenario, got %+v", res)
	}
	if !strings.HasPrefix(res.CorrelationID, "tr-20260101T000000-ORD-001-") {
		t.Errorf("allErrored cid must embed run_id + scenario_id, got %q", res.CorrelationID)
	}
}

// FINDING-1 (2026-06-18): failure.observed must describe the responder's ACTUAL
// behaviour only — never the test's expected value (VR-C8). The expectation belongs
// in the `expected` field (test hat). Source-level guard — the leak was in judge().
func TestJudge_ObservedRealityOnly(t *testing.T) {
	_, observed := judge([]int{200}, []int{202}, nil) // status mismatch
	if strings.Contains(observed, "202") || strings.Contains(strings.ToLower(observed), "expected") {
		t.Fatalf("status-mismatch observed leaks the expected value: %q", observed)
	}
	if !strings.Contains(observed, "200") {
		t.Errorf("observed should state the actual code: %q", observed)
	}
	_, obs2 := judge([]int{200}, []int{202, 409}, nil) // count mismatch (idempotency-style)
	if strings.Contains(obs2, "202") || strings.Contains(obs2, "409") || strings.Contains(strings.ToLower(obs2), "expected") {
		t.Fatalf("count-mismatch observed leaks the expected value(s): %q", obs2)
	}
}

// VR12-E7 — REWRITTEN. This test used to assert the old SCRAPE, and its second case is the exact
// shape that made the scrape dangerous:
//
//	extractStatuses([]string{"First: status=202", "Second: status=409, body has same order_id"})
//
// Two prose sentences, two 3-digit numbers, and the runner therefore fired the request TWICE with a
// shared Idempotency-Key. That is a behaviour nobody declared, and on PERM-001 it was firing against
// three prose VARIANTS of a single unauthorized request.
//
// The replacement reads only the DECLARED, ANCHORED forms, from the RUNNABLE bullets.
func TestExtractDeclaredStatuses(t *testing.T) {
	t.Run("a single declared status", func(t *testing.T) {
		first, second := extractDeclaredStatuses([]string{"status=202", "body has order_id"})
		if first != 202 || second != 0 {
			t.Errorf("first=%d second=%d, want 202 / 0", first, second)
		}
	})

	t.Run("a declared replay", func(t *testing.T) {
		first, second := extractDeclaredStatuses([]string{"status=202", "status2=409"})
		if first != 202 || second != 409 {
			t.Errorf("first=%d second=%d, want 202 / 409", first, second)
		}
	})

	t.Run("⛔ prose that MENTIONS statuses declares nothing", func(t *testing.T) {
		first, second := extractDeclaredStatuses([]string{
			"First: status=202",
			"Second: status=409, body has same order_id",
		})
		if second != 0 {
			t.Errorf("a sentence must not declare a replay; second=%d", second)
		}
		if first != 0 {
			t.Errorf("an unanchored mention must not declare the status either; first=%d", first)
		}
	})
}

func TestDeriveProps(t *testing.T) {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	s := &scenario.Scenario{ID: "ORD-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders", Payload: `{"x":1}`},
		Expect:  []string{"status=202"}, ExpectRunnable: []string{"status=202"}}
	p := mustProps(t, c, s, "tr-xyz")
	if p["http.host"] != "order-api" || p["http.port"] != "8080" {
		t.Errorf("host/port: %v", p)
	}
	if p["trigger.path"] != "/api/v1/orders" {
		t.Errorf("trigger.path = %q", p["trigger.path"])
	}
	if p["expect.status"] != "202" || p["correlation.id"] != "tr-xyz" || p["trigger.method"] != "POST" {
		t.Errorf("props: %v", p)
	}
}

// Unit 6 (M25-FX3 / 4.8): a scenario's TRIGGER Authorization header OVERRIDES the config
// default token, so a wrong/absent token actually exercises the SUT's auth (no false bypass).
func TestDeriveProps_TriggerAuthOverride(t *testing.T) {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Targets.Auth = &config.AuthTarget{BearerToken: "tok-default"}
	base := &scenario.Scenario{ID: "PERM-001", Layers: []string{"Permissions"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"}, Expect: []string{"status=401"}, ExpectRunnable: []string{"status=401"}}

	if got := mustProps(t, c, base, "tr-1")["auth.header"]; got != "Bearer tok-default" {
		t.Errorf("no trigger header -> config default; got auth.header = %q", got)
	}
	base.Trigger.Headers = map[string]string{"Authorization": "Bearer wrong"}
	if got := mustProps(t, c, base, "tr-2")["auth.header"]; got != "Bearer wrong" {
		t.Errorf("trigger Authorization must override; got auth.header = %q", got)
	}
	base.Trigger.Headers = map[string]string{"Authorization": ""}
	if got, ok := mustProps(t, c, base, "tr-3")["auth.header"]; !ok || got != "" {
		t.Errorf("empty Authorization -> send no token (present+empty); got ok=%v %q", ok, got)
	}
}

// SECRETS-VAR-PARITY Gap-A guard (plan §2): a ${VAR} in targets.auth.bearer_token is resolved by
// config.Load (expandEnv, D2) and reaches JMeter via DeriveProps as "Bearer <resolved>" — the
// end-to-end HTTP token path. Loads through config.Load (not an in-memory Config) so expandEnv runs.
func TestDeriveProps_ResolvesBearerTokenFromEnv(t *testing.T) {
	t.Setenv("OS_TOKEN", "os-secret")
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: order-service\ntargets:\n  http:\n    base_url: http://order-api:8080\n  auth:\n    type: bearer\n    bearer_token: ${OS_TOKEN}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := &scenario.Scenario{ID: "ORD-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"}, Expect: []string{"status=202"}, ExpectRunnable: []string{"status=202"}}
	if got := mustProps(t, c, s, "tr-1")["auth.header"]; got != "Bearer os-secret" {
		t.Errorf("Gap A: DeriveProps must emit the Load-resolved token; got auth.header = %q, want Bearer os-secret", got)
	}
}

// Bodyless verbs (GET/HEAD/DELETE) must not carry a request body — JMeter rejects a
// raw body on GET, and health/readiness/discovery endpoints (any service) are GET.
func TestDeriveProps_GetSendsNoBody(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://social-gateway:8080"}
	s := &scenario.Scenario{ID: "SMCP-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "GET", URL: "${GW}/health"}, Expect: []string{"status=200"}, ExpectRunnable: []string{"status=200"}}
	p := mustProps(t, c, s, "tr-1")
	if p["trigger.payload"] != "" {
		t.Errorf("GET must send no body, got payload %q", p["trigger.payload"])
	}
	// POST with no payload still defaults to {} (unchanged behaviour).
	s.Trigger.Method = "POST"
	if p2 := mustProps(t, c, s, "tr-2"); p2["trigger.payload"] != "{}" {
		t.Errorf("POST no-payload should default to {}, got %q", p2["trigger.payload"])
	}
}

// Decision-2: saga-presence scenarios get the in-network Loki URL (the JSR223 verify
// step queries Loki from inside the JMeter container, NOT the host's localhost:3100)
// + the mandated saga step. Non-saga scenarios get neither.
func TestDeriveProps_SagaPresence(t *testing.T) {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	saga := &scenario.Scenario{ID: "SAGA-001", Layers: []string{"Rate Limiting"}, Tags: []string{"saga-presence", "control-action"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/admin/rate-limit"}, Expect: []string{"status=200"}, ExpectRunnable: []string{"status=200"}}
	p := mustProps(t, c, saga, "tr-saga")
	if p["loki.url"] != "http://loki:3100" {
		t.Errorf("saga-presence must query in-network Loki, got loki.url=%q", p["loki.url"])
	}
	if p["loki.expected_saga"] != "control_action" {
		t.Errorf("loki.expected_saga = %q, want control_action", p["loki.expected_saga"])
	}
	// a non-saga scenario must NOT carry the loki props
	plain := &scenario.Scenario{ID: "ORD-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"}, Expect: []string{"status=202"}, ExpectRunnable: []string{"status=202"}}
	if pp := mustProps(t, c, plain, "tr-x"); pp["loki.url"] != "" {
		t.Errorf("non-saga scenario should not carry loki.url, got %q", pp["loki.url"])
	}
}

// FINDING-7: content layers (Database State / Message Flow) get the structured EXPECT
// props so the template evaluates column/count/negative assertions; status layers don't.
func TestDeriveProps_ExpectGrammar(t *testing.T) {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	db := &scenario.Scenario{ID: "DB-002", Layers: []string{"Database State"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"},
		Expect:  []string{"row_count == 1", "customer_id == cust-db-002"}, ExpectRunnable: []string{"row_count == 1", "customer_id == cust-db-002"}}
	p := mustProps(t, c, db, "tr-db")
	if p["expect.has_rows"] != "true" || p["expect.row_count"] != "1" || p["expect.columns"] != "customer_id=cust-db-002" {
		t.Errorf("DB EXPECT props: has_rows=%q row_count=%q columns=%q", p["expect.has_rows"], p["expect.row_count"], p["expect.columns"])
	}
	// negative (DB-008): zero-rows scenario must not carry the correlation_id-substring fallback expectation
	// VR12-E9: a negative is DECLARED, not detected inside a sentence. The declared forms are
	// unchanged (`no rows` / `zero rows` / `0 rows` / `rejected` / `not persisted` /
	// `has_rows == false`); what changed is that one must BE the clause.
	neg := &scenario.Scenario{ID: "DB-008", Layers: []string{"Database State"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"},
		Expect:  []string{"no rows"}, ExpectRunnable: []string{"no rows"}}
	if pn := mustProps(t, c, neg, "tr-neg"); pn["expect.has_rows"] != "false" {
		t.Errorf("DB-008 negative must set expect.has_rows=false, got %q", pn["expect.has_rows"])
	}
	// ⛔ THE REGRESSION GUARD FOR THE ANCHOR. This fixture previously read "no row is persisted
	// (rejected)" — a SENTENCE — and the unanchored regex matched "rejected" inside it. Measured on
	// the shipped catalogue, that same accident fired on seven bullets whose only crime was quoting
	// a Neo4j error ("... no rows in result set") or explaining that "an unknown tool is rejected on
	// the protocol plane". A sentence must never invert a database assertion.
	prose := &scenario.Scenario{ID: "DB-008b", Layers: []string{"Database State"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"},
		Expect:  []string{"row_count == 1", "the malformed variant is rejected before it reaches the database"}, ExpectRunnable: []string{"row_count == 1", "the malformed variant is rejected before it reaches the database"}}
	if pp := mustProps(t, c, prose, "tr-prose"); pp["expect.has_rows"] == "false" {
		t.Error("a SENTENCE containing 'rejected' must not invert the database assertion (VR12-E9)")
	}
	// a status layer (HTTP Ingestion) must NOT carry expect.* content props
	http := &scenario.Scenario{ID: "ORD-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${INGESTION_URL}/api/v1/orders"}, Expect: []string{"status=202"}, ExpectRunnable: []string{"status=202"}}
	if ph := mustProps(t, c, http, "tr-h"); ph["expect.has_rows"] != "" || ph["expect.columns"] != "" {
		t.Errorf("status layer should not carry content EXPECT props: %v", ph)
	}
}

func TestTemplateBaseFor(t *testing.T) {
	cases := map[string]string{
		"HTTP Ingestion": "http-ingestion", "Database State": "database-state",
		"Message Flow": "message-flow", "External Delivery": "external-delivery",
		"Permissions": "http-ingestion",
		"Error Path":  "http-ingestion", "Rate Limiting": "http-ingestion",
	}
	for layer, want := range cases {
		if got := templateBaseFor(layer); got != want {
			t.Errorf("templateBaseFor(%q)=%q want %q", layer, got, want)
		}
	}
}

// fakeRunner writes a synthetic .jtl; success keyed by scenario.id via the pass set.
// failMsg (optional) is written into the failureMessage column for a failing id —
// content layers + saga-presence (Decision-2) read it as the observed reason.
type fakeRunner struct {
	pass    map[string]bool
	failMsg map[string]string
}

func (f *fakeRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	ok := f.pass[id]
	// On pass, echo the scenario's first expected status so status-judged layers match.
	code, msg, succ, fmsg := props["expect.status"], "OK", "true", ""
	if code == "" {
		code = "202"
	}
	if !ok {
		code, msg, succ = "400", "Bad Request", "false"
		fmsg = f.failMsg[id]
	}
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"
	if templateBase == "saga-presence" {
		// The .jtl saga-presence.jmx produces: the SUT trigger answers the DECLARED status in the
		// healthy AND the SAGA_SUPPRESS case (the saga is the assertion, not the code), and the
		// `<id>-verify-saga` JSR223 row carries the verdict + reality-only failureMessage. The
		// generic failure row below (a 400 on the trigger) would model a SUT that REFUSED the
		// request — a different defect, and one the declared-status gate now catches first.
		trig := props["expect.status"]
		if trig == "" {
			trig = "200"
		}
		row += "1781024939842,42," + id + "-trigger," + trig + ",OK," + templateBase + " " + id + " 1-1,true,\n" +
			"1781024939900,7," + id + "-verify-saga,,," + templateBase + " " + id + " 1-1," + succ + "," + fmsg + "\n"
		return os.WriteFile(jtlPath, []byte(row), 0o644)
	}
	row += "1781024939842,42," + id + "," + code + "," + msg + "," + templateBase + " " + id + " 1-1," + succ + "," + fmsg + "\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

func writeScenario(t *testing.T, dir, layer, id, expect string) {
	t.Helper()
	d := filepath.Join(dir, layer)
	_ = os.MkdirAll(d, 0o755)
	md := strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id,
		"- **Layer**: HTTP Ingestion", "", "## TRIGGER",
		// ⛔ V31-002 (R1): the old FLAT `## EXPECT` is no longer half-executed — a file with no
		// sub-heading declares NO runnable check. Every fixture says which bullets it means to be
		// executed, which is also what makes each test's intent readable.
		"POST \x60${INGESTION_URL}/api/v1/orders\x60", "", "## EXPECT", "### Runnable", "- " + expect, "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunAll_PassAndFail(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")
	results := filepath.Join(dir, "results")

	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}

	fr := &fakeRunner{pass: map[string]bool{"ORD-001": true}} // ORD-002 fails
	rr, err := RunAll(c, scDir, results, "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Total != 2 || rr.Report.Summary.Passed != 1 || rr.Report.Summary.Failed != 1 {
		t.Fatalf("summary = %+v", rr.Report.Summary)
	}
	if rr.CorrelationID["ORD-001"] == "" || rr.CorrelationID["ORD-002"] == "" {
		t.Errorf("correlation ids not recorded: %v", rr.CorrelationID)
	}
	res, _ := rr.Report.Find("ORD-002")
	if res == nil || res.Status != "failed" || res.Failure == nil {
		t.Fatalf("ORD-002 should be failed with a failure: %+v", res)
	}
	if !strings.Contains(res.Failure.Observed, "400") {
		t.Errorf("observed should carry the 400: %q", res.Failure.Observed)
	}
	if res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "202") {
		t.Errorf("expected should carry the EXPECT: %v", res.Failure.Expected)
	}
	if res.CorrelationID == "" {
		t.Error("failed scenario must carry a correlation_id")
	}
}

// countByOutcome tallies a RequestSample slice into the 3-way counts (test helper).
func countByOutcome(samples []report.RequestSample) (success, failed, errored int) {
	for _, s := range samples {
		switch s.Outcome {
		case report.OutcomeSuccess:
			success++
		case report.OutcomeFailed:
			failed++
		case report.OutcomeError:
			errored++
		}
	}
	return
}

// Test-requests panel (r3): classifyRequestSamples classifies each SUT-trigger sample by its OWN
// response code — any real 1xx/2xx/3xx success; >=400 failed (a negative RESPONSE); 0 (connection
// failure / non-numeric) error — and stamps each with its .jtl fire-time.
func TestClassifyRequestSamples_ByCode(t *testing.T) {
	got := classifyRequestSamples([]int{100, 200, 204, 301, 404, 500, 600, 0}, nil, []int64{10, 20, 30, 40, 50, 60, 70, 80}, nil, "")
	if s, f, e := countByOutcome(got); s != 4 || f != 3 || e != 1 {
		t.Fatalf("classify = success %d failed %d error %d; want 4/3/1 (1xx/2xx/3xx=success, >=400=failed, 0=error)", s, f, e)
	}
	if got[0].AtMs != 10 || got[4].AtMs != 50 || got[7].AtMs != 80 {
		t.Errorf("fire-time timestamps not preserved: %+v", got)
	}
	if len(classifyRequestSamples(nil, nil, nil, nil, "")) != 0 {
		t.Fatalf("no samples (dead rig / no .jtl) must yield an empty slice")
	}
}

// r3 gate-fix: the verify-sample exclusion is anchored to "<scenario.id>-verify", so a scenario
// whose id itself contains "verify" still counts its REAL SUT trigger (the old bare-substring
// matcher would have silently dropped it).
func TestClassifyRequestSamples_VerifyInScenarioID(t *testing.T) {
	id := "ORDE-verify-email"
	// trigger sample (template-prefixed) + the content-verify probe "<id>-verify".
	got := classifyRequestSamples([]int{202, 200}, []string{"http-ingestion " + id, id + "-verify"}, []int64{1, 2}, nil, id)
	if s, f, e := countByOutcome(got); s != 1 || f != 0 || e != 0 {
		t.Errorf("a scenario id containing 'verify' must still count its trigger (1 success); the -verify probe is excluded; got %d/%d/%d", s, f, e)
	}
}

// Test-requests panel HIGH-fix: the content/saga JMX templates emit a NON-SUT verification
// sampler (JDBC verify / RabbitMQ mgmt-API GET / Loki saga poll) alongside the SUT trigger.
// Those carry a "verify" label and must NOT be counted as requests sent to the SUT — and a
// code-less saga-verify sample (responseCode -> 0) must NOT become a phantom error response
// on a PASSING scenario.
func TestClassifyRequestSamples_ExcludesVerify(t *testing.T) {
	ts := []int64{1, 2}
	// database-state: trigger 202 (SUT) + verify 200 (JDBC) -> ONE SUT success request.
	if s, f, e := countByOutcome(classifyRequestSamples([]int{202, 200}, []string{"DB-001-trigger", "DB-001-verify"}, ts, nil, "DB-001")); s != 1 || f != 0 || e != 0 {
		t.Errorf("content-layer trigger+verify must count ONE SUT success, got %d/%d/%d", s, f, e)
	}
	// message-flow: trigger 202 (SUT) + verify-queue 200 (rabbitmq mgmt) -> ONE SUT success request.
	if s, f, e := countByOutcome(classifyRequestSamples([]int{202, 200}, []string{"MF-001-trigger", "MF-001-verify-queue"}, ts, nil, "MF-001")); s != 1 || f != 0 || e != 0 {
		t.Errorf("message-flow trigger+verify-queue must count ONE SUT success, got %d/%d/%d", s, f, e)
	}
	// saga-presence: trigger 202 (SUT) + verify-saga 0 (code-less Loki poll) -> 1 success, NO phantom error.
	if s, f, e := countByOutcome(classifyRequestSamples([]int{202, 0}, []string{"SAGA-001-trigger", "SAGA-001-verify-saga"}, ts, nil, "SAGA-001")); s != 1 || f != 0 || e != 0 {
		t.Errorf("saga verify sample must NOT be a phantom error, got %d/%d/%d", s, f, e)
	}
	// plain http-ingestion: single SUT sample (no -trigger/verify suffix) -> counted.
	if s, f, e := countByOutcome(classifyRequestSamples([]int{202}, []string{"http-ingestion ORD-001"}, []int64{1}, nil, "ORD-001")); s != 1 || f != 0 || e != 0 {
		t.Errorf("plain http sample must count as success, got %d/%d/%d", s, f, e)
	}
	// http-idempotency: two SUT trigger samples (202 then 409) -> 1 success + 1 FAILED (the 409
	// is a negative RESPONSE though the idempotency scenario PASSES); never an `error`.
	if s, f, e := countByOutcome(classifyRequestSamples([]int{202, 409}, []string{"http-idempotency ID-001", "http-idempotency ID-001"}, ts, nil, "ID-001")); s != 1 || f != 1 || e != 0 {
		t.Errorf("idempotency two triggers -> success 1 / failed 1 / error 0, got %d/%d/%d", s, f, e)
	}
}

// Test-requests panel (r3): a passed HTTP scenario records its 2xx sample as ReqSuccess; a
// failed one records its 4xx sample as ReqFailed (a negative RESPONSE, not an `error`). The
// sample also carries the .jtl fire-time (the distribution x-axis).
func TestRunAll_RequestCounts_HTTP(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")
	fr := &fakeRunner{pass: map[string]bool{"ORD-001": true}} // ORD-002 fails → code 400
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := rr.Report.Find("ORD-001")
	if ok.ReqSuccess != 1 || ok.ReqFailed != 0 || ok.ReqError != 0 {
		t.Errorf("ORD-001 (2xx) must be success=1 failed=0 error=0, got %d/%d/%d", ok.ReqSuccess, ok.ReqFailed, ok.ReqError)
	}
	if len(ok.Requests) != 1 || ok.Requests[0].AtMs != 1781024939842 || ok.Requests[0].Outcome != report.OutcomeSuccess {
		t.Errorf("ORD-001 sample must carry the .jtl fire-time + success outcome, got %+v", ok.Requests)
	}
	bad, _ := rr.Report.Find("ORD-002")
	if bad.ReqSuccess != 0 || bad.ReqFailed != 1 || bad.ReqError != 0 {
		t.Errorf("ORD-002 (400) must be success=0 failed=1 error=0, got %d/%d/%d", bad.ReqSuccess, bad.ReqFailed, bad.ReqError)
	}
}

// HEADLINE — request outcome ≠ scenario verdict. An error-path/permissions scenario that
// EXPECTs a 403 and GETS a 403 PASSES, yet the request is counted as an error RESPONSE
// (a 4xx). The pass/fail panels carry the verdict; this panel carries request health.
func TestRunAll_RequestOutcomeNotVerdict(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "permissions", "PERM-001", "status=403")
	fr := &fakeRunner{pass: map[string]bool{"PERM-001": true}} // pass ⇒ fakeRunner echoes code 403
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("PERM-001")
	if res.Status != "passed" {
		t.Fatalf("the error-path scenario must PASS (403 expected == 403 observed): %+v", res)
	}
	if res.ReqFailed != 1 || res.ReqSuccess != 0 || res.ReqError != 0 {
		t.Errorf("the 403 RESPONSE must count ReqFailed=1 (a negative response, NOT an `error`) even though the scenario PASSED, got success=%d failed=%d error=%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// twoSampleRunner writes a .jtl shaped like a content/saga template run: a SUT -trigger
// sample + a NON-SUT -verify sample — to prove RunAll counts ONLY the trigger as a SUT
// request through the full readJTLCodes -> labels -> classifyRequestSamples wiring.
type twoSampleRunner struct{ triggerCode, verifyCode string }

func (r twoSampleRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	rows := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1,10," + id + "-trigger," + r.triggerCode + ",OK,t 1-1,true,\n" +
		"2,20," + id + "-verify," + r.verifyCode + ",OK,t 1-2,true,\n"
	return os.WriteFile(jtlPath, []byte(rows), 0o644)
}

// End-to-end: a .jtl with a SUT trigger + a verification sampler must count exactly ONE SUT
// request through RunAll (the verify sample is the runner's infra assertion, not a SUT request).
func TestRunAll_RequestCounts_ExcludesVerifySample(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", twoSampleRunner{"202", "200"})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("ORD-001")
	if res.ReqSuccess != 1 || res.ReqFailed != 0 || res.ReqError != 0 {
		t.Errorf("trigger(202)+verify(200) must count ONE SUT success request, got success=%d failed=%d error=%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// partialDeadRigRunner writes a spurious .jtl sample BUT returns an executor failure — the
// rig died. The request counter must read 0 (the run never validly reached the SUT).
type partialDeadRigRunner struct{}

func (partialDeadRigRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	_ = os.WriteFile(jtlPath, []byte("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n1,1,"+id+",0,,t 1-1,false,\n"), 0o644)
	return errors.New(`jmeter exec failed: exit status 1: service "jmeter" is not running`)
}

// A dead-rig (executor failure) scenario must count 0 requests even if a partial .jtl exists.
func TestRunAll_RequestCounts_ZeroOnExecutorFailure(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", partialDeadRigRunner{})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("ORD-001")
	if res.Status != report.StatusError {
		t.Fatalf("expected StatusError (dead rig), got %s", res.Status)
	}
	if res.ReqSuccess != 0 || res.ReqFailed != 0 || res.ReqError != 0 || len(res.Requests) != 0 {
		t.Errorf("a dead-rig scenario must count 0/0/0 requests with no samples, got success=%d failed=%d error=%d samples=%d", res.ReqSuccess, res.ReqFailed, res.ReqError, len(res.Requests))
	}
}

// FINDING-6 (2026-06-19): the runner-core honors --layer/--tag (the CLI now wires
// them). These guard the filtering contract the CLI exercises. writeScenarioLT writes
// a scenario with an explicit primary layer + tags so the filters have something to bite.
func writeScenarioLT(t *testing.T, dir, layer, id string, tags []string) {
	t.Helper()
	slug := map[string]string{"HTTP Ingestion": "http-ingestion", "Rate Limiting": "rate-limiting", "Database State": "database-state"}[layer]
	d := filepath.Join(dir, slug)
	_ = os.MkdirAll(d, 0o755)
	lines := []string{"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: " + layer}
	if len(tags) > 0 {
		lines = append(lines, "- **Tags**: http, "+strings.Join(tags, ", "))
	}
	lines = append(lines, "", "## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "", "## EXPECT", "- status=202", "")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func filterRun(t *testing.T, layerFilter, tagFilter string) *report.Report {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioLT(t, scDir, "HTTP Ingestion", "ORD-001", []string{"critical"})
	writeScenarioLT(t, scDir, "HTTP Ingestion", "ORD-002", []string{"smoke"})
	writeScenarioLT(t, scDir, "Rate Limiting", "RL-001", []string{"critical"})
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	fr := &fakeRunner{pass: map[string]bool{"ORD-001": true, "ORD-002": true, "RL-001": true}}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", layerFilter, tagFilter, "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	return rr.Report
}

func TestRunAll_FilterByLayer(t *testing.T) {
	if got := filterRun(t, "Rate Limiting", "").Summary.Total; got != 1 {
		t.Fatalf("--layer 'Rate Limiting' should select 1 (RL-001), got %d", got)
	}
}

func TestRunAll_FilterByTag(t *testing.T) {
	if got := filterRun(t, "", "critical").Summary.Total; got != 2 {
		t.Fatalf("--tag critical should select 2 (ORD-001, RL-001), got %d", got)
	}
}

func TestRunAll_FilterCombined(t *testing.T) {
	if got := filterRun(t, "HTTP Ingestion", "critical").Summary.Total; got != 1 {
		t.Fatalf("--layer 'HTTP Ingestion' AND --tag critical should select 1 (ORD-001), got %d", got)
	}
}

func TestRunAll_FilterEmptyResult(t *testing.T) {
	rep := filterRun(t, "", "no-such-tag")
	if rep.Summary.Total != 0 || rep.Failed() {
		t.Fatalf("unknown tag should yield Total=0 and not-failed (exit OK), got total=%d failed=%v", rep.Summary.Total, rep.Failed())
	}
}

// writeSagaScenario writes a saga-presence-tagged Rate Limiting scenario (the
// control-action trigger; EXPECT status=200 so the transport passes, leaving the
// saga as the deciding assertion).
func writeSagaScenario(t *testing.T, dir, id string) {
	t.Helper()
	d := filepath.Join(dir, "rate-limiting")
	_ = os.MkdirAll(d, 0o755)
	md := strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id,
		"- **Layer**: Rate Limiting", "- **Tags**: http, saga-presence, control-action", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/admin/rate-limit\x60", "",
		"## EXPECT", "### Runnable", "- status=200", "- a control_action saga is emitted for the correlation_id", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sagaRun drives a saga-presence scenario through RunAll. Decision-2: the saga
// judgment runs IN JMeter (saga-presence.jmx), so RunAll judges by the .jtl success
// flag — these tests simulate the .jtl the JMX would produce (sagaFound + the
// reality-only failureMessage), proving RunAll wires the verdict correctly. The LIVE
// JMeter+Loki behaviour (real SAGA_SUPPRESS red + real Loki-unreachable fail-closed)
// is exercised by the compose red-fixtures (final gate), not here.
func sagaRun(t *testing.T, sagaFound bool, failMsg string) *report.ScenarioResult {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeSagaScenario(t, scDir, "SAGA-001")
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	fr := &fakeRunner{pass: map[string]bool{"SAGA-001": sagaFound}}
	if !sagaFound {
		fr.failMsg = map[string]string{"SAGA-001": failMsg}
	}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("SAGA-001")
	if res == nil {
		t.Fatal("SAGA-001 not in report")
	}
	return res
}

// The saga IS the assertion: the trigger returns 200 in BOTH the healthy and the
// SAGA_SUPPRESS case, so judging by HTTP code alone cannot tell them apart — the
// saga-presence.jmx Loki assertion (mirrored here as the .jtl success flag) decides it.
func TestRunAll_SagaPresence_PassWhenSagaPresent(t *testing.T) {
	res := sagaRun(t, true, "") // JMX found the control_action saga -> success
	if res.Status != "passed" {
		t.Fatalf("saga present => pass, got %s (%v)", res.Status, res.Failure)
	}
}

func TestRunAll_SagaPresence_FailWhenSagaAbsent(t *testing.T) {
	// SAGA_SUPPRESS path: trigger 200 but no saga emitted => the JMX marks the verify
	// step failed => MUST fail (this is the bug #5 guards).
	res := sagaRun(t, false, "mandated control_action saga absent for correlation_id tr-x (saga-presence FAIL)")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("saga absent => fail, got %s (%v)", res.Status, res.Failure)
	}
	if !strings.Contains(strings.ToLower(res.Failure.Observed), "saga") {
		t.Errorf("observed should name the missing saga: %q", res.Failure.Observed)
	}
}

func TestRunAll_SagaPresence_FailClosedWhenUnverifiable(t *testing.T) {
	// Loki unreachable: the JMX verify step fails closed (never silently passes).
	res := sagaRun(t, false, "saga-presence not verifiable: Loki unreachable (ConnectException) - failing closed")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("Loki unreachable => fail closed, got %s (%v)", res.Status, res.Failure)
	}
	if !strings.Contains(strings.ToLower(res.Failure.Observed), "failing closed") {
		t.Errorf("observed should state fail-closed: %q", res.Failure.Observed)
	}
}

// writeScenarioMD writes a WHOLE scenario file (writeScenario above writes a flat ## EXPECT, which
// cannot express the `### Runnable` / `### Non-runnable` split this round is about). V31-006.
func writeScenarioMD(t *testing.T, dir, layer, id, md string) {
	t.Helper()
	d := filepath.Join(dir, layer)
	_ = os.MkdirAll(d, 0o755)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// c (V31-006 / VR13-RB). The HTTP whole-answer body gate is turned on by hasBodyAssert, which read
// every bullet — so a `### Non-runnable` body line switched the gate on and the scenario was judged
// on an assertion its author had explicitly marked as documentation.
func TestRunAll_NonRunnableBodyLineDoesNotTurnOnTheBodyGate(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	md := strings.Join([]string{
		"# Scenario: GATE-001", "",
		"## Metadata",
		"- **ID**: GATE-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http", "",
		"## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT",
		"### Runnable",
		"- status=400", "",
		"### Non-runnable",
		"- body has error containing x", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	writeScenarioMD(t, scDir, "http-ingestion", "GATE-001", md)

	fr := &fakeRunner{pass: map[string]bool{}, failMsg: map[string]string{"GATE-001": "BODY-ASSERT-FAIL: marker"}}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("GATE-001")
	if res == nil {
		t.Fatal("GATE-001 not in the report")
	}
	if res.Status != "passed" {
		obs := ""
		if res.Failure != nil {
			obs = res.Failure.Observed
		}
		t.Fatalf("status = %q (observed %q), want passed — the body gate read a `### Non-runnable` line", res.Status, obs)
	}
}
