// Package amqpload turns the JTL of one `AMQP Load` step into the per-step record (report.LoadStep) and
// the ramp's verdict. It is pure: it reads files and does arithmetic, it never dials a broker and it
// never starts JMeter (the run loop in internal/argus does, behind the Runner interface).
//
// The sampler contract it reads (design §2.B/§2.C, frozen): three labels `<id>-amqp-setup|publish|deliver`,
// the `us=<microseconds>` token in responseMessage, `503 blocked by broker: <reason> (since=<epoch ms>)`
// while the broker holds the publisher blocked, and ` (was blocked <ms>ms)` on the first success after it.
package amqpload

import (
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Row is one JTL row, read by header NAME.
type Row struct {
	TimeStampMs    int64
	ElapsedMs      int
	Label          string
	Code           string
	Message        string
	FailureMessage string
	Success        bool
}

// StepInput is everything AggregateStep needs besides the rows. The thresholds feed Comfortable and are
// NOT copied into the record.
type StepInput struct {
	Step, Sessions, RampSeconds int
	RatePerSession              float64
	Confirm                     string
	ScenarioID                  string
	TargetP95Ms                 int
	MaxErrorRate                float64
	MinDeliveredRatio           float64
}

// Outcome is the scenario-level verdict of a ramp.
type Outcome struct {
	Status   string // passed | failed | degraded
	Observed string // reality only (VR-C8): never a declared threshold
}

// ReadJTL parses a JTL CSV by header name (never by position: JMeter's column order is configuration).
func ReadJTL(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 2 {
		return nil, nil
	}
	head := map[string]int{}
	for i, h := range recs[0] {
		head[h] = i
	}
	get := func(rec []string, k string) string {
		if i, ok := head[k]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	var rows []Row
	for _, rec := range recs[1:] {
		ts, _ := strconv.ParseInt(get(rec, "timeStamp"), 10, 64)
		el, _ := strconv.Atoi(get(rec, "elapsed"))
		rows = append(rows, Row{
			TimeStampMs: ts, ElapsedMs: el, Label: get(rec, "label"), Code: get(rec, "responseCode"),
			Message: get(rec, "responseMessage"), FailureMessage: get(rec, "failureMessage"),
			Success: strings.EqualFold(get(rec, "success"), "true"),
		})
	}
	return rows, nil
}

var (
	usRe       = regexp.MustCompile(`\bus=(\d+)`)
	sinceRe    = regexp.MustCompile(`\(since=(\d+)\)`)
	reasonRe   = regexp.MustCompile(`blocked by broker: (.*?)(?: \(since=|$)`)
	wasBlocked = "was blocked"
)

// latencyUs is the figure of one successful row: the sampler's `us=` token, else elapsed*1000 (the JTL
// elapsed is whole milliseconds, too coarse for a sub-10ms p99 -- which is why the token exists, D-5).
func latencyUs(r Row) int64 {
	if m := usRe.FindStringSubmatch(r.Message); m != nil {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return v
		}
	}
	return int64(r.ElapsedMs) * 1000
}

// errorClass is the CLOSED class set of the record (bounded Pushgateway label cardinality, design §4.2).
func errorClass(kind string, r Row) string {
	if kind == "setup" {
		return "setup"
	}
	switch r.Code {
	case "503":
		return "blocked"
	case "504":
		return "confirm_timeout"
	case "502":
		return "nack"
	case "408":
		return "deliver_timeout"
	case "404", "403", "405", "406":
		return "channel:" + r.Code
	}
	return "other"
}

// nearestRank is the repo's own percentile (report.nearestRankPercentile): rank = ceil(p/100 x N) over
// the ascending-sorted values, no interpolation, so every reported value was observed.
func nearestRank(sorted []int64, p int) int64 {
	idx := (p*len(sorted)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func quantiles(vals []int64) *report.Quantiles {
	if len(vals) == 0 {
		return nil
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return &report.Quantiles{Min: s[0], P50: nearestRank(s, 50), P75: nearestRank(s, 75),
		P95: nearestRank(s, 95), P99: nearestRank(s, 99), Max: s[len(s)-1]}
}

// QuantilesOf is the nearest-rank quantiles of vals (microseconds), nil for none: the ONE percentile rule of a
// ramp step, shared with the HTTP ramp (internal/httpload) so both drivers report the same way.
func QuantilesOf(vals []int64) *report.Quantiles { return quantiles(vals) }

// AggregateStep computes one step's record from its rows.
func AggregateStep(rows []Row, in StepInput) report.LoadStep {
	st := report.LoadStep{Step: in.Step, Sessions: in.Sessions, Status: report.LoadStepMeasured}
	prefix := in.ScenarioID + "-amqp-"
	type krow struct {
		kind string
		Row
	}
	var rs []krow
	for _, r := range rows {
		if k, ok := strings.CutPrefix(r.Label, prefix); ok && (k == "setup" || k == "publish" || k == "deliver") {
			rs = append(rs, krow{k, r})
		}
	}
	if len(rs) == 0 {
		st.Status = report.LoadStepSetupFailed // nothing the sampler wrote: the step never got a session
		return st
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].TimeStampMs < rs[j].TimeStampMs })
	t0, maxTs := rs[0].TimeStampMs, rs[len(rs)-1].TimeStampMs
	winStart := t0 + int64(in.RampSeconds)*1000
	if win := float64(maxTs-winStart) / 1000; win > 0 {
		st.WindowSeconds = win
	}

	// blocked periods: over EVERY row (a block during the ramp still happened)
	var cur *report.BlockedPeriod
	for _, r := range rs {
		switch {
		case r.Code == "503" && strings.Contains(r.Message, "blocked by broker"):
			if cur == nil {
				since := r.TimeStampMs
				if m := sinceRe.FindStringSubmatch(r.Message); m != nil {
					if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
						since = v
					}
				}
				reason := ""
				if m := reasonRe.FindStringSubmatch(r.Message); m != nil {
					reason = strings.TrimSpace(m[1])
				}
				cur = &report.BlockedPeriod{SinceMs: since, Reason: reason}
			}
		case r.Success && cur != nil && strings.Contains(r.Message, wasBlocked):
			cur.UntilMs = r.TimeStampMs
			st.Blocked = append(st.Blocked, *cur)
			st.BlockedSeconds += float64(cur.UntilMs-cur.SinceMs) / 1000
			cur = nil
		}
	}
	if cur != nil {
		st.Blocked = append(st.Blocked, *cur)
		st.BlockedSeconds += float64(maxTs-cur.SinceMs) / 1000
	}

	var offered, sent, confirmed, delivered, failedPD, totalPD, setupFailed int
	var confirmUs, deliverUs []int64
	errs := map[string]int{}
	for _, r := range rs {
		if r.kind == "setup" {
			if !r.Success {
				setupFailed++
				errs["setup"]++
			}
			continue
		}
		if r.TimeStampMs < winStart {
			continue // the ramp is excluded from every statistic
		}
		totalPD++
		if r.kind == "publish" {
			offered++
		}
		if !r.Success {
			failedPD++
			errs[errorClass(r.kind, r.Row)]++
			continue
		}
		if r.kind == "publish" {
			sent++
			if in.Confirm == "each" {
				confirmed++
				confirmUs = append(confirmUs, latencyUs(r.Row))
			}
		} else {
			delivered++
			deliverUs = append(deliverUs, latencyUs(r.Row))
		}
	}
	if len(errs) > 0 {
		st.Errors = errs
	}
	if in.Confirm == "each" {
		st.PublishConfirmUs = quantiles(confirmUs)
	}
	st.PublishDeliverUs = quantiles(deliverUs)
	if w := st.WindowSeconds; w > 0 {
		st.OfferedPerS = float64(offered) / w
		st.SentPerS = float64(sent) / w
		st.ConfirmedPerS = float64(confirmed) / w
		st.DeliveredPerS = float64(delivered) / w
		// a fixed-rate generator that falls behind under-reports latency (coordinated omission): this
		// flag is the guard, and a flagged step is never comfortable.
		if expected := float64(in.Sessions) * in.RatePerSession; st.OfferedPerS < 0.9*expected && len(st.Blocked) == 0 {
			// a step the BROKER blocked is not flagged: a block stalls the publishers, so
			// the shortfall is the broker's, and the step is already not comfortable by its blocked status.
			// (Condition above: the generator offered under 90% of its rate with no block to explain it.)
			st.GeneratorLimited = true
		}
	}
	if sent > 0 {
		st.DeliveredRatio = float64(delivered) / float64(sent)
	}

	switch {
	case len(st.Blocked) > 0:
		st.Status = report.LoadStepBlocked
	case setupFailed > 0:
		st.Status = report.LoadStepSetupFailed
	}

	errRate := 0.0
	if totalPD > 0 {
		errRate = float64(failedPD) / float64(totalPD)
	}
	st.Comfortable = st.Status == report.LoadStepMeasured && !st.GeneratorLimited &&
		st.PublishDeliverUs != nil && st.PublishDeliverUs.P95 <= int64(in.TargetP95Ms)*1000 &&
		errRate <= in.MaxErrorRate && st.DeliveredRatio >= in.MinDeliveredRatio
	return st
}

// CountRequests counts the publish and deliver rows of a step by outcome, for the row's req_success /
// req_failed (the dashboard's request counters). Setup rows are not requests.
func CountRequests(rows []Row, scenarioID string) (ok, failed int) {
	for _, r := range rows {
		if r.Label != scenarioID+"-amqp-publish" && r.Label != scenarioID+"-amqp-deliver" {
			continue
		}
		if r.Success {
			ok++
		} else {
			failed++
		}
	}
	return ok, failed
}

// FirstSetupFailure returns the first failed setup row's text, for the Observed line (the caller scrubs
// it: it is sampler text and can carry a connect error).
func FirstSetupFailure(rows []Row, scenarioID string) string {
	for _, r := range rows {
		if r.Label == scenarioID+"-amqp-setup" && !r.Success {
			return strings.TrimSpace(r.Code + " " + r.Message + " " + r.FailureMessage)
		}
	}
	return ""
}

// Verdict applies the D.5 rule to the whole ramp (D-3, an architect default kept in ONE function so a
// product answer flips one branch):
//
//	failed    a step recorded a blocked period or a session-setup failure (the ramp already stopped there),
//	          or a declared `Must Sustain` step was not comfortable, or no step was comfortable
//	degraded  every assertion held, but the broker restarted during a step
//	passed    otherwise -- a ramp that crosses the limit at its top step is a MEASURED limit, not a failure
func Verdict(p *scenario.AMQPLoadProfile, steps []report.LoadStep) Outcome {
	total := len(steps)
	for _, s := range steps {
		switch s.Status {
		case report.LoadStepBlocked:
			reason := ""
			if len(s.Blocked) > 0 && s.Blocked[0].Reason != "" {
				reason = ": " + s.Blocked[0].Reason
			}
			return Outcome{"failed", fmt.Sprintf("blocked by broker%s — the broker held the publisher blocked at step %d of %d (%d sessions) and the ramp stopped there",
				reason, s.Step, total, s.Sessions)}
		case report.LoadStepSetupFailed:
			return Outcome{"failed", fmt.Sprintf("a session could not be set up at step %d of %d (%d sessions) and the ramp stopped there", s.Step, total, s.Sessions)}
		}
	}
	any := false
	for _, s := range steps {
		any = any || s.Comfortable
	}
	if p != nil && p.MustSustain > 0 {
		for _, s := range steps {
			if s.Sessions == p.MustSustain && !s.Comfortable {
				return Outcome{"failed", fmt.Sprintf("the %d-session step was not comfortable (%s)", s.Sessions, describe(s))}
			}
		}
	}
	if !any {
		first := report.LoadStep{}
		if total > 0 {
			first = steps[0]
		}
		return Outcome{"failed", fmt.Sprintf("no step was comfortable: even the smallest (%d sessions) was not (%s)", first.Sessions, describe(first))}
	}
	for _, s := range steps {
		if s.RestartsDelta != nil && *s.RestartsDelta > 0 {
			return Outcome{"degraded", fmt.Sprintf("the broker restarted %d time(s) during step %d of %d (%d sessions) while every assertion held", *s.RestartsDelta, s.Step, total, s.Sessions)}
		}
	}
	return Outcome{Status: "passed"}
}

// describe is the measured reality of a step, never a threshold.
func describe(s report.LoadStep) string {
	p95 := "no delivery measured"
	if s.PublishDeliverUs != nil {
		p95 = fmt.Sprintf("publish to deliver p95 %.1f ms", float64(s.PublishDeliverUs.P95)/1000)
	}
	extra := ""
	if s.GeneratorLimited {
		extra = ", the load generator itself was the limit"
	}
	return fmt.Sprintf("%s, delivered/sent %.3f%s", p95, s.DeliveredRatio, extra)
}
