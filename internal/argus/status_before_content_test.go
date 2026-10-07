package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// THE DECLARED STATUS IS ASSERTED BEFORE THE CONTENT ASSERTION, IN EVERY LAYER.
//
// Measured on a live run (2026-09-17): MSGF-005 ("an unauthorized order publishes no event",
// `Permissions -> Message Flow`, `- status=401`) went GREEN while the SUT accepted the order with a
// wrong token and answered 202. The primary layer is a content layer, and the content branch of
// runOneScenario judged on the JMeter success flag alone — the queue assertion held, so the
// scenario passed, and the 401 the author declared was compared against nothing. No JMX template
// reads `expect.status` either (grep: zero hits across templates/*.jmx), so the status was
// asserted NOWHERE for a content-judged scenario.
//
// The rule: when a scenario declares `status=<code>`, the SUT-trigger response code is compared
// to it FIRST, in every layer. A mismatch is `failed` with an observed line naming the actual
// code, and the content assertion is not consulted. A scenario that declares no status keeps its
// content-only judgment (a content layer is not refused for lacking one — V29-016 is unchanged).

// contentTriggerRunner writes the .jtl a content template produces: verification samplers around a
// single SUT trigger. The trigger answers `code`; the content assertion holds iff contentOK. The
// verify samplers are labelled `<id>-verify-*` exactly as templates/message-flow.jmx labels them,
// and the baseline probe comes BEFORE the trigger, as in the real template — so a judge that read
// codes[0] would be comparing a management-API probe, not the SUT.
type contentTriggerRunner struct {
	code      string
	contentOK bool
	gotProps  map[string]string
}

func (r *contentTriggerRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.gotProps = props
	id := props["scenario.id"]
	verifyOK, failMsg := "true", ""
	if !r.contentOK {
		verifyOK, failMsg = "false", "1 message(s) found on the queue for this correlation_id"
	}
	rows := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939800,5," + id + "-verify-negative-baseline,200,OK," + templateBase + " 1-1,true,\n" +
		"1781024939842,42," + id + "-trigger," + r.code + ",," + templateBase + " 1-1,true,\n" +
		"1781024939900,7," + id + "-verify-queue,200,OK," + templateBase + " 1-1," + verifyOK + "," + failMsg + "\n"
	return os.WriteFile(jtlPath, []byte(rows), 0o644)
}

// writeContentScenario writes a `Permissions -> Message Flow` scenario (MSGF-005's shape) whose
// runnable bullets are exactly `runnable`.
func writeContentScenario(t *testing.T, dir, id string, runnable []string) {
	t.Helper()
	d := filepath.Join(dir, "message-flow")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: Permissions -> Message Flow",
		"- **Tags**: http, message-broker, permissions", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "Authorization: Bearer not-a-real-token", "",
		"## VERIFY", "N/A — the queue is read by the template.", "",
		"## EXPECT", "### Runnable",
	}
	for _, b := range runnable {
		lines = append(lines, "- "+b)
	}
	lines = append(lines, "", "### Non-runnable", "- no message with correlation_id on the incoming queue", "",
		"## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a rejected request writes nothing.", "")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runContentScenario(t *testing.T, id string, runnable []string, r *contentTriggerRunner) report.ScenarioResult {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeContentScenario(t, scDir, id, runnable)
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Targets.MessageBroker = &config.MQTarget{
		URL:           "amqp://argus:pw@rabbitmq:5672/",
		ManagementURL: "http://rabbitmq:15672",
		Queues:        map[string]string{"incoming": "orders.incoming.q"},
	}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if got := rr.Report.Summary.Total; got != 1 {
		t.Fatalf("expected exactly one scenario to run, got %d", got)
	}
	for _, l := range rr.Report.Layers {
		for _, s := range l.Scenarios {
			if s.ID == id {
				return s
			}
		}
	}
	t.Fatalf("scenario %s not in the report", id)
	return report.ScenarioResult{}
}

// The defect: expected 401, the SUT answered 202, the queue assertion held — must be `failed`.
func TestStatusBeforeContent_DeclaredStatusMismatchFailsBeforeContent(t *testing.T) {
	r := &contentTriggerRunner{code: "202", contentOK: true}
	res := runContentScenario(t, "MSGF-905", []string{"status=401", "no rows"}, r)
	if r.gotProps["expect.status"] != "401" {
		t.Fatalf("the declared status must reach the runner as expect.status=401, got %q", r.gotProps["expect.status"])
	}
	if res.Status != "failed" {
		t.Fatalf("declared status=401, SUT answered 202, content matched: want `failed`, got %q (failure=%+v)", res.Status, res.Failure)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "202") {
		t.Fatalf("the observed line must name the actual status 202, got %+v", res.Failure)
	}
	if res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, "status=401") {
		t.Fatalf("failure.expected must carry the declared status, got %+v", res.Failure)
	}
	// VR-C8: observed is reality-only — the declared value lives in failure.expected, never here.
	if strings.Contains(res.Failure.Observed, "401") {
		t.Fatalf("observed must not echo the declared status (holdout leak): %q", res.Failure.Observed)
	}
}

// Expected 401, actual 401, content holds — passed.
func TestStatusBeforeContent_DeclaredStatusMatchThenContentPasses(t *testing.T) {
	res := runContentScenario(t, "MSGF-906", []string{"status=401", "no rows"}, &contentTriggerRunner{code: "401", contentOK: true})
	if res.Status != "passed" {
		t.Fatalf("declared status=401, SUT answered 401, content matched: want `passed`, got %q (failure=%+v)", res.Status, res.Failure)
	}
}

// Expected 401, actual 401, content does NOT hold — the status gate passes and the content
// assertion still decides, with the content template's own message.
func TestStatusBeforeContent_DeclaredStatusMatchThenContentFails(t *testing.T) {
	res := runContentScenario(t, "MSGF-907", []string{"status=401", "no rows"}, &contentTriggerRunner{code: "401", contentOK: false})
	if res.Status != "failed" {
		t.Fatalf("status matched but the content assertion failed: want `failed`, got %q", res.Status)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "message(s) found on the queue") {
		t.Fatalf("a content failure keeps the template's observed message, got %+v", res.Failure)
	}
}

// No declared status — the content judgment alone decides, exactly as before.
func TestStatusBeforeContent_NoDeclaredStatusIsContentJudgmentAlone(t *testing.T) {
	r := &contentTriggerRunner{code: "202", contentOK: true}
	res := runContentScenario(t, "MSGF-908", []string{"no rows"}, r)
	if r.gotProps["expect.status"] != "" {
		t.Fatalf("no status declared, none may be invented: expect.status=%q", r.gotProps["expect.status"])
	}
	if res.Status != "passed" {
		t.Fatalf("no declared status, content matched: want `passed`, got %q (failure=%+v)", res.Status, res.Failure)
	}
	res = runContentScenario(t, "MSGF-909", []string{"no rows"}, &contentTriggerRunner{code: "202", contentOK: false})
	if res.Status != "failed" {
		t.Fatalf("no declared status, content did not match: want `failed`, got %q", res.Status)
	}
}
