package argus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneyallow_resolved_test.go — at the RUN door (runOneScenario): the resolved path is
// what is allowed (point 2c), and an amqp entry builds the step while an unlisted one is
// refused with no dial.
//
// As read before the change that introduced the resolved-path rule: these two tests were written
// FIRST and asserted the DEFECT — the money-handling run with NO money_writes block SENT both requests:
//
//	status=passed seen=[/api/v1/items/orders]
//	status=passed seen=[/api/v1/items/../orders/1]
//
// They now assert the fix (refused, nothing on the wire).

// resolvedPathServer answers GET /seed with {"id": <seedID>} and records every other request.
func resolvedPathServer(t *testing.T, seedID string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seed" {
			_, _ = w.Write([]byte(`{"id":"` + seedID + `"}`))
			return
		}
		mu.Lock()
		paths = append(paths, r.RequestURI)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// savedPathChainMD is a two-step chain: read an id from /seed, then GET <route>/${saved.id}. The file
// names no money word, so the static guard has nothing to refuse for the items route.
func savedPathChainMD(base, route string) string {
	trig := `{"steps":[
	  {"type":"http","name":"seed","method":"GET","url":"${BASE}/seed","save":{"id":"id"}},
	  {"type":"http","name":"use","method":"GET","url":"${BASE}` + route + `/${saved.id}"}
	]}`
	return chainRunMD(base, trig, "### Runnable\n- step seed: status=200\n- step use: status=200\n")
}

func runOne(c *config.Config, s *scenario.Scenario, ledger *scenario.MoneySpendLedger) report.ScenarioResult {
	return runOneScenario(c, scenarioFile{s: s}, "tr-allow", filepath.Join(os.TempDir(), "argus-allow-results"), nil,
		c.MoneyWriteAllowlist(), ledger, "", nil)
}

func stepNamed(res report.ScenarioResult, name string) *report.StepResult {
	for i := range res.Steps {
		if res.Steps[i].Name == name {
			return &res.Steps[i]
		}
	}
	return nil
}

func TestRunDoor_F20261011_3_SavedValueCannotResolveToAMoneyVerbPath(t *testing.T) {
	for _, saved := range []string{"orders", "../orders/1", "quote", "%6frders"} {
		srv, seen := resolvedPathServer(t, saved)
		s := scenario.Parse(savedPathChainMD(srv.URL, "/api/v1/items"))
		if v := scenario.MoneyGuardViolations(s, nil); len(v) != 0 {
			t.Fatalf("the static guard must accept the file (it names no money word): %v", v)
		}
		res := runOne(&config.Config{MoneyHandling: true}, s, &scenario.MoneySpendLedger{})
		if got := seen(); len(got) != 0 {
			t.Errorf("saved %q: the SUT received %v — a money-handling run sent a request that resolved to a money path", saved, got)
		}
		st := stepNamed(res, "use")
		// chain.Run records a step that did not go out as not-measured and stops the chain; the refusal's own
		// wording is what it carries (chain.notMeasuredReason), never "SUT unreachable".
		if st == nil || st.Status != report.StepNotMeasured || !strings.HasPrefix(st.Observed, "refused before firing: this SUT is declared money_handling, and ") {
			t.Errorf("saved %q: step use = %+v; want the money_handling refusal wording", saved, st)
		}
		if res.Status != report.StatusError {
			t.Errorf("saved %q: the scenario must be error (a refusal says nothing about the SUT), got %s", saved, res.Status)
		}
		if st != nil && len(saved) > 6 && strings.Contains(st.Observed, saved) {
			t.Errorf("saved %q: the refusal echoes the saved value: %s", saved, st.Observed)
		}
	}
	// a value that keeps the path off every money verb is sent, with no entry needed
	srv, seen := resolvedPathServer(t, "42")
	res := runOne(&config.Config{MoneyHandling: true}, scenario.Parse(savedPathChainMD(srv.URL, "/api/v1/items")), &scenario.MoneySpendLedger{})
	if res.Status != "passed" || len(seen()) != 1 {
		t.Fatalf("a plain read that needs no entry must be sent: %s seen=%v", res.Status, seen())
	}
	// and a run that is not money-handling is untouched
	srv, seen = resolvedPathServer(t, "orders")
	res = runOne(&config.Config{}, scenario.Parse(savedPathChainMD(srv.URL, "/api/v1/items")), &scenario.MoneySpendLedger{})
	if res.Status != "passed" || len(seen()) != 1 {
		t.Fatalf("a non-money run changed: %s seen=%v", res.Status, seen())
	}
}

func getOrdersConfig() *config.Config {
	no := false
	c := &config.Config{MoneyHandling: true, MoneyWrites: &config.MoneyWritesConfig{
		Allow: []config.MoneyWriteAllowEntry{{Method: "GET", Path: "/api/v1/orders/{id}", Spends: &no}}}}
	c.Project.Name = "p"
	return c
}

func TestRunDoor_AGETEntryAllowsItsRouteAndNotAnotherAfterTheValueResolves(t *testing.T) {
	for _, tc := range []struct {
		saved string
		sent  bool
	}{
		{"42", true},
		{"42/cancel", false},
		{"42%2Fcancel", false},
		{"..", false},
	} {
		srv, seen := resolvedPathServer(t, tc.saved)
		s := scenario.Parse(savedPathChainMD(srv.URL, "/api/v1/orders"))
		c := getOrdersConfig()
		if v := scenario.MoneyGuardViolations(s, c.MoneyWriteAllowlist()); len(v) != 0 {
			t.Fatalf("the file must pass on its entry: %v", v)
		}
		res := runOne(c, s, &scenario.MoneySpendLedger{})
		got := seen()
		if tc.sent && (len(got) != 1 || res.Status != "passed") {
			t.Errorf("saved %q: the listed route must be sent: %s %v", tc.saved, res.Status, got)
		}
		if !tc.sent && len(got) != 0 {
			t.Errorf("saved %q: the resolved request left the listed route and was SENT: %v", tc.saved, got)
		}
	}
	// without the entry the same file is refused statically, before anything runs
	srv, seen := resolvedPathServer(t, "42")
	c := getOrdersConfig()
	c.MoneyWrites = nil
	res := runOne(c, scenario.Parse(savedPathChainMD(srv.URL, "/api/v1/orders")), &scenario.MoneySpendLedger{})
	if res.Status != report.StatusError || len(seen()) != 0 || res.Failure == nil || !strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal) {
		t.Fatalf("without its entry a GET of an order path must be refused as before: %s %+v seen=%v", res.Status, res.Failure, seen())
	}
}

// ── the plain (JMeter) TRIGGER door ─────────────────────────────────────────────────────────────────

func TestRunDoor_PlainTriggerPlaceholderEntryIsBoundedInOneBucket(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	for _, id := range []string{"A1", "B2", "C3"} {
		writeMoneyWriteScenario(t, scDir, "PH-"+id, "POST", "http://sut.invalid/api/v1/orders/"+id+"/fill", `{"amount": 5}`)
	}
	yes := true
	cfg := &config.Config{MoneyHandling: true, MoneyWrites: &config.MoneyWritesConfig{Allow: []config.MoneyWriteAllowEntry{
		{Method: "POST", Path: "/api/v1/orders/{id}/fill", Spends: &yes, AmountField: "amount", MaxAmount: 10, MaxPerRun: 2}}}}
	cfg.Project.Name = "p"
	cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	fr := &fakeRunner{pass: map[string]bool{"PH-A1": true, "PH-B2": true, "PH-C3": true}}
	rr, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	var passed, refused int
	for _, id := range []string{"PH-A1", "PH-B2", "PH-C3"} {
		res, _ := rr.Report.Find(id)
		switch {
		case res == nil:
			t.Fatalf("%s missing from the report", id)
		case res.Status == "passed":
			passed++
		case res.Status == report.StatusError:
			refused++
			if !strings.Contains(res.Failure.Observed, "max_per_run (2)") {
				t.Errorf("%s: refused for the wrong reason: %s", id, res.Failure.Observed)
			}
		}
	}
	if passed != 2 || refused != 1 {
		t.Fatalf("three ids on one placeholder entry: passed=%d refused=%d, want 2 and 1 (one bucket)", passed, refused)
	}
}

func TestRunDoor_PlainTriggerGETEntryAllowsAnOrderPath(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMoneyWriteScenario(t, scDir, "G-OK", "GET", "http://sut.invalid/api/v1/orders/abc", "{}")
	writeMoneyWriteScenario(t, scDir, "G-NO", "GET", "http://sut.invalid/api/v1/orders/abc/cancel", "{}")
	fr := &fakeRunner{pass: map[string]bool{"G-OK": true, "G-NO": true}}
	cfg := getOrdersConfig()
	cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	rr, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res, _ := rr.Report.Find("G-OK"); res == nil || res.Status != "passed" {
		t.Errorf("the listed GET was not run: %+v", res)
	}
	if res, _ := rr.Report.Find("G-NO"); res == nil || res.Status != report.StatusError || !strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal) {
		t.Errorf("another route was not refused: %+v", res)
	}
}

// ── an amqp entry: the step is BUILT; an unlisted one is refused with no dial ──────────────────────

const allowedConsumeTrigger = `{"steps":[{"type":"amqp","name":"read","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"reply.inbox","wait":"1s"}]}`
const unlistedPublishTrigger = `{"steps":[{"type":"amqp","name":"send","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"msgbus.inbox","routing_key":"inbox.agent.hop","body":"hello"}]}`

func amqpAllowConfig() *config.Config {
	q := "reply.inbox"
	c := &config.Config{MoneyHandling: true, MoneyWrites: &config.MoneyWritesConfig{Allow: []config.MoneyWriteAllowEntry{
		{Engine: "amqp", Op: "consume", Queue: &q}}}}
	c.Project.Name = "p"
	return c
}

func TestRunDoor_AnAllowedAMQPStepIsBuiltAndAnUnlistedOneIsRefusedWithZeroDials(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{Code: 200}}
	withFakeBroker(t, f)
	c := amqpAllowConfig()

	allowed := scenario.Parse(chainRunMD("", allowedConsumeTrigger, "### Runnable\n- step read: broker accepts\n"))
	res := runOne(c, allowed, &scenario.MoneySpendLedger{})
	if res.Status == report.StatusError && res.Failure != nil && strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal) {
		t.Fatalf("the listed consume was refused by the money guard: %+v", res.Failure)
	}
	if len(f.dialed) != 1 || f.dialed[0] != "MSGBUS_ARGUS_TEST_URL" {
		t.Fatalf("the listed consume must be BUILT and dial once, by name: dialed=%v status=%s %+v", f.dialed, res.Status, res.Failure)
	}

	f.dialed, f.other = nil, 0
	unlisted := scenario.Parse(chainRunMD("", unlistedPublishTrigger, "### Runnable\n- step send: broker accepts\n"))
	res = runOne(c, unlisted, &scenario.MoneySpendLedger{})
	if res.Status != report.StatusError || res.Failure == nil || !strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal+`chain step send is a "amqp" step — against a money-handling system only GET http steps may run`) {
		t.Fatalf("an unlisted publish must be refused with the base sentence leading: %s %+v", res.Status, res.Failure)
	}
	if len(f.dialed) != 0 || len(f.bodies) != 0 || f.other != 0 {
		t.Fatalf("an unlisted publish touched the broker: dialed=%v bodies=%d other=%d", f.dialed, len(f.bodies), f.other)
	}

	// the same consume on a config with no money_writes block is refused as it always was, 0 dials
	f.dialed = nil
	bare := &config.Config{MoneyHandling: true}
	res = runOne(bare, allowed, &scenario.MoneySpendLedger{})
	if res.Status != report.StatusError || len(f.dialed) != 0 {
		t.Fatalf("with no entry the consume must stay refused: %s dialed=%v", res.Status, f.dialed)
	}
}
