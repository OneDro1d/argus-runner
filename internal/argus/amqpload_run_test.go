package argus

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// S7 + S8 + S9 ( / -27). No JMeter and no broker is involved anywhere here: the Runner is
// a fake that writes JTL fixtures, and the "dial" a refusal must prevent is a call to it.

// loadSecret is the broker password of the fixture config. It is built from parts so no literal of it
// sits in this file next to the word it belongs to.
var loadSecret = "Zq7" + "-ld-" + "k9xW4"

const loadCfgTmpl = `project:
  name: lab
targets:
  http:
    base_url: http://sut.invalid
  message_broker_targets:
    load-lab:
      type: amqp
      url: amqp://loaduser:%s@load-lab.invalid:5672/
      management_url: http://load-lab.invalid:15672
      exchanges:
        load: argus.load
`

func loadConfig(t *testing.T, allow string) string {
	t.Helper()
	return fmt.Sprintf(loadCfgTmpl, loadSecret) + allow
}

func amqpLoadMD(id, steps, extraLoad string) string {
	return "# Scenario: " + id + "\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: AMQP Load\n- **Tags**: http, load\n- **Target**: load-lab\n\n" +
		"## TRIGGER\nPOST `/amqp-load`\n\n## EXPECT\n### Runnable\n- broker is not blocked\n- every step is measured\n\n" +
		"## LOAD\n- **Steps**: " + steps + "\n- **Step Duration Seconds**: 20\n- **Ramp Seconds**: 10\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n" + extraLoad +
		"\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — the sampler deletes its queues.\n"
}

func writeAMQPLoad(t *testing.T, dir, id, steps, extraLoad string) {
	t.Helper()
	d := filepath.Join(dir, "amqp-load")
	_ = os.MkdirAll(d, 0o755)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(amqpLoadMD(id, steps, extraLoad)), 0o644); err != nil {
		t.Fatal(err)
	}
}

type loadCall struct {
	base, jtl string
	props     map[string]string
	timeout   time.Duration
}

// loadFake is the counting Runner: every Run is a "dial" (it is where JMeter would start). gen builds
// the JTL of call n (0-based).
type loadFake struct {
	calls []loadCall
	gen   func(n int, props map[string]string) string
}

func (f *loadFake) Run(base, jtl string, props map[string]string, timeout time.Duration) error {
	cp := map[string]string{}
	for k, v := range props {
		cp[k] = v
	}
	f.calls = append(f.calls, loadCall{base, jtl, cp, timeout})
	return os.WriteFile(jtl, []byte(f.gen(len(f.calls)-1, props)), 0o600)
}

const jtlHead = "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"

// healthyJTL: `sessions` sessions, one publish + one deliver per second, over a 20 s step whose first 10 s are ramp.
func healthyJTL(id string, props map[string]string) string {
	sessions, _ := strconv.Atoi(props["load.users"])
	var b strings.Builder
	b.WriteString(jtlHead)
	t0 := int64(1_000_000)
	fmt.Fprintf(&b, "%d,5,%s-amqp-setup,200,session ready,T 1-1,true,\n", t0, id)
	for sec := 0; sec <= 20; sec++ {
		for s := 0; s < sessions; s++ {
			ts := t0 + int64(sec)*1000
			fmt.Fprintf(&b, "%d,2,%s-amqp-publish,200,published 1000B confirm=each us=2100,T %d-1,true,\n", ts, id, s)
			fmt.Fprintf(&b, "%d,4,%s-amqp-deliver,200,delivered 1000B us=4200 redelivered=false,T %d-1,true,\n", ts, id, s)
		}
	}
	return b.String()
}

func blockedJTL(id string) string {
	t0 := int64(1_000_000)
	var b strings.Builder
	b.WriteString(jtlHead)
	fmt.Fprintf(&b, "%d,5,%s-amqp-setup,200,session ready,T 1-1,true,\n", t0, id)
	for sec := 11; sec <= 20; sec++ {
		fmt.Fprintf(&b, "%d,0,%s-amqp-publish,503,blocked by broker: memory alarm (since=%d),T 1-1,false,\n", t0+int64(sec)*1000, id, t0+11000)
	}
	return b.String()
}

type loadEnv struct {
	dir, results string
	sleeps       []time.Duration
	envCalls     int
}

func newLoadEnv(t *testing.T) *loadEnv {
	t.Helper()
	e := &loadEnv{dir: t.TempDir()}
	e.results = filepath.Join(e.dir, "results")
	oldSleep, oldEnv := amqpLoadSleep, amqpLoadEnvCapture
	amqpLoadSleep = func(d time.Duration) { e.sleeps = append(e.sleeps, d) }
	amqpLoadEnvCapture = func(string) *envcapture.Capture { e.envCalls++; return nil }
	t.Cleanup(func() { amqpLoadSleep, amqpLoadEnvCapture = oldSleep, oldEnv })
	return e
}

func (e *loadEnv) run(t *testing.T, cfg string, f Runner) report.ScenarioResult {
	t.Helper()
	c := loadCfg(t, cfg)
	rr, err := RunAll(c, filepath.Join(e.dir, "scenarios"), e.results, "lab", "", "", "", "20261002T120000000", f)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range rr.Report.Layers {
		for _, s := range l.Scenarios {
			if strings.HasPrefix(s.ID, "AL-") {
				return s
			}
		}
	}
	t.Fatalf("no AMQP Load row in the report: %+v", rr.Report)
	return report.ScenarioResult{}
}

const allowLab = "load_allowed_targets:\n  load-lab:\n    max_sessions: 1000\n"

// ── S9 ─────────────────────────────────────────────────────────────────────────────────────────

// THE NAMED TEST of the wiring mutation: deleting the amqpLoadRefusal CALL in runOneScenario must turn
// this red, because without it the scenario fires.
func TestAMQPLoad_NotAllowedIsRefusedBeforeFiring(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, ""), f)

	if len(f.calls) != 0 {
		t.Fatalf("the runner (the dial) was called %d time(s) for a target that does not allow load", len(f.calls))
	}
	if files, _ := filepath.Glob(filepath.Join(e.results, "*.jtl")); len(files) != 0 {
		t.Errorf("JTL files exist for a refused scenario: %v", files)
	}
	if e.envCalls != 0 || len(e.sleeps) != 0 {
		t.Errorf("a refused scenario touched the environment (%d) or slept (%d)", e.envCalls, len(e.sleeps))
	}
	if res.Status != report.StatusError {
		t.Errorf("status = %q, want %q (nothing was measured; it says nothing about the SUT)", res.Status, report.StatusError)
	}
	obs := ""
	if res.Failure != nil {
		obs = res.Failure.Observed
	}
	t.Logf("REFUSAL TEXT: %s", obs)
	for _, want := range []string{"refused before firing", `message_broker target "load-lab"`, "load_allowed_targets", "Nothing was sent", "preflight"} {
		if !strings.Contains(obs, want) {
			t.Errorf("refusal text lacks %q: %s", want, obs)
		}
	}
	for _, leak := range []string{"load-lab.invalid", "amqp://", loadSecret, "loaduser"} {
		if strings.Contains(obs, leak) {
			t.Errorf("refusal leaks %q", leak)
		}
	}
	if len(res.LoadSteps) != 0 || res.LoadDriver != "" {
		t.Errorf("a refused scenario carries load results: %+v", res)
	}
}

func TestAMQPLoad_CeilingRefusedBeforeFiring(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 500", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, "load_allowed_targets:\n  load-lab:\n    max_sessions: 100\n"), f)
	if len(f.calls) != 0 || res.Status != report.StatusError || !strings.Contains(res.Failure.Observed, "max_sessions is 100") {
		t.Fatalf("calls=%d status=%s failure=%+v", len(f.calls), res.Status, res.Failure)
	}
}

// ── the run loop ───────────────────────────────────────────────────────────────────────────────

func TestAMQPLoad_AllowedRunsOneJMeterRunPerStep(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, allowLab), f)

	if len(f.calls) != 2 {
		t.Fatalf("runner calls = %d, want one per step (2)", len(f.calls))
	}
	for i, want := range []string{"2", "4"} {
		c := f.calls[i]
		if c.base != "amqp-load" || c.props["load.users"] != want || c.props["amqp.step"] != strconv.Itoa(i+1) {
			t.Errorf("call %d: base=%s users=%s step=%s", i, c.base, c.props["load.users"], c.props["amqp.step"])
		}
		if !strings.HasSuffix(c.jtl, fmt.Sprintf("amqp-load__AL-001__step%d.jtl", i+1)) {
			t.Errorf("call %d jtl = %s", i, c.jtl)
		}
	}
	if len(e.sleeps) != 1 || e.sleeps[0] != 30*time.Second {
		t.Errorf("settle sleeps = %v, want exactly one 30s pause between the two steps", e.sleeps)
	}
	if res.Status != "passed" {
		t.Fatalf("status = %s (%+v)", res.Status, res.Failure)
	}
	if res.LoadDriver != "amqp" || res.LoadTarget != "load-lab" || len(res.LoadSteps) != 2 {
		t.Fatalf("driver=%q target=%q steps=%d", res.LoadDriver, res.LoadTarget, len(res.LoadSteps))
	}
	if s := res.LoadSteps[1]; s.Step != 2 || s.Sessions != 4 || s.Status != "measured" || !s.Comfortable {
		t.Errorf("step 2 = %+v", s)
	}
	if res.Requests != nil || res.Load != nil {
		t.Error("an AMQP Load row must carry no per-message Requests (Loki stream cap) and no legacy LoadStats")
	}
	if res.AssertionsEnforcedCount != 2 {
		t.Errorf("assertions_enforced_count = %d, want the 2 load-vocabulary bullets", res.AssertionsEnforcedCount)
	}
	if u := UnexecutedRunnable(parseFile(t, e.dir, "AL-001")); len(u) != 0 {
		t.Errorf("the load vocabulary must not be reported as unexecuted: %+v", u)
	}
}

func TestAMQPLoad_PropsCarryTheProfile_AndTheCredentialOnlyAsASecretProp(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "- **Rate Per Session**: 5\n- **Message Size**: 2000\n- **Queue Type**: quorum\n- **Confirm**: batch:10\n- **Ack**: auto\n- **Prefetch**: 7\n")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	e.run(t, loadConfig(t, allowLab), f)
	if len(f.calls) == 0 {
		t.Fatal("the runner was never called")
	}
	p := f.calls[0].props
	for k, want := range map[string]string{
		"load.users": "2", "load.ramp": "10", "load.scheduler": "true", "load.duration": "20",
		"amqp.rate": "5", "amqp.size": "2000", "amqp.queue_type": "quorum", "amqp.confirm": "batch:10",
		"amqp.ack": "auto", "amqp.prefetch": "7", "amqp.exchange": "argus.load", "amqp.run": "20261002T120000000",
		"amqp.expires_ms": "170000", "scenario.id": "AL-001", "mq.amqp.username": "loaduser",
		"mq.amqp.uri":                                               "amqp://load-lab.invalid:5672", // the default vhost as NO path: a bare "/" is the empty vhost to the Java client (F-47)
		"jmeter.save.saveservice.output_format":                     "csv",
		"jmeter.save.saveservice.response_message":                  "true",
		"jmeter.save.saveservice.timestamp_format":                  "ms",
		"jmeter.save.saveservice.print_field_names":                 "true",
		"jmeter.save.saveservice.successful":                        "true",
		"jmeter.save.saveservice.assertion_results_failure_message": "true",
	} {
		if p[k] != want {
			t.Errorf("props[%s] = %q, want %q", k, p[k], want)
		}
	}
	if p["mq.amqp.password"] != loadSecret || !isSecretProp("mq.amqp.password") {
		t.Error("the broker password must travel as mq.amqp.password, a SECRET prop (the private 0600 file, never -J)")
	}
	pub, _ := splitSecretProps(p)
	for k, v := range pub {
		if strings.Contains(v, loadSecret) {
			t.Errorf("public (-J) prop %s carries the password", k)
		}
	}
	if f.calls[0].timeout != 20*time.Second+30*time.Second+jmeterProcessGrace {
		t.Errorf("backstop = %v, want step duration + 30s margin + grace", f.calls[0].timeout)
	}
}

func TestAMQPLoad_RampStopsAtABlockedStep_AndFailsNamingTheBroker(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4, 8", "")
	f := &loadFake{gen: func(n int, p map[string]string) string {
		if n == 1 {
			return blockedJTL("AL-001")
		}
		return healthyJTL("AL-001", p)
	}}
	res := e.run(t, loadConfig(t, allowLab), f)
	if len(f.calls) != 2 {
		t.Fatalf("the ramp went on after a blocked step: %d runs", len(f.calls))
	}
	if res.Status != "failed" || !strings.Contains(res.Failure.Observed, "blocked by broker") || !strings.Contains(res.Failure.Observed, "memory alarm") {
		t.Fatalf("status=%s failure=%+v", res.Status, res.Failure)
	}
	if len(res.LoadSteps) != 3 {
		t.Fatalf("load steps = %d, want 3", len(res.LoadSteps))
	}
	got := []string{res.LoadSteps[0].Status, res.LoadSteps[1].Status, res.LoadSteps[2].Status}
	if got[0] != "measured" || got[1] != "blocked" || got[2] != "not_run" {
		t.Errorf("step statuses = %v", got)
	}
	if len(e.sleeps) != 1 {
		t.Errorf("sleeps = %v: settle only between steps that both ran", e.sleeps)
	}
}

func TestAMQPLoad_RestartsDeltaComesFromTwoCapturesAroundTheStep(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	n := 0
	amqpLoadEnvCapture = func(string) *envcapture.Capture {
		n++
		restarts := 0
		if n == 2 {
			restarts = 3
		}
		return &envcapture.Capture{Captured: true,
			Pods:      []envcapture.Pod{{Name: "rabbit-0", Containers: []envcapture.Container{{Name: "rabbitmq", RestartCount: restarts}}}},
			Workloads: []envcapture.Workload{{Kind: "StatefulSet", Name: "rabbit", ReplicasDesired: 3, ReplicasReady: 2}}}
	}
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, allowLab), f)
	if len(res.LoadSteps) == 0 {
		t.Fatal("no load steps in the row")
	}
	s := res.LoadSteps[0]
	if s.RestartsDelta == nil || *s.RestartsDelta != 3 || s.NotReady == nil || *s.NotReady != 1 {
		t.Fatalf("restarts_delta/not_ready = %v/%v", s.RestartsDelta, s.NotReady)
	}
	if res.Status != report.StatusDegraded {
		t.Errorf("status = %s, want degraded (every assertion held, the broker restarted)", res.Status)
	}
}

func TestAMQPLoad_NoEnvironmentRead_MeansNotMeasured_NeverZero(t *testing.T) {
	e := newLoadEnv(t) // the default capture returns nil: no SUT read Role
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, allowLab), f)
	if len(res.LoadSteps) == 0 {
		t.Fatal("no load steps in the row")
	}
	if res.LoadSteps[0].RestartsDelta != nil || res.LoadSteps[0].NotReady != nil {
		t.Errorf("absent evidence reported as a number: %+v", res.LoadSteps[0])
	}
	b, _ := json.Marshal(res.LoadSteps[0])
	if strings.Contains(string(b), "restarts_delta") {
		t.Errorf("restarts_delta must be absent, not 0: %s", b)
	}
}

func TestAMQPLoad_EnvironmentCaptureCoversTheNewLayer(t *testing.T) {
	if !anyLoadDeclared([]scenarioFile{{s: scenarioFromMD(t, amqpLoadMD("AL-009", "2", ""))}}) {
		t.Error("a run holding only an AMQP Load scenario must count as a load run for the environment capture")
	}
}

// ── S10 ─────────────────────────────────────────────────────────────────────────────────────────

// The password reaches the JTL (the sampler's own error text can carry the connect URI) and must reach
// NEITHER the report, nor the log. The control asserts the secret IS in the input first.
func TestAMQPLoad_BrokerPasswordNeverReachesTheReportOrTheLog(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	var logBuf bytes.Buffer
	oldW := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldW)

	enc := url.QueryEscape(loadSecret)
	b64 := base64.StdEncoding.EncodeToString([]byte("loaduser:" + loadSecret))
	f := &loadFake{gen: func(n int, p map[string]string) string {
		t0 := int64(1_000_000)
		return jtlHead +
			fmt.Sprintf("%d,5,AL-001-amqp-setup,500,\"connect failed amqp://loaduser:%s@load-lab.invalid:5672/ pw=%s enc=%s b64=%s\",T 1-1,false,\n",
				t0, loadSecret, loadSecret, enc, b64)
	}}
	res := e.run(t, loadConfig(t, allowLab), f)

	if len(f.calls) == 0 {
		t.Fatal("the runner was never called")
	}
	jtl, err := os.ReadFile(f.calls[0].jtl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jtl), loadSecret) { // POSITIVE CONTROL: the secret is in the input
		t.Fatal("control failed: the fixture JTL does not contain the password, so this test proves nothing")
	}
	out, _ := json.Marshal(res)
	for name, hay := range map[string]string{"report": string(out), "log": logBuf.String()} {
		for what, needle := range map[string]string{"password": loadSecret, "url-encoded": enc, "base64 user:pw": b64} {
			if strings.Contains(hay, needle) {
				t.Errorf("%s contains the %s form of the broker password", name, what)
			}
		}
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "[REDACTED]") {
		t.Errorf("the setup failure should reach Observed, scrubbed: %+v", res.Failure)
	}
}

// ── the template ───────────────────────────────────────────────────────────────────────────────

func TestAMQPLoadTemplate_ReadsTheLoadPropertiesAndLoopsUntilTheSchedulerStopsIt(t *testing.T) {
	jmx := readTemplate(t, "amqp-load.jmx")
	for _, want := range []string{
		`${__P(load.users,1)}`, `${__P(load.ramp,0)}`, `${__P(load.scheduler,false)}`, `${__P(load.duration,0)}`,
		`<stringProp name="LoopController.loops">-1</stringProp>`,
		`com.onedroid.jmeter.amqp.AmqpSessionSetupSampler`, `com.onedroid.jmeter.amqp.AmqpPublishSampler`, `com.onedroid.jmeter.amqp.AmqpSubscribeSampler`,
		`${__P(scenario.id,unknown)}-amqp-setup`, `${__P(scenario.id,unknown)}-amqp-publish`, `${__P(scenario.id,unknown)}-amqp-deliver`,
		`${__P(mq.amqp.password,)}`, `${__P(mq.amqp.username,)}`, `${__P(mq.amqp.uri,)}`,
		`${__P(amqp.rate,1)}`, `${__P(amqp.size,1000)}`, `${__P(amqp.confirm,each)}`, `${__P(amqp.queue_type,classic)}`,
		`OnceOnlyController`, `ConstantThroughputTimer`, `${__P(amqp.run,)}`,
	} {
		if !strings.Contains(jmx, want) {
			t.Errorf("amqp-load.jmx lacks %q", want)
		}
	}
	if strings.Contains(jmx, loadSecret) {
		t.Error("a credential is baked into the template")
	}
	// The sampler's argument names are its contract: a name it does not read is silently ignored (the value
	// falls back to the sampler's default), so a typo here is a wrong measurement, not an error. AmqpPublishSampler
	// reads `message_size_bytes` (upstream name, kept for existing plans) — never `message_size`.
	if !strings.Contains(jmx, `<stringProp name="Argument.name">message_size_bytes</stringProp>`) {
		t.Error("amqp-load.jmx does not pass message_size_bytes to AmqpPublishSampler")
	}
	if strings.Contains(jmx, `<stringProp name="Argument.name">message_size</stringProp>`) {
		t.Error("amqp-load.jmx passes message_size, which AmqpPublishSampler does not read (it reads message_size_bytes)")
	}
	if _, ok := TemplateReads["amqp-load"]; !ok {
		t.Error("TemplateReads does not know amqp-load")
	}
	if got := templateSamplerCount["amqp-load"]; got < 1 {
		t.Errorf("templateSamplerCount[amqp-load] = %d", got)
	}
}

func scenarioFromMD(t *testing.T, md string) *scenario.Scenario {
	t.Helper()
	return scenario.Parse(md)
}

func parseFile(t *testing.T, dir, id string) *scenario.Scenario {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "scenarios", "amqp-load", id+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return scenario.Parse(string(b))
}
