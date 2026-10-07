package obsquery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// PushMetrics pushes the run's argus_* metrics to a Prometheus Pushgateway
// (DF-DEC-006: compose has no continuous-mode scrape, so a one-shot run pushes).
// The per-scenario metrics are grouped under job=argus / instance=<id> / run_id=<run>, so runs
// ACCUMULATE (R3-2/R3-3, below) — each run_id gets its own series, none overwritten. The single
// argus_last_run_timestamp marker is pushed separately, grouped by instance only, so IT is what
// REPLACES each run (the spec-18a freshness panel renders the latest run for the instance).
// Best-effort.
func PushMetrics(pushgatewayURL, instance, project, cluster string, rep *report.Report, retention time.Duration) error {
	if pushgatewayURL == "" {
		return nil
	}
	lbl := fmt.Sprintf(`argus_instance=%q,project=%q,cluster=%q`, instance, project, cluster)
	var b strings.Builder
	runByLayer := map[string]float64{}

	// Text exposition format: each metric family declares # TYPE before its samples,
	// and all samples of a family are grouped together (pushgateway rejects otherwise).
	b.WriteString("# TYPE argus_scenarios_total counter\n")
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			fmt.Fprintf(&b, "argus_scenarios_total{status=%q,layer=%q,scenario_id=%q,%s} 1\n", s.Status, l.Layer, s.ID, lbl)
			runByLayer[l.Layer] += float64(s.DurationMs) / 1000.0
		}
	}
	b.WriteString("# TYPE argus_scenario_duration_seconds gauge\n")
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			// status label so the "Slowest scenarios" panel can honor the dashboard's pass/fail/error
			// filter (bounded label — NFR-3 ok). One series per scenario (each has one status).
			fmt.Fprintf(&b, "argus_scenario_duration_seconds{status=%q,layer=%q,scenario_id=%q,%s} %g\n", s.Status, l.Layer, s.ID, lbl, float64(s.DurationMs)/1000.0)
		}
	}
	b.WriteString("# TYPE argus_run_duration_seconds gauge\n")
	for layer, dur := range runByLayer {
		fmt.Fprintf(&b, "argus_run_duration_seconds{layer=%q,%s} %g\n", layer, lbl, dur)
	}
	writeLoadStepFamilies(&b, rep, lbl)
	// argus_last_run_timestamp is the SINGLE-series "latest run" marker (grouped by instance only, so it
	// is REPLACED each run): the freshness panel (time() - argus_last_run_timestamp) and the "is the
	// selected run the latest?" gate rely on exactly one series. It is pushed to its OWN grouping key
	// (below), SEPARATE from the per-scenario metrics — which now accumulate per run_id.
	// (r3) argus_sut_requests_total is RETIRED: the "Test requests sent" panel moved to the per-request
	// Loki event stream (obsquery.PushRequestEvents) for the true within-run distribution.
	var mb strings.Builder
	mb.WriteString("# TYPE argus_last_run_timestamp gauge\n")
	fmt.Fprintf(&mb, "argus_last_run_timestamp{run_id=%q,%s} %d\n", rep.RunID, lbl, time.Now().Unix())

	base := strings.TrimRight(pushgatewayURL, "/") + "/metrics/job/argus/instance/" + instance
	// R3-2/R3-3 (FX-7, Option A): the per-scenario metrics go to a run_id-keyed grouping so runs
	// ACCUMULATE (no longer overwrite) and — via Prometheus honor_labels — every series carries run_id,
	// making the pass/fail, latency and per-layer panels addressable per run (run_id=~"$current_run").
	// Owner accepted the added cardinality (NFR-3) for the dev/eval rig.
	if rep.RunID == "" {
		// defensive (unreachable in prod — NewRunID always sets rep.RunID): with no run_id key, a
		// separate marker push would target the SAME instance-only key and OVERWRITE the scenario
		// metrics. Fold the marker into the single push instead.
		b.WriteString(mb.String())
		return putGroup(base, b.String())
	}
	if err := putGroup(base+"/run_id/"+rep.RunID, b.String()); err != nil {
		return err
	}
	if err := putGroup(base, mb.String()); err != nil {
		return err
	}
	pruneOldGroups(strings.TrimRight(pushgatewayURL, "/"), instance, rep.RunID, retention)
	return nil
}

// groupLabelRe is the only shape of instance / run_id value the prune step will put into a DELETE
// path. Anything else (a "/" or an encoded value) is skipped rather than guessed at.
var groupLabelRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// pruneOldGroups deletes THIS instance's finished per-run groups
// from the Pushgateway, so they stop growing without bound. It lists /api/v1/metrics and DELETEs
// every group with job=argus, instance=<instance> and a run_id that is not the current run and
// whose push_time_seconds is older than retention. The instance-only marker group (no run_id), other
// instances and other jobs are never touched. retention <= 0 keeps everything (and does not list).
// Best-effort: a failed list or delete logs one line and never fails the push.
func pruneOldGroups(gwURL, instance, currentRun string, retention time.Duration) {
	if retention <= 0 || !groupLabelRe.MatchString(instance) {
		return
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(gwURL + "/api/v1/metrics")
	if err != nil {
		log.Printf("pushgateway prune: list failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("pushgateway prune: list returned %d", resp.StatusCode)
		return
	}
	var doc struct {
		Data []struct {
			Labels   map[string]string `json:"labels"`
			PushTime struct {
				Metrics []struct {
					Value string `json:"value"`
				} `json:"metrics"`
			} `json:"push_time_seconds"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&doc); err != nil {
		log.Printf("pushgateway prune: list unreadable: %v", err)
		return
	}
	cutoff := time.Now().Add(-retention)
	deleted, failed := 0, 0
	for _, g := range doc.Data {
		run := g.Labels["run_id"]
		if g.Labels["job"] != "argus" || g.Labels["instance"] != instance || run == "" || run == currentRun {
			continue
		}
		if len(g.Labels) != 3 || !groupLabelRe.MatchString(run) || len(g.PushTime.Metrics) == 0 {
			continue // an unexpected grouping shape or no push time: not ours to delete
		}
		secs, err := strconv.ParseFloat(g.PushTime.Metrics[0].Value, 64)
		if err != nil || secs <= 0 {
			continue
		}
		if time.Unix(int64(secs), 0).After(cutoff) {
			continue
		}
		req, err := http.NewRequest(http.MethodDelete, gwURL+"/metrics/job/argus/instance/"+instance+"/run_id/"+run, nil)
		if err != nil {
			failed++
			continue
		}
		dr, err := client.Do(req)
		if err != nil {
			failed++
			continue
		}
		_ = dr.Body.Close()
		if dr.StatusCode >= 300 {
			failed++
			continue
		}
		deleted++
	}
	if failed > 0 {
		log.Printf("pushgateway prune: %d old group(s) of instance %s deleted, %d delete(s) failed", deleted, instance, failed)
	}
}

// writeLoadStepFamilies adds the argus_load_step_* families for an AMQP Load run (, design
// 4.3): one series per step, labelled like every other family here (argus_instance, project, cluster,
// scenario_id) plus `step`. Nothing but numbers and two closed label sets leaves: `quantile` (0.5, 0.75,
// 0.95, 0.99) and `code` (the closed error-class set). NO broker URL, host, user, password or target name
// is ever a label or a value (custody; a test pins it). A step that never ran (status not_run) has no
// series. Values that are absent in the record stay absent: confirm numbers only when the ramp measured
// publish->confirm (Confirm=each), restarts only when the SUT read could count them (never a made-up 0).
// Each family is written whole under its own # TYPE line (the grouping rule at the top of PushMetrics).
func writeLoadStepFamilies(b *strings.Builder, rep *report.Report, lbl string) {
	type row struct {
		id string
		st report.LoadStep
	}
	var rows []row
	for _, l := range rep.Layers {
		for _, sc := range l.Scenarios {
			for _, st := range sc.LoadSteps {
				if st.Status != report.LoadStepNotRun {
					rows = append(rows, row{sc.ID, st})
				}
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	family := func(name string, each func(r row, put func(extra string, v float64))) {
		var lines []string
		for _, r := range rows {
			each(r, func(extra string, v float64) {
				lines = append(lines, fmt.Sprintf("%s{scenario_id=%q,step=\"%d\"%s,%s} %g\n", name, r.id, r.st.Step, extra, lbl, v))
			})
		}
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(b, "# TYPE %s gauge\n", name)
		for _, ln := range lines {
			b.WriteString(ln)
		}
	}
	quantiles := func(q *report.Quantiles, put func(extra string, v float64)) {
		if q == nil {
			return
		}
		for _, p := range []struct {
			label string
			us    int64
		}{{"0.5", q.P50}, {"0.75", q.P75}, {"0.95", q.P95}, {"0.99", q.P99}} {
			put(fmt.Sprintf(",quantile=%q", p.label), float64(p.us)/1e6)
		}
	}
	family("argus_load_step_sessions", func(r row, put func(string, float64)) { put("", float64(r.st.Sessions)) })
	family("argus_load_step_sent_per_second", func(r row, put func(string, float64)) { put("", r.st.SentPerS) })
	family("argus_load_step_confirmed_per_second", func(r row, put func(string, float64)) {
		if r.st.PublishConfirmUs != nil {
			put("", r.st.ConfirmedPerS)
		}
	})
	family("argus_load_step_delivered_per_second", func(r row, put func(string, float64)) { put("", r.st.DeliveredPerS) })
	family("argus_load_step_publish_confirm_seconds", func(r row, put func(string, float64)) { quantiles(r.st.PublishConfirmUs, put) })
	family("argus_load_step_publish_deliver_seconds", func(r row, put func(string, float64)) { quantiles(r.st.PublishDeliverUs, put) })
	// an HTTP Load step's request -> response time; absent on every AMQP step (nil ResponseUs).
	family("argus_load_step_response_seconds", func(r row, put func(string, float64)) { quantiles(r.st.ResponseUs, put) })
	family("argus_load_step_errors", func(r row, put func(string, float64)) {
		codes := make([]string, 0, len(r.st.Errors))
		for c := range r.st.Errors {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		for _, c := range codes {
			put(fmt.Sprintf(",code=%q", c), float64(r.st.Errors[c]))
		}
	})
	family("argus_load_step_blocked_seconds", func(r row, put func(string, float64)) { put("", r.st.BlockedSeconds) })
	family("argus_load_step_comfortable", func(r row, put func(string, float64)) {
		v := 0.0
		if r.st.Comfortable {
			v = 1
		}
		put("", v)
	})
	family("argus_load_step_restarts_delta", func(r row, put func(string, float64)) {
		if r.st.RestartsDelta != nil {
			put("", float64(*r.st.RestartsDelta))
		}
	})
}

// putGroup PUTs one text-exposition body to a Pushgateway grouping-key URL (best-effort helper).
func putGroup(url, body string) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pushgateway returned %d", resp.StatusCode)
	}
	return nil
}
