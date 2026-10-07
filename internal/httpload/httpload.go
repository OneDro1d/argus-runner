// Package httpload turns the JTL of one `HTTP Load` step into the per-step record (report.LoadStep) and the
// ramp's verdict. It is the HTTP twin of internal/amqpload and, like it, pure: it reads rows and
// does arithmetic. It never sends a request and never starts JMeter (internal/argus does, behind the Runner).
//
// The sampler contract it reads is the http-ingestion template's: ONE sampler labelled exactly with the
// scenario id, its numeric responseCode (a transport failure has "Non HTTP response code: <class>" there), its
// responseMessage, timeStamp (the request's START, epoch ms) and elapsed (ms).
package httpload

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// StepInput is everything AggregateStep needs besides the rows. The thresholds feed Comfortable and are NOT
// copied into the record (they are holdout material, exactly as on an AMQP step).
type StepInput struct {
	Step, Users, RampSeconds int
	ScenarioID               string
	TargetP95Ms              int
	MaxErrorRate             float64
	// Scrub removes declared credentials from a transport-failure reason before it is cut (may be nil).
	Scrub func(string) string
	// GeneratorThrottled is the share of the executor's own CFS periods that were throttled during the step
	// (ThrottledShare); nil = not measured. Above GeneratorThrottleBound the step is
	// generator_limited and never comfortable.
	GeneratorThrottled *float64
}

// applyGenerator records the executor-throttling reading on a step and, past the bound, flags the generator.
func applyGenerator(st *report.LoadStep, share *float64) {
	if share == nil {
		st.GeneratorNotMeasured = true
		return
	}
	v := *share
	st.GeneratorCPUThrottledShare = &v
	if st.Status == report.LoadStepMeasured && v > GeneratorThrottleBound {
		st.GeneratorLimited = true
		st.Comfortable = false
	}
}

// Outcome is the scenario-level verdict of a ramp.
type Outcome struct {
	Status   string // passed | failed | degraded
	Observed string // reality only (VR-C8): never a declared threshold
}

// answered: the request got an HTTP status. ok: that status is below 400 (a request counts as served).
func answered(r amqpload.Row) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.Code))
	return n, err == nil
}

func requestOK(r amqpload.Row) bool {
	n, ok := answered(r)
	return ok && n >= 100 && n < 400
}

// errorClass is the closed class of a failed request (bounded Pushgateway label cardinality): `status:<code>`
// for an HTTP answer of 400 or more (at most 500 values), `timeout` for a request the declared ## TIMEOUT cut,
// `transport` for any other request that got no HTTP status (refused, reset, unknown host, TLS).
func errorClass(r amqpload.Row) string {
	if n, ok := answered(r); ok {
		if n >= 400 && n <= 999 {
			return "status:" + strconv.Itoa(n)
		}
		return "transport"
	}
	t := strings.ToLower(r.Code + " " + r.Message)
	if strings.Contains(t, "timed out") || strings.Contains(t, "timeout") {
		return "timeout"
	}
	return "transport"
}

// stepRows are the rows of the scenario's own sampler, oldest first.
func stepRows(rows []amqpload.Row, scenarioID string) []amqpload.Row {
	var out []amqpload.Row
	for _, r := range rows {
		if r.Label == scenarioID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeStampMs < out[j].TimeStampMs })
	return out
}

// AggregateStep computes one step's record from its rows.
//
//	offered_per_s    requests STARTED in the steady window / window
//	sent_per_s       of those, requests that got an HTTP status
//	delivered_per_s  of those, requests answered below 400 (served)
//	delivered_ratio  served / started
//	response_us      request -> response time of every request of the window (failures included: a step that
//	                 answers fast with errors is caught by its error rate, a slow one by its p95)
//	errors           failed requests by class; error_reasons: why the transport failures failed
func AggregateStep(rows []amqpload.Row, in StepInput) report.LoadStep {
	st := report.LoadStep{Step: in.Step, Sessions: in.Users, Status: report.LoadStepMeasured}
	rs := stepRows(rows, in.ScenarioID)
	if len(rs) == 0 {
		// JMeter recorded no request at all: nothing was measured (the HTTP twin of a session that could not
		// be set up). The rig, a launch failure or a target that took every connection forever.
		st.Status = report.LoadStepSetupFailed
		applyGenerator(&st, in.GeneratorThrottled)
		return st
	}
	t0, maxTs := rs[0].TimeStampMs, rs[len(rs)-1].TimeStampMs
	winStart := t0 + int64(in.RampSeconds)*1000
	if win := float64(maxTs-winStart) / 1000; win > 0 {
		st.WindowSeconds = win
	}
	var offered, got, served int
	var lat []int64
	var samples []report.LoadSample
	errs := map[string]int{}
	for _, r := range rs {
		if r.TimeStampMs < winStart {
			continue // the ramp is excluded from every statistic
		}
		offered++
		lat = append(lat, int64(r.ElapsedMs)*1000)
		samples = append(samples, report.LoadSample{StartMs: r.TimeStampMs, ElapsedMs: r.ElapsedMs, Code: r.Code, Message: r.Message})
		if _, ok := answered(r); ok {
			got++
		}
		if requestOK(r) {
			served++
			continue
		}
		errs[errorClass(r)]++
	}
	if len(errs) > 0 {
		st.Errors = errs
	}
	st.ErrorReasons = report.LoadErrorsFrom(samples, in.Scrub)
	st.ResponseUs = amqpload.QuantilesOf(lat)
	if w := st.WindowSeconds; w > 0 {
		st.OfferedPerS = float64(offered) / w
		st.SentPerS = float64(got) / w
		st.DeliveredPerS = float64(served) / w
	}
	errRate := 0.0
	if offered > 0 {
		st.DeliveredRatio = float64(served) / float64(offered)
		errRate = float64(offered-served) / float64(offered)
	}
	st.Comfortable = st.Status == report.LoadStepMeasured && st.ResponseUs != nil &&
		st.ResponseUs.P95 <= int64(in.TargetP95Ms)*1000 && errRate <= in.MaxErrorRate
	applyGenerator(&st, in.GeneratorThrottled)
	return st
}

// CountRequests counts the step's requests by outcome (served below 400, or not), for the row's req_success /
// req_failed counters.
func CountRequests(rows []amqpload.Row, scenarioID string) (ok, failed int) {
	for _, r := range stepRows(rows, scenarioID) {
		if requestOK(r) {
			ok++
		} else {
			failed++
		}
	}
	return ok, failed
}

// Verdict applies the ramp rule, the AMQP rule (amqpload.Verdict) with the HTTP vocabulary:
//
//	failed    a step recorded no request (`every step is measured`; the ramp stopped there), or no step was
//	          comfortable (`the smallest step is comfortable`: the ramp stops at the first step that is not, so
//	          that is its first step), or a declared `Must Sustain` step was not comfortable or never reached
//	degraded  every assertion held, but the target restarted during a step
//	passed    otherwise -- a ramp that breaks at a step above its first is a MEASURED limit, not a failure
func Verdict(p *scenario.HTTPLoadProfile, steps []report.LoadStep, stoppedAt int) Outcome {
	total := len(steps)
	for _, s := range steps {
		if s.Status == report.LoadStepSetupFailed {
			return Outcome{"failed", fmt.Sprintf("no request was recorded at step %d of %d (%d users), so that step could not be measured and the ramp stopped there", s.Step, total, s.Sessions)}
		}
	}
	any := false
	for _, s := range steps {
		any = any || s.Comfortable
	}
	if p != nil && p.MustSustain > 0 {
		for _, s := range steps {
			if s.Sessions != p.MustSustain {
				continue
			}
			if s.Status == report.LoadStepNotRun {
				broke := ""
				if stoppedAt > 0 && stoppedAt <= total {
					b := steps[stoppedAt-1]
					broke = fmt.Sprintf(": the ramp stopped at step %d (%d users), which was not comfortable (%s)", b.Step, b.Sessions, Describe(b))
				}
				return Outcome{"failed", fmt.Sprintf("the %d-user step was never reached%s", s.Sessions, broke)}
			}
			if !s.Comfortable {
				return Outcome{"failed", fmt.Sprintf("the %d-user step was not comfortable (%s)", s.Sessions, Describe(s))}
			}
		}
	}
	if !any {
		first := report.LoadStep{}
		if total > 0 {
			first = steps[0]
		}
		return Outcome{"failed", fmt.Sprintf("no step was comfortable: even the smallest (%d users) was not (%s), and the ramp stopped there", first.Sessions, Describe(first))}
	}
	// #615: a step the generator limited measured the executor, not the target. Every assertion held, but the
	// ramp did not reach the target's limit, so it is not a plain pass.
	for _, s := range steps {
		if s.GeneratorLimited {
			return Outcome{"degraded", fmt.Sprintf("the load generator, not the target, was the limit at step %d of %d (%d users): %s; the target's own limit was not reached", s.Step, total, s.Sessions, generatorWords(s))}
		}
	}
	for _, s := range steps {
		if s.RestartsDelta != nil && *s.RestartsDelta > 0 {
			return Outcome{"degraded", fmt.Sprintf("the target restarted %d time(s) during step %d of %d (%d users) while every assertion held", *s.RestartsDelta, s.Step, total, s.Sessions)}
		}
	}
	return Outcome{Status: "passed"}
}

// generatorWords is the measured throttling of a generator-limited step (never a threshold).
func generatorWords(s report.LoadStep) string {
	if s.GeneratorCPUThrottledShare == nil {
		return "executor CPU throttling not measured"
	}
	return fmt.Sprintf("the executor's CPU was throttled in %.0f%% of periods", *s.GeneratorCPUThrottledShare*100)
}

// Describe is the measured reality of a step, never a threshold.
func Describe(s report.LoadStep) string {
	p95 := "no response measured"
	if s.ResponseUs != nil {
		p95 = fmt.Sprintf("response p95 %.1f ms", float64(s.ResponseUs.P95)/1000)
	}
	failed := 0
	classes := make([]string, 0, len(s.Errors))
	for c, n := range s.Errors {
		failed += n
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		if s.Errors[classes[i]] != s.Errors[classes[j]] {
			return s.Errors[classes[i]] > s.Errors[classes[j]]
		}
		return classes[i] < classes[j]
	})
	out := fmt.Sprintf("%s, served/started %.3f", p95, s.DeliveredRatio)
	if s.GeneratorLimited {
		out += "; the load generator was the limit (" + generatorWords(s) + ")"
	}
	if failed > 0 {
		out += fmt.Sprintf(", %d request(s) failed, most often %s (%d)", failed, classes[0], s.Errors[classes[0]])
	}
	if len(s.ErrorReasons) > 0 {
		out += fmt.Sprintf("; transport: %s (%d)", s.ErrorReasons[0].Reason, s.ErrorReasons[0].Count)
	}
	return out
}
