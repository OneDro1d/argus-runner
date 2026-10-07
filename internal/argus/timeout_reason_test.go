package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// timeoutSampleRunner writes the .jtl a REAL JMeter produces when an HTTPSampler's own
// connect_timeout/response_timeout fires (VR12-TO-RUN's per-request enforcement,
// HTTPSampler.connect_timeout/response_timeout = ${__P(trigger.timeout_ms,)}): a non-numeric
// responseCode (parsed as 0) and a responseMessage naming the socket-level timeout — the shape
// documented on isSamplerTimeoutMessage. It never sleeps: this is a report-classification test, not
// a process-timing one (that is local_runner_timeout_test.go's job).
type timeoutSampleRunner struct{}

func (timeoutSampleRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,dataType,success,failureMessage\n" +
		"1781024939842," + props["trigger.timeout_ms"] + "," + id + ",Non HTTP response code: java.net.SocketTimeoutException," +
		"Non HTTP response message: Read timed out," + templateBase + " " + id + " 1-1,text,false,\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

// TestRunOneScenario_SampleTimeoutIsFailedNotErrored is EVIDENCE ITEM 1 (item 3.1): a request to
// the SUT that misses the scenario's declared `## TIMEOUT` must be judged as a `failed` SAMPLE
// (a measurement WAS taken: the SUT was slow, not absent) whose Observed reason names the TIMEOUT —
// never `error`/StatusError, which VR12-CH's own NeverReachedSUT() reserves for a genuinely
// unreachable rig (deadsut_test.go's twin fixture).
func TestRunOneScenario_SampleTimeoutIsFailedNotErrored(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	md := strings.Join([]string{
		"# Scenario: SLOW-001", "",
		"## Metadata",
		"- **ID**: SLOW-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http", "",
		"## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT",
		"### Runnable",
		"- status=202", "",
		"## TIMEOUT", "15s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	writeScenarioMD(t, scDir, "http-ingestion", "SLOW-001", md)

	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", timeoutSampleRunner{})
	if err != nil {
		t.Fatal(err)
	}
	rep := rr.Report
	if rep.Summary.Failed != 1 || rep.Summary.Errored != 0 {
		t.Fatalf("summary = failed:%d errored:%d, want 1/0 — a JMeter sampler TIMEOUT is a MEASURED "+
			"SUT-too-slow result, never an absent-rig error", rep.Summary.Failed, rep.Summary.Errored)
	}
	s := rep.Layers[0].Scenarios[0]
	if s.NeverReachedSUT() {
		t.Fatalf("%s counts read as never-reached-SUT (ok=%d fail=%d err=%d) — a sampler TIMEOUT must "+
			"count as a FAILED response, not an errored one", s.ID, s.ReqSuccess, s.ReqFailed, s.ReqError)
	}
	if s.Failure == nil {
		t.Fatal("no failure recorded")
	}
	obs := s.Failure.Observed
	if !strings.Contains(obs, "TIMEOUT") || !strings.Contains(obs, "15s") {
		t.Fatalf("Observed = %q, want it to name the declared TIMEOUT and its 15s value", obs)
	}
	t.Logf("GREEN: status=%s observed=%q req(ok=%d fail=%d err=%d)", s.Status, obs, s.ReqSuccess, s.ReqFailed, s.ReqError)
}

// TestJmeterProcessBackstop_LoadScenarioNotKilled is EVIDENCE ITEM 2 (item 3.2): a `## LOAD`
// profile of ramp 2s + duration 3s (5s of legitimate thread-group wall clock) with a short declared
// `## TIMEOUT` (1s, well under the load window) must NOT be killed by the process backstop — the
// pre-existing bug (3d91fd0's first cut passed the raw TimeoutDuration straight into Run as the
// WHOLE-PROCESS deadline) would have killed this on every run, since 1s < 5s.
func TestJmeterProcessBackstop_LoadScenarioNotKilled(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	md := strings.Join([]string{
		"# Scenario: LOAD-001", "",
		"## Metadata",
		"- **ID**: LOAD-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http", "",
		"## TRIGGER",
		"POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## LOAD",
		"- **Users**: 5",
		"- **Ramp Seconds**: 2",
		"- **Duration Seconds**: 3",
		"- **Target P95 Ms**: 5000",
		"- **Max Error Rate**: 1.0", "",
		"## EXPECT",
		"### Runnable",
		"- status=202", "",
		"## TIMEOUT", "1s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-001", md)

	// The fake jmeter "runs" for slightly less than ramp+duration (a HEALTHY load run finishing on
	// its own schedule) — well past the 1s declared TIMEOUT, exactly the regression this backstop
	// formula exists to not misfire on.
	runner := &loadAwareFakeRunner{sleep: 4 * time.Second, code: "202"}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", runner)
	if err != nil {
		t.Fatal(err)
	}
	rep := rr.Report
	if rep.Summary.Failed != 0 || rep.Summary.Errored != 0 || rep.Summary.Passed != 1 {
		t.Fatalf("summary = passed:%d failed:%d errored:%d, want 1/0/0 — a healthy load run that takes "+
			"longer than the per-request TIMEOUT (by design) must NOT be killed by the process backstop",
			rep.Summary.Passed, rep.Summary.Failed, rep.Summary.Errored)
	}
	if !runner.deadlineWasGenerous {
		t.Fatal("the runner never observed a deadline generous enough to cover ramp+duration — the backstop math is too tight")
	}
}

// loadAwareFakeRunner records whether the deadline it was given comfortably covers `sleep` (proving
// jmeterProcessBackstop folded in ramp+duration) and simulates a healthy run that legitimately takes
// `sleep` wall time to finish.
type loadAwareFakeRunner struct {
	sleep               time.Duration
	code                string
	deadlineWasGenerous bool
}

func (r *loadAwareFakeRunner) Run(templateBase, jtlPath string, props map[string]string, timeout time.Duration) error {
	if timeout > r.sleep {
		r.deadlineWasGenerous = true
	}
	time.Sleep(r.sleep)
	id := props["scenario.id"]
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,42," + id + "," + r.code + ",OK," + templateBase + " " + id + " 1-1,true,\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

// TestJmeterProcessBackstop_Formula pins jmeterProcessBackstop's arithmetic directly (no subprocess,
// no RunAll) — EVIDENCE ITEM 2's justification for the chosen multiple/grace, and a mutation target:
// change the formula's `+`/`*` and this test must go RED.
func TestJmeterProcessBackstop_Formula(t *testing.T) {
	statusOnly := scenarioWithTimeout(t, "10s", "")
	if got, want := jmeterProcessBackstop(statusOnly, "http-ingestion"), 10*time.Second+jmeterProcessGrace; got != want {
		t.Fatalf("http-ingestion (1 sampler): backstop=%s want %s", got, want)
	}
	content := scenarioWithTimeout(t, "10s", "")
	if got, want := jmeterProcessBackstop(content, "database-state"), 10*time.Second*3+jmeterProcessGrace; got != want {
		t.Fatalf("database-state (3 samplers): backstop=%s want %s", got, want)
	}
	loadBlock := "- **Users**: 5\n- **Ramp Seconds**: 2\n- **Duration Seconds**: 3\n" +
		"- **Target P95 Ms**: 5000\n- **Max Error Rate**: 1.0"
	loaded := scenarioWithTimeout(t, "1s", loadBlock)
	want := 1*time.Second + 2*time.Second + 3*time.Second + jmeterProcessGrace
	if got := jmeterProcessBackstop(loaded, "http-ingestion"); got != want {
		t.Fatalf("load scenario: backstop=%s want %s (TIMEOUT + ramp + duration + grace)", got, want)
	}
	if jmeterProcessGrace < 60*time.Second {
		t.Fatalf("jmeterProcessGrace=%s, want >= 60s (the design note's own floor)", jmeterProcessGrace)
	}
}

// scenarioWithTimeout builds a minimal, valid *scenario.Scenario with an EXPLICIT `## TIMEOUT` and
// an optional `## LOAD` block — minimalLoadScenario (loadmode_template_test.go) hardcodes 30s, so
// this test (which needs its own values, including a sub-ceiling 1s) builds the text directly.
func scenarioWithTimeout(t *testing.T, timeout, loadBlock string) *scenario.Scenario {
	t.Helper()
	lines := []string{
		"# Scenario: t", "", "## Metadata",
		"- **ID**: T-BACKSTOP", "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT", "### Runnable", "- status=202", "",
	}
	if loadBlock != "" {
		lines = append(lines, "## LOAD", loadBlock, "")
	}
	lines = append(lines, "## TIMEOUT", timeout, "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return scenario.Parse(strings.Join(lines, "\n"))
}

// TestDeriveProps_EmitsTriggerTimeout confirms DeriveProps wires the declared `## TIMEOUT` into
// BOTH units every JMX template consumes: trigger.timeout_ms (HTTPSampler.connect_timeout/
// response_timeout) and trigger.timeout_s (JDBCSampler.queryTimeout, JDBC's own unit — matching
// cleanup-sql.jmx's pre-existing cleanup.timeout.s convention), rounded UP so a sub-second TIMEOUT
// never becomes queryTimeout "0" (JDBC's "no timeout").
func TestDeriveProps_EmitsTriggerTimeout(t *testing.T) {
	c := httpConfig()
	for _, tc := range []struct {
		timeout, wantMs, wantS string
	}{
		{"15s", "15000", "15"},
		{"800ms", "800", "1"}, // ceil(0.8s) = 1s, never "0"
		{"1s", "1000", "1"},
	} {
		s := scenarioWithTimeout(t, tc.timeout, "")
		props, err := DeriveProps(c, s, "tr-t")
		if err != nil {
			t.Fatalf("%s: DeriveProps: %v", tc.timeout, err)
		}
		if props["trigger.timeout_ms"] != tc.wantMs {
			t.Errorf("TIMEOUT %s: trigger.timeout_ms = %q, want %q", tc.timeout, props["trigger.timeout_ms"], tc.wantMs)
		}
		if props["trigger.timeout_s"] != tc.wantS {
			t.Errorf("TIMEOUT %s: trigger.timeout_s = %q, want %q", tc.timeout, props["trigger.timeout_s"], tc.wantS)
		}
	}
}

// TestTemplatesReadTheTimeoutPropertiesDeriveEmits is EVIDENCE ITEM 1's JMX-side proof (the
// Go↔JMX contract, same shape as TestTemplatesReadTheLoadPropertiesDeriveEmits): every
// HTTPSamplerProxy in every JMX template that fires one must read trigger.timeout_ms for BOTH
// connect_timeout and response_timeout, and database-state.jmx's JDBCSampler must read
// trigger.timeout_s for queryTimeout — an empty default (`${__P(trigger.timeout_ms,)}`) so a
// scenario that (somehow) reaches a run with no TIMEOUT prop is byte-identical to JMeter's own
// unbounded default, never a hard-coded number this file would have to keep in sync by hand.
func TestTemplatesReadTheTimeoutPropertiesDeriveEmits(t *testing.T) {
	httpSamplerCounts := map[string]int{
		"http-ingestion.jmx":    1,
		"database-state.jmx":    2,
		"external-delivery.jmx": 2,
		"http-idempotency.jmx":  2,
		"saga-presence.jmx":     1,
		"message-flow.jmx":      8,
	}
	for name, n := range httpSamplerCounts {
		t.Run(name, func(t *testing.T) {
			jmx := readTemplate(t, name)
			gotConnect := strings.Count(jmx, `<stringProp name="HTTPSampler.connect_timeout">${__P(trigger.timeout_ms,)}</stringProp>`)
			gotResponse := strings.Count(jmx, `<stringProp name="HTTPSampler.response_timeout">${__P(trigger.timeout_ms,)}</stringProp>`)
			if gotConnect != n || gotResponse != n {
				t.Errorf("%s: connect_timeout wired %d/%d, response_timeout wired %d/%d samplers", name, gotConnect, n, gotResponse, n)
			}
		})
	}
	jmx := readTemplate(t, "database-state.jmx")
	if want := `<stringProp name="queryTimeout">${__P(trigger.timeout_s,)}</stringProp>`; !strings.Contains(jmx, want) {
		t.Errorf("database-state.jmx's JDBCSampler does not read trigger.timeout_s for queryTimeout")
	}
}
