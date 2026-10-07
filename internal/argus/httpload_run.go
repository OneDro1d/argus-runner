package argus

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/httpload"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// httpload_run.go -- the `HTTP Load` run loop and its door in the run path.
//
// The AMQP Load loop's shape (amqpload_run.go): ONE JMeter run PER STEP, the settle pause between steps, a
// per-step environment read, the per-step record (report.LoadStep) and the ramp verdict. The generator is the
// http-ingestion template the plain `## LOAD` check uses, with the request DeriveProps builds for the scenario's
// TRIGGER and named http target, every user a keep-alive client looping back to back.
//
// The one rule that differs: an HTTP ramp STOPS at the first step that is not comfortable. Nothing is sent after
// that step's hold, and the step is named (report.ScenarioResult.LoadStoppedAtStep -> the ramp entry's
// stopped_at_step).

// httpLoadSleep is the settle pause between steps; a test swaps it for a recorder.
var httpLoadSleep = time.Sleep

// httpLoadEnvCapture reads the namespace once around each step (best effort); nil = not measured. A test swaps it.
var httpLoadEnvCapture = defaultAMQPLoadEnvCapture

// httpLoadCPUStat reads the executor's own cgroup CPU counters (#615); a test swaps it. ok=false = not measured.
var httpLoadCPUStat = httpload.ReadCPUStat

// httpLoadStepMargin is added to a step's own duration for the process backstop, with twice the request
// TIMEOUT (a request still in flight when the scheduler ends the step may take its connect and its response
// timeout) and the JVM grace: the backstop catches a hung JVM, it never bounds a healthy step.
const httpLoadStepMargin = 30 * time.Second

// httpLoadCapRefusal is the RUN-TIME half of the layer's hard caps (the write-time half is
// scenario.buildHTTPLoad, whose profile is the only one Parse keeps). It re-checks the profile the run is about
// to drive, so a profile that reached this door any other way (built by hand, a future parser) is refused, not
// sent. "" = within the caps.
func httpLoadCapRefusal(p *scenario.HTTPLoadProfile) string {
	if p == nil {
		return ""
	}
	var why []string
	if n := len(p.Steps); n == 0 || n > scenario.HTTPLoadMaxSteps {
		why = append(why, fmt.Sprintf("it has %d steps (1 to %d are allowed)", n, scenario.HTTPLoadMaxSteps))
	}
	prev := 0
	for _, u := range p.Steps {
		if u < 1 || u > scenario.HTTPLoadMaxUsers {
			why = append(why, fmt.Sprintf("a step asks for %d users (1 to %d are allowed)", u, scenario.HTTPLoadMaxUsers))
			break
		}
		if u <= prev {
			why = append(why, fmt.Sprintf("its steps do not rise (%d after %d)", u, prev))
			break
		}
		prev = u
	}
	if d := p.StepDurationSeconds; d < scenario.HTTPLoadMinStepSeconds || d > scenario.HTTPLoadMaxStepSeconds {
		why = append(why, fmt.Sprintf("a step is held %d s (%d to %d s are allowed)", d, scenario.HTTPLoadMinStepSeconds, scenario.HTTPLoadMaxStepSeconds))
	}
	if p.RampSeconds < 0 || p.RampSeconds > p.StepDurationSeconds/2 {
		why = append(why, fmt.Sprintf("its ramp is %d s (0 to half the step are allowed)", p.RampSeconds))
	}
	if p.SettleSeconds < 0 || p.SettleSeconds > scenario.HTTPLoadMaxSettle {
		why = append(why, fmt.Sprintf("its settle pause is %d s (0 to %d s are allowed)", p.SettleSeconds, scenario.HTTPLoadMaxSettle))
	}
	if len(why) == 0 {
		return ""
	}
	return "refused before firing: this HTTP Load ramp is outside the layer's hard caps: " + strings.Join(why, "; ") + ". Nothing was sent. - preflight"
}

// httpLoadRefusal is the HTTP Load door in the run path, beside amqpLoadRefusal: nil = allowed. S9 unchanged
// (config.LoadRefusal: the target must be listed under load_allowed_targets and the top step within its
// max_sessions), then the hard caps. It is `error`, never `failed`, and comes before DeriveProps, JMeter and any
// environment read: NOTHING is sent to the target. Deleting its CALL in runOneScenario turns
// TestHTTPLoad_NotAllowedIsRefusedBeforeAnyRequest red.
func httpLoadRefusal(c *config.Config, s *scenario.Scenario, corr string) *report.ScenarioResult {
	if s == nil || PrimaryLayer(s) != scenario.HTTPLoadLayer {
		return nil
	}
	why := c.LoadRefusal(s)
	if why == "" {
		why = httpLoadCapRefusal(s.HTTPLoad)
	}
	if why == "" {
		return nil
	}
	return &report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
		Failure: &report.Failure{Observed: why}}
}

// httpLoadJTLProps pin the JTL columns readJTL reads by name, exactly as the AMQP Load run pins them.
var httpLoadJTLProps = map[string]string{
	"jmeter.save.saveservice.output_format":                     "csv",
	"jmeter.save.saveservice.print_field_names":                 "true",
	"jmeter.save.saveservice.timestamp_format":                  "ms",
	"jmeter.save.saveservice.timestamp":                         "true",
	"jmeter.save.saveservice.time":                              "true",
	"jmeter.save.saveservice.label":                             "true",
	"jmeter.save.saveservice.response_code":                     "true",
	"jmeter.save.saveservice.response_message":                  "true",
	"jmeter.save.saveservice.thread_name":                       "true",
	"jmeter.save.saveservice.successful":                        "true",
	"jmeter.save.saveservice.assertion_results_failure_message": "true",
	"jmeter.save.saveservice.latency":                           "true",
	"jmeter.save.saveservice.connect_time":                      "true",
}

// httpLoadStepProps is one step's property set: the scenario's request (DeriveProps) plus the step's thread group.
func httpLoadStepProps(base map[string]string, p *scenario.HTTPLoadProfile, users int) map[string]string {
	props := make(map[string]string, len(base)+len(httpLoadJTLProps)+8)
	for k, v := range base {
		props[k] = v
	}
	for k, v := range httpLoadJTLProps {
		props[k] = v
	}
	props["load.users"] = strconv.Itoa(users)
	props["load.ramp"] = strconv.Itoa(p.RampSeconds)
	props["load.scheduler"] = "true"
	props["load.duration"] = strconv.Itoa(p.StepDurationSeconds)
	props["load.loops"] = "-1"       // the scheduler ends the step, never a loop count
	props["http.keepalive"] = "true" // a user is a persistent client
	return props
}

// runHTTPLoad executes the ramp of one `HTTP Load` scenario (already past httpLoadRefusal) and returns its row.
func runHTTPLoad(c *config.Config, s *scenario.Scenario, corr, resultsDir string, r Runner, mode string) report.ScenarioResult {
	res := report.ScenarioResult{ID: s.ID, CorrelationID: corr, LoadDriver: "http", LoadTarget: s.Target}
	p := s.HTTPLoad
	if p == nil { // the profile did not parse: the validator names why; nothing was measured
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: "the ## LOAD profile of this HTTP Load scenario is not valid, so no step was run — preflight"}
		return res
	}
	if why := httpLoadCapRefusal(p); why != "" {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: why}
		return res
	}
	sel, serr := c.SelectTarget(s)
	if serr != nil || sel == nil || sel.HTTP == nil {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: "this HTTP Load scenario names no usable http target — preflight"}
		return res
	}
	base, err := DeriveProps(c, s, corr)
	if err != nil {
		return targetRefused(s, corr, err)
	}
	// What a transport failure's text may carry: every declared credential and every secret prop of the request
	// (an Authorization header value, an author header marked secret).
	var secrets []string
	for k, v := range base {
		if isSecretProp(k) && v != "" {
			secrets = append(secrets, v)
		}
	}
	cred := credentialScrub(c)
	scrub := func(t string) string { return cred(amqpengine.RedactSecrets(t, secrets...)) }

	envNS, _ := chooseCaptureNamespace(c.TestTargets, []loadCheckRef{{ID: s.ID, Tags: s.Tags}}, os.Getenv(sutNamespaceEnvVar))
	timeout := s.TimeoutDuration()

	started := time.Now()
	steps := make([]report.LoadStep, len(p.Steps))
	for i, n := range p.Steps {
		steps[i] = report.LoadStep{Step: i + 1, Sessions: n, Status: report.LoadStepNotRun}
	}
	var runFailure string
	var reqOK, reqFailed, stoppedAt int
	for i, n := range p.Steps {
		if i > 0 {
			httpLoadSleep(time.Duration(p.SettleSeconds) * time.Second)
		}
		props := httpLoadStepProps(base, p, n)
		jtl := filepath.Join(resultsDir, fmt.Sprintf("http-load__%s__step%d.jtl", s.ID, i+1))
		_ = os.Remove(jtl) // JMeter -l APPENDS; start each step fresh
		_ = os.Remove(jtl + ".log")
		backstop := time.Duration(p.StepDurationSeconds)*time.Second + 2*timeout + httpLoadStepMargin + jmeterProcessGrace

		before := httpLoadEnvCapture(envNS)
		cpuBefore, haveBefore := httpLoadCPUStat()
		runErr := r.Run("http-ingestion", jtl, props, backstop)
		cpuAfter, haveAfter := httpLoadCPUStat()
		after := httpLoadEnvCapture(envNS)
		var genShare *float64 // #615: nil = not measured; the step then says so
		if v, ok := httpload.ThrottledShare(cpuBefore, cpuAfter, haveBefore, haveAfter); ok {
			genShare = &v
		}

		rows, _ := amqpload.ReadJTL(jtl)
		st := httpload.AggregateStep(rows, httpload.StepInput{
			Step: i + 1, Users: n, RampSeconds: p.RampSeconds, ScenarioID: s.ID,
			TargetP95Ms: p.TargetP95Ms, MaxErrorRate: p.MaxErrorRate, Scrub: scrub,
			GeneratorThrottled: genShare,
		})
		st.RestartsDelta, st.NotReady = restartsAndNotReady(before, after)
		steps[i] = st
		ok, failed := httpload.CountRequests(rows, s.ID)
		reqOK, reqFailed = reqOK+ok, reqFailed+failed

		verdict := st.Status
		if st.Status == report.LoadStepMeasured && !st.Comfortable {
			verdict = "measured, not comfortable: the ramp stops here"
			if st.GeneratorLimited {
				verdict = "measured, generator limited (executor CPU throttled): the ramp stops here"
			}
		}
		// the same custody as the AMQP loop: a certifying mode logs no scenario id or outcome.
		if report.ModeIsCertifying(mode) {
			log.Printf("http-load step %d/%d (%d users)", i+1, len(p.Steps), n)
		} else {
			log.Printf("http-load %s step %d/%d (%d users): %s", s.ID, i+1, len(p.Steps), n, verdict)
		}

		if runErr != nil {
			// the rig, not the target: JMeter did not finish (a hung JVM past the backstop, a launch failure).
			// VR-7: a rig failure never passes as a SUT result.
			runFailure = "jmeter run error: " + scrub(runErr.Error())
			break
		}
		if st.Status != report.LoadStepMeasured {
			break // no request recorded: nothing to stand on, and nothing is driven harder
		}
		if !st.Comfortable {
			stoppedAt = i + 1 // the step that broke: no further step is sent
			break
		}
	}

	res.DurationMs = int(time.Since(started).Milliseconds())
	res.LoadSteps = steps
	res.LoadStoppedAtStep = stoppedAt
	res.ReqSuccess, res.ReqFailed = reqOK, reqFailed
	res.AssertionsEnforced = append([]string(nil), s.RunnableExpect()...)
	res.AssertionsEnforcedCount = len(res.AssertionsEnforced)

	if runFailure != "" {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: runFailure}
		return res
	}
	out := httpload.Verdict(p, steps, stoppedAt)
	res.Status = out.Status
	observed := scrub(out.Observed)
	switch out.Status {
	case "failed":
		exp := strings.Join(s.Expect, "; ")
		res.Failure = &report.Failure{Observed: observed, Expected: &exp}
	case report.StatusDegraded:
		res.Failure = &report.Failure{Observed: observed}
	}
	return res
}
