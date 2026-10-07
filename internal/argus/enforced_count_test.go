package argus

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// assertions_enforced_count on the http/JMeter and mcp engines.
//
// The count is the number of CONTENT checks the judge enforced — body checks and database column
// checks. A status line (`status=…`, `result.isError == false`) is not a content check, so a
// scenario that declares only a status reports 0, which reads "only the status / no-error was
// checked". Before this, both engines left the field at 0 for EVERY scenario, so a scenario whose
// body and column checks were judged read exactly like one that only proved the call did not error.

// statusBodyRunner writes the .jtl http-ingestion.jmx produces for one SUT trigger: the response
// code is `code` (the declared status when empty) and the template's body assertion holds iff
// bodyOK — on a miss it carries the template's own BODY-ASSERT-FAIL marker.
type statusBodyRunner struct {
	code     string
	bodyOK   bool
	gotProps map[string]string
}

func (r *statusBodyRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.gotProps = props
	id := props["scenario.id"]
	code := r.code
	if code == "" {
		code = props["expect.status"]
	}
	succ, fmsg := "true", ""
	if !r.bodyOK {
		succ, fmsg = "false", "BODY-ASSERT-FAIL: response body did not satisfy the expected body assertion"
	}
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,42," + id + "," + code + ",OK," + templateBase + " " + id + " 1-1," + succ + "," + fmsg + "\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

func runOneHTTP(t *testing.T, layerDir, layer, id string, runnable []string, r Runner) report.ScenarioResult {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	lines := []string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: " + layer, "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "", "## EXPECT", "### Runnable",
	}
	for _, b := range runnable {
		lines = append(lines, "- "+b)
	}
	lines = append(lines, "", "### Non-runnable", "- body has note containing prose-only", "")
	writeScenarioMD(t, scDir, layerDir, id, strings.Join(lines, "\n"))
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find(id)
	if res == nil {
		t.Fatalf("scenario %s not in the report", id)
	}
	return *res
}

func assertEnforced(t *testing.T, res report.ScenarioResult, want int, mustMention ...string) {
	t.Helper()
	if res.AssertionsEnforcedCount != want || len(res.AssertionsEnforced) != want {
		t.Fatalf("assertions_enforced_count = %d, list = %q; want %d (status=%s failure=%+v)",
			res.AssertionsEnforcedCount, res.AssertionsEnforced, want, res.Status, res.Failure)
	}
	joined := strings.Join(res.AssertionsEnforced, "; ")
	for _, m := range mustMention {
		if !strings.Contains(joined, m) {
			t.Errorf("assertions_enforced %q does not mention %q", joined, m)
		}
	}
	for _, e := range res.AssertionsEnforced {
		if strings.Contains(e, "status") && !strings.Contains(e, "field status") {
			t.Errorf("a status line is not a content check and must not be listed: %q", e)
		}
	}
}

// HTTP: `status=200` plus two body checks, judged and passed — count 2, status not counted.
func TestEnforcedCount_HTTPTwoBodyChecksCountTwo(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "ENF-001",
		[]string{"status=200", "body has order_id containing ord-", "body contains accepted"},
		&statusBodyRunner{bodyOK: true})
	if res.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", res.Status, res.Failure)
	}
	assertEnforced(t, res, 2, "field order_id", "accepted")
}

// HTTP: status only — nothing but the status was checked, so 0.
func TestEnforcedCount_HTTPStatusOnlyIsZero(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "ENF-002", []string{"status=200"},
		&statusBodyRunner{bodyOK: true})
	if res.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", res.Status, res.Failure)
	}
	assertEnforced(t, res, 0)
}

// HTTP: the body plane is the one that failed — the body checks WERE evaluated (as on a chain step).
func TestEnforcedCount_HTTPBodyPlaneFailureStillCounts(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "ENF-003",
		[]string{"status=200", "body has order_id containing ord-", "body contains accepted"},
		&statusBodyRunner{bodyOK: false})
	if res.Status != "failed" {
		t.Fatalf("want failed, got %s", res.Status)
	}
	assertEnforced(t, res, 2)
}

// HTTP: the status plane failed first — the body verdict was never consulted, so 0.
func TestEnforcedCount_HTTPStatusPlaneFailureIsZero(t *testing.T) {
	res := runOneHTTP(t, "http-ingestion", "HTTP Ingestion", "ENF-004",
		[]string{"status=200", "body contains accepted"},
		&statusBodyRunner{code: "500", bodyOK: true})
	if res.Status != "failed" {
		t.Fatalf("want failed, got %s", res.Status)
	}
	assertEnforced(t, res, 0)
}

// Database State: the column checks the template evaluated — their count.
func TestEnforcedCount_DatabaseColumnsCountTheirColumns(t *testing.T) {
	r := &contentTriggerRunner{code: "202", contentOK: true}
	res := runOneHTTP(t, "database-state", "Database State", "ENF-005",
		[]string{"status=202", "customer_id == cust-db-002, currency == EUR, amount == 12"}, r)
	if res.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", res.Status, res.Failure)
	}
	assertEnforced(t, res, 3, "customer_id", "currency", "amount")

	// the content plane failed — the columns were still evaluated.
	res = runOneHTTP(t, "database-state", "Database State", "ENF-006",
		[]string{"status=202", "customer_id == cust-db-002, currency == EUR"},
		&contentTriggerRunner{code: "202", contentOK: false})
	if res.Status != "failed" {
		t.Fatalf("want failed, got %s", res.Status)
	}
	assertEnforced(t, res, 2)

	// the declared status failed first — the content was not consulted.
	res = runOneHTTP(t, "database-state", "Database State", "ENF-007",
		[]string{"status=202", "customer_id == cust-db-002"},
		&contentTriggerRunner{code: "500", contentOK: true})
	if res.Status != "failed" {
		t.Fatalf("want failed, got %s", res.Status)
	}
	assertEnforced(t, res, 0)
}

// MCP: only `result.isError == false` — 0. With a content check — ≥1.
func TestEnforcedCount_MCP(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc","state":"open"}`))
	defer ts.Close()

	only := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMDSplit("{}",
		"### Runnable\n- result.isError == false")), "tr-enf-0")
	if only.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", only.Status, only.Failure)
	}
	assertEnforced(t, only, 0)

	two := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMDSplit("{}",
		"### Runnable\n- result.isError == false\n- body has id containing abc\n- body contains open")), "tr-enf-2")
	if two.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", two.Status, two.Failure)
	}
	assertEnforced(t, two, 2, "abc", "open")

	// a body-plane miss: the checks were evaluated, so they are counted (as on a chain step).
	miss := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMDSplit("{}",
		"### Runnable\n- result.isError == false\n- body has id containing zzz")), "tr-enf-miss")
	if miss.Status != "failed" {
		t.Fatalf("want failed, got %s", miss.Status)
	}
	assertEnforced(t, miss, 1)
}

// AC-D16 — a declared `- broker refuses with <code>` is a content check like the database column
// checks above: the AMQP sampler judges it (never enforced.go, VR-C8 — one verdict, one place), so
// it is counted and named by the code the scenario declared.

// refusalContentRunner writes the .jtl message-flow.jmx produces when a refusal is declared: the
// ordinary trigger row plus the AMQP verify sample (`<id>-verify-amqp-refusal`), labelled exactly
// as the template labels its JavaSampler — whose OWN success flag is the Java sampler's verdict,
// never re-judged here.
type refusalContentRunner struct {
	code      string // the SUT trigger's response code
	refusalOK bool   // whether the AMQP verify sample (the Java sampler) succeeded
	gotProps  map[string]string
}

func (r *refusalContentRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.gotProps = props
	id := props["scenario.id"]
	verifyOK, failMsg := "true", ""
	if !r.refusalOK {
		verifyOK, failMsg = "false", "observed reply code 404 NOT_FOUND"
	}
	rows := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,42," + id + "-trigger," + r.code + ",," + templateBase + " 1-1,true,\n" +
		"1781024939900,7," + id + "-verify-amqp-refusal,200,OK," + templateBase + " 1-1," + verifyOK + "," + failMsg + "\n"
	return os.WriteFile(jtlPath, []byte(rows), 0o644)
}

// runRefusalScenario writes a plain `Message Flow` scenario (not `Permissions -> Message Flow`,
// unlike status_before_content_test.go's helper) with a message-broker target configured, so
// DeriveProps's AC-D16 branch actually arms.
func runRefusalScenario(t *testing.T, id string, runnable []string, r *refusalContentRunner) report.ScenarioResult {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	lines := []string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: Message Flow", "- **Tags**: http, message-flow", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "User-Id: guest", "",
		"## EXPECT", "### Runnable",
	}
	for _, b := range runnable {
		lines = append(lines, "- "+b)
	}
	lines = append(lines, "", "### Non-runnable", "- a fixture", "",
		"## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	writeScenarioMD(t, scDir, "message-flow", id, strings.Join(lines, "\n"))
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Targets.MessageBroker = &config.MQTarget{
		URL:           "amqp://argus:pw@rabbitmq:5672/",
		ManagementURL: "http://rabbitmq:15672",
		Queues:        map[string]string{"incoming": "orders.incoming.q"},
		Exchanges:     map[string]string{"incoming": "orders.incoming"},
		RoutingKeys:   map[string]string{"incoming": "orders.created"},
	}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find(id)
	if res == nil {
		t.Fatalf("scenario %s not in the report", id)
	}
	return *res
}

// A refusal declared and observed — the count is exactly 1, and names the declared code.
func TestEnforcedCount_MessageFlowRefusalCountsOne(t *testing.T) {
	res := runRefusalScenario(t, "ENF-020", []string{"broker refuses with 406"},
		&refusalContentRunner{code: "202", refusalOK: true})
	if res.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", res.Status, res.Failure)
	}
	assertEnforced(t, res, 1, "406")
}

// A refusal declared but NOT observed (the Java sampler's own verdict failed) — the check was
// still evaluated, so it is still counted, exactly like the database column checks above.
func TestEnforcedCount_MessageFlowRefusalStillCountsOnFailure(t *testing.T) {
	res := runRefusalScenario(t, "ENF-021", []string{"broker refuses with 406"},
		&refusalContentRunner{code: "202", refusalOK: false})
	if res.Status != "failed" {
		t.Fatalf("want failed, got %s", res.Status)
	}
	assertEnforced(t, res, 1, "406")
}

// No refusal declared — nothing but the status was checked, so 0 (mirrors
// TestEnforcedCount_HTTPStatusOnlyIsZero, on the Message Flow layer instead).
func TestEnforcedCount_MessageFlowNoRefusalDeclaredIsZero(t *testing.T) {
	res := runOneHTTP(t, "message-flow", "Message Flow", "ENF-022", []string{"status=202"},
		&statusBodyRunner{bodyOK: true})
	if res.Status != "passed" {
		t.Fatalf("want passed, got %s (%+v)", res.Status, res.Failure)
	}
	assertEnforced(t, res, 0)
}
