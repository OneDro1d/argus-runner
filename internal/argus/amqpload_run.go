package argus

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// amqpload_run.go -- the `AMQP Load` run loop (, design §2.D.4) and S9's door in the run
// path.
//
// One JMeter run PER STEP (templates/amqp-load.jmx): the sampler's per-session connections live until the
// whole test ends (JavaSampler's teardownTest runs at test end only), so a clean per-step teardown needs
// a process per step. The Runner interface is unchanged; the JTL of step k is read by internal/amqpload.

// amqpLoadSleep is the settle pause between steps; a test swaps it for a recorder (an injected clock).
var amqpLoadSleep = time.Sleep

// amqpLoadEnvCapture reads the namespace it is handed once (best effort, bounded). nil = nothing could be
// read, which makes a step's restarts_delta "not measured", never 0. A test swaps it.
var amqpLoadEnvCapture = defaultAMQPLoadEnvCapture

func defaultAMQPLoadEnvCapture(ns string) *envcapture.Capture {
	if ns == "" {
		return nil
	}
	cl, err := envcapture.NewInClusterClient()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), envCaptureTimeout)
	defer cancel()
	c := envcapture.CaptureNamespace(ctx, cl, ns)
	return &c
}

const (
	// amqpLoadStepMargin is added to a step's own duration for the process backstop (the ramp is INSIDE
	// the step duration): the backstop catches a hung JVM, it never bounds a healthy step.
	amqpLoadStepMargin = 30 * time.Second
	// amqpLoadQueueGrace is added to duration+settle for the per-session queues' x-expires: a killed run
	// is reaped by the broker itself.
	amqpLoadQueueGraceSeconds = 120
	amqpLoadDefaultExchange   = "amq.direct"
	// amqpLoadDefaultQueuePrefix starts every session queue's name unless the target's `queues.load` names
	// another: a load login is usually granted ONE name pattern (RabbitMQ permissions are regexes over names).
	amqpLoadDefaultQueuePrefix = "argus-load"
	// maxSetupDetail bounds the sampler text quoted on a setup failure.
	maxSetupDetail = 300
)

// amqpLoadRefusal is S9's door in the run path: nil = allowed.
//
// ⛔ IT IS THE GUARANTEE, and runOneScenario is the single choke point every runner path passes. It
// returns `error` and not `failed` (it says nothing about the SUT, the contract MoneyGuardRefusal has),
// before DeriveProps, before JMeter, before any environment read: NOTHING dials the broker. The text
// never echoes a URL, host or credential. Mutation W-S9 deletes the CALL in runOneScenario and
// TestAMQPLoad_NotAllowedIsRefusedBeforeFiring goes red.
func amqpLoadRefusal(c *config.Config, s *scenario.Scenario, corr string) *report.ScenarioResult {
	if s == nil || PrimaryLayer(s) != scenario.AMQPLoadLayer {
		return nil
	}
	why := c.LoadRefusal(s)
	if why == "" && runIDFromCorr(corr) == "" {
		// every queue and routing key of the run is named from the run id. Without one
		// they are all `argus-load--s1-N`, shared with every other run that ever lacked one.
		why = "refused before firing: this run has no run id (correlation id " + strconv.Quote(corr) +
			"), and the AMQP Load queue names are derived from it. Nothing was sent — preflight."
	}
	if why == "" {
		return nil
	}
	return &report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
		Failure: &report.Failure{Observed: why}}
}

// runIDFromCorr extracts <run_id> from tr-<run_id>-<scenario_id>-<8hex> (the run id has no '-').
// "" = the correlation id has no run id in that position: it used to hand back the
// whole remainder for `tr--AL-001-x`, a non-empty nonsense value that named every queue the same way.
func runIDFromCorr(corr string) string {
	if !strings.HasPrefix(corr, "tr-") {
		return ""
	}
	rest := strings.TrimPrefix(corr, "tr-")
	if i := strings.Index(rest, "-"); i > 0 {
		return rest[:i]
	}
	return ""
}

// javaSamplerURI is the connection URI handed to the Java sampler (`mq.amqp.uri`): the configured URL with
// its userinfo removed (the credential travels as its own secret props) and its vhost normalised so that the
// Java client reads the SAME vhost amqp091-go reads from the configured URL.
//
// They disagree on exactly one form: `amqp://host:5672/`. amqp091-go keeps its default vhost "/" for it; the
// Java client's setUri takes the path after the first "/" literally, which is the EMPTY vhost, which no broker
// has (`530 NOT_ALLOWED - vhost  not found`). So the vhost is read with amqp091-go (the reader the rest of
// Argus connects with) and rendered unambiguously: the default vhost as NO path, a named one as a single
// percent-encoded segment. A URL amqp091-go cannot parse is passed through userinfo-stripped, so the
// sampler reports its own connect error rather than this layer inventing one.
func javaSamplerURI(u *url.URL, raw string) string {
	stripped := *u
	stripped.User = nil
	parsed, err := amqp.ParseURI(raw)
	if err != nil {
		return stripped.String()
	}
	if parsed.Vhost == "/" {
		stripped.Path, stripped.RawPath = "", ""
	} else {
		stripped.Path = "/" + parsed.Vhost
		stripped.RawPath = "/" + url.PathEscape(parsed.Vhost)
	}
	return stripped.String()
}

// amqpLoadBaseProps are the props every step shares. The broker credential is split out of the target
// URL exactly as DeriveProps does for the AC-D16 refusal sampler: mq.amqp.password is a SECRET prop, so
// it travels in the private 0600 properties file and never as `-J`.
func amqpLoadBaseProps(s *scenario.Scenario, p *scenario.AMQPLoadProfile, mq *config.MQTarget, corr string) map[string]string {
	props := map[string]string{
		"scenario.id":     s.ID,
		"correlation.id":  corr,
		"load.scheduler":  "true",
		"load.duration":   strconv.Itoa(p.StepDurationSeconds),
		"load.ramp":       strconv.Itoa(p.RampSeconds),
		"amqp.run":        runIDFromCorr(corr),
		"amqp.rate":       strconv.FormatFloat(p.RatePerSession, 'f', -1, 64),
		"amqp.size":       strconv.Itoa(p.MessageSize),
		"amqp.queue_type": p.QueueType,
		"amqp.confirm":    p.Confirm,
		"amqp.ack":        p.Ack,
		"amqp.prefetch":   strconv.Itoa(p.Prefetch),
		"amqp.exchange":   amqpLoadDefaultExchange,
		// templates/amqp-load.jmx names each session queue <prefix>-<run>-s<step>-<thread>
		"amqp.queue_prefix": amqpLoadDefaultQueuePrefix,
		// ⛔ the JTL columns are PINNED, not left to JMeter 5.6.3's defaults: readJTL reads by name.
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
	if mq == nil {
		return props
	}
	if e := strings.TrimSpace(mq.Exchanges["load"]); e != "" {
		props["amqp.exchange"] = e
	}
	if q := strings.TrimSpace(mq.Queues["load"]); q != "" {
		props["amqp.queue_prefix"] = q
	}
	if u, err := url.Parse(mq.URL); err == nil {
		props["mq.amqp.uri"] = javaSamplerURI(u, mq.URL)
		if u.User != nil {
			props["mq.amqp.username"] = u.User.Username()
			if pw, set := u.User.Password(); set {
				props["mq.amqp.password"] = pw
			}
		}
	}
	return props
}

// amqpLoadSecrets are the values that must never reach a report, a log or an error text: every secret
// prop of the run, and user:password (whose Base64 is an HTTP basic-auth blob).
func amqpLoadSecrets(props map[string]string) []string {
	var out []string
	for k, v := range props {
		if isSecretProp(k) && v != "" {
			out = append(out, v)
		}
	}
	if pw := props["mq.amqp.password"]; pw != "" {
		out = append(out, props["mq.amqp.username"]+":"+pw)
	}
	return out
}

// restartsAndNotReady is the step's restarts_delta and not_ready, from the namespace captured before and
// after it. nil/nil = not measured (no SUT read Role, or either read failed): never 0.
//
// ⚠ KNOWN LIMIT: a container's restartCount is per POD. A pod that was DELETED and recreated during the
// step comes back at 0 and shows no delta here (the replaced pod is visible through not_ready and the
// environment fingerprint instead); an in-place container restart (OOMKill, crash) is what it counts.
func restartsAndNotReady(before, after *envcapture.Capture) (*int, *int) {
	if before == nil || after == nil || !before.Captured || !after.Captured {
		return nil, nil
	}
	was := map[string]int{}
	for _, p := range before.Pods {
		for _, ct := range p.Containers {
			was[p.Name+"/"+ct.Name] = ct.RestartCount
		}
	}
	delta := 0
	for _, p := range after.Pods {
		for _, ct := range p.Containers {
			if b, ok := was[p.Name+"/"+ct.Name]; ok {
				if ct.RestartCount > b {
					delta += ct.RestartCount - b
				}
			} else {
				delta += ct.RestartCount
			}
		}
	}
	notReady := 0
	for _, w := range after.Workloads {
		if w.ReplicasReady < w.ReplicasDesired {
			notReady++
		}
	}
	return &delta, &notReady
}

// runAMQPLoad executes the ramp of one `AMQP Load` scenario (already past S9's door) and returns its row.
func runAMQPLoad(c *config.Config, s *scenario.Scenario, corr, resultsDir string, r Runner, mode string) report.ScenarioResult {
	res := report.ScenarioResult{ID: s.ID, CorrelationID: corr, LoadDriver: "amqp", LoadTarget: s.Target}
	p := s.AMQPLoad
	if p == nil { // the profile did not parse: the validator names why; nothing was measured
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: "the ## LOAD profile of this AMQP Load scenario is not valid, so no step was run — preflight"}
		return res
	}
	sel, serr := c.SelectTarget(s)
	if serr != nil || sel == nil || sel.MQ == nil {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: "this AMQP Load scenario names no usable message_broker target — preflight"}
		return res
	}
	base := amqpLoadBaseProps(s, p, sel.MQ, corr)
	secrets := amqpLoadSecrets(base)
	scrub := func(t string) string { return amqpengine.RedactSecrets(t, secrets...) }

	// ARGUS-TA-5: the per-step environment read goes to the namespace of this check's test
	// target, else ARGUS_SUT_NAMESPACE: the same rule as the run-level capture, with the one check as the input.
	// One check can never be "several", so the reason is empty here.
	envNS, _ := chooseCaptureNamespace(c.TestTargets, []loadCheckRef{{ID: s.ID, Tags: s.Tags}}, os.Getenv(sutNamespaceEnvVar))

	started := time.Now()
	steps := make([]report.LoadStep, len(p.Steps))
	for i, n := range p.Steps {
		steps[i] = report.LoadStep{Step: i + 1, Sessions: n, Status: report.LoadStepNotRun}
	}
	var setupDetail, runFailure, backstopNote string
	var reqOK, reqFailed int
	for i, n := range p.Steps {
		if i > 0 {
			amqpLoadSleep(time.Duration(p.SettleSeconds) * time.Second)
		}
		props := make(map[string]string, len(base)+6)
		for k, v := range base {
			props[k] = v
		}
		props["load.users"] = strconv.Itoa(n)
		props["amqp.step"] = strconv.Itoa(i + 1)
		props["amqp.expires_ms"] = strconv.Itoa((p.StepDurationSeconds + p.SettleSeconds + amqpLoadQueueGraceSeconds) * 1000)

		jtl := filepath.Join(resultsDir, fmt.Sprintf("amqp-load__%s__step%d.jtl", s.ID, i+1))
		_ = os.Remove(jtl) // JMeter -l APPENDS; start each step fresh
		_ = os.Remove(jtl + ".log")
		backstop := time.Duration(p.StepDurationSeconds)*time.Second + amqpLoadStepMargin + jmeterProcessGrace

		before := amqpLoadEnvCapture(envNS)
		runErr := r.Run("amqp-load", jtl, props, backstop)
		after := amqpLoadEnvCapture(envNS)

		rows, _ := amqpload.ReadJTL(jtl)
		st := amqpload.AggregateStep(rows, amqpload.StepInput{
			Step: i + 1, Sessions: n, RampSeconds: p.RampSeconds, RatePerSession: p.RatePerSession,
			Confirm: p.Confirm, ScenarioID: s.ID,
			TargetP95Ms: p.TargetP95Ms, MaxErrorRate: p.MaxErrorRate, MinDeliveredRatio: p.MinDeliveredRatio,
		})
		st.RestartsDelta, st.NotReady = restartsAndNotReady(before, after)
		steps[i] = st
		ok, failed := amqpload.CountRequests(rows, s.ID)
		reqOK, reqFailed = reqOK+ok, reqFailed+failed
		// a certification scenario id (and its per-step outcome) must not reach the executor
		// log, which reaches Loki, which a builder can query. The control plane already
		// refuses AMQP Load on those modes; this is the executor not depending on that.
		if report.ModeIsCertifying(mode) {
			log.Printf("amqp-load step %d/%d (%d sessions)", i+1, len(p.Steps), n)
		} else {
			log.Printf("amqp-load %s step %d/%d (%d sessions): %s", s.ID, i+1, len(p.Steps), n, st.Status)
		}

		if runErr != nil {
			// a broker that blocks holds its publishers, and with them the JVM, past the backstop.
			// A step that MEASURED a block is the SUT's answer; the kill that followed is reported after it, never
			// instead of it. Without a measured block it stays the rig's error (VR-7: a rig failure never passes
			// as a SUT result).
			if st.Status == report.LoadStepBlocked {
				backstopNote = "the step's JMeter process then outlived the step and was stopped by the executor (" + scrub(runErr.Error()) + ")"
				break
			}
			// the rig, not the broker: JMeter did not finish (a hung JVM past the backstop, a launch failure)
			runFailure = "jmeter run error: " + scrub(runErr.Error())
			break
		}
		if st.Status == report.LoadStepBlocked || st.Status == report.LoadStepSetupFailed {
			if st.Status == report.LoadStepSetupFailed {
				if d := amqpload.FirstSetupFailure(rows, s.ID); d != "" {
					setupDetail = scrub(d)
					if len(setupDetail) > maxSetupDetail {
						setupDetail = setupDetail[:maxSetupDetail] + "…"
					}
				}
			}
			break // bounded harm: a blocked broker is not driven any harder
		}
	}

	res.DurationMs = int(time.Since(started).Milliseconds())
	res.LoadSteps = steps
	res.ReqSuccess, res.ReqFailed = reqOK, reqFailed
	// the two vocabulary bullets are enforced by the verdict rule every time
	res.AssertionsEnforced = append([]string(nil), s.RunnableExpect()...)
	res.AssertionsEnforcedCount = len(res.AssertionsEnforced)

	if runFailure != "" {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: runFailure}
		return res
	}
	out := amqpload.Verdict(p, steps)
	res.Status = out.Status
	observed := scrub(out.Observed)
	if setupDetail != "" {
		observed += ": " + setupDetail
	}
	if backstopNote != "" {
		observed += "; " + backstopNote
	}
	switch out.Status {
	case "failed":
		exp := strings.Join(s.Expect, "; ")
		res.Failure = &report.Failure{Observed: observed, Expected: &exp}
	case report.StatusDegraded:
		res.Failure = &report.Failure{Observed: observed}
	}
	return res
}
