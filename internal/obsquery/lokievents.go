package obsquery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// lokiPushStream / lokiPushBody are the Loki push-API JSON shape: a set of streams, each a
// label set + ordered [ns-timestamp, line] value pairs.
type lokiPushStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}
type lokiPushBody struct {
	Streams []lokiPushStream `json:"streams"`
}

// requestEventStreamLabels are the FIXED + bounded labels of the runner-owned request-event
// stream. job/event_type are constant; outcome is bounded to 3 values; instance/project/cluster
// are bounded per deployment. run_id/scenario_id/correlation_id are NEVER labels (they ride the
// line body) — that keeps Loki stream cardinality flat regardless of run/scenario count (NFR-3).

// PushRequestEvents ships ONE Loki log line per request the runner fired (r3, 1a′), each stamped
// with the request's REAL fire-time, so Panel A can plot the genuine within-run distribution via
// count_over_time({job="argus-runner",event_type="request"}[$__interval]) split by `outcome`.
//
// Streams are keyed by outcome only (success|failed|error). Best-effort, mirroring PushMetrics:
// a missing url, a nil report, or a run that fired no request is a no-op; a Loki-down push
// returns an error but never blocks the run (the caller ignores it).
//
// tenant (T3.3) is the X-Scope-OrgID a shared Loki files the events under — the same tenant the
// executor reads with, or its own events would be invisible to it. "" sends no header (bundled,
// adopt, export: exactly the request they sent before T3.3).
//
// l carries the target: BaseURL, or PushURL verbatim when a hosted Loki declares
// one, plus the Credential and Tenant the query side already uses. A nil l is a no-op.
func PushRequestEvents(l *Loki, instance, project, cluster string, rep *report.Report) error {
	if l == nil || (l.BaseURL == "" && l.PushURL == "") || rep == nil {
		return nil
	}

	// One body line per fired request, grouped by outcome. scenario_id + correlation_id come
	// from the owning ScenarioResult; run_id is the run handle.
	type entry struct {
		atMs     int64
		scenario string
		corr     string
	}
	byOutcome := map[string][]entry{}
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			for _, smp := range s.Requests {
				byOutcome[smp.Outcome] = append(byOutcome[smp.Outcome], entry{atMs: smp.AtMs, scenario: s.ID, corr: s.CorrelationID})
			}
		}
	}
	if len(byOutcome) == 0 {
		return nil // the run fired no request (e.g. a dead rig) — nothing to plot
	}

	var streams []lokiPushStream
	// Deterministic outcome order keeps the push body stable (test-friendly).
	for _, outcome := range []string{report.OutcomeSuccess, report.OutcomeFailed, report.OutcomeError} {
		entries := byOutcome[outcome]
		if len(entries) == 0 {
			continue
		}
		// Loki requires per-stream entries in non-decreasing timestamp order. Sort ascending,
		// then force STRICTLY increasing nanoseconds (bump sub-ms collisions by +1ns) so the
		// push is accepted regardless of the bundled Loki's out-of-order/duplicate policy — a
		// nanosecond nudge is invisible on a ≥15s-bucket chart.
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].atMs < entries[j].atMs })
		values := make([][2]string, 0, len(entries))
		var lastNs int64 = -1
		for _, e := range entries {
			ns := e.atMs * 1_000_000
			if ns <= lastNs {
				ns = lastNs + 1
			}
			lastNs = ns
			line, _ := json.Marshal(map[string]string{
				"run_id":         rep.RunID,
				"scenario_id":    e.scenario,
				"correlation_id": e.corr,
				"outcome":        outcome,
			})
			values = append(values, [2]string{strconv.FormatInt(ns, 10), string(line)})
		}
		streams = append(streams, lokiPushStream{
			Stream: map[string]string{
				"job":            "argus-runner",
				"event_type":     "request",
				"outcome":        outcome,
				"argus_instance": instance,
				"project":        project,
				"cluster":        cluster,
			},
			Values: values,
		})
	}

	body, err := json.Marshal(lokiPushBody{Streams: streams})
	if err != nil {
		return err
	}
	// a hosted Loki (export) declares its own push endpoint, which need not share a
	// host with the query url the executor was given — push_url is used as-is.
	url := strings.TrimRight(l.BaseURL, "/") + "/loki/api/v1/push"
	if l.PushURL != "" {
		url = l.PushURL
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Same rule as queryLines: Basic Auth only when a credential is configured, otherwise no
	// Authorization header at all. Without it a hosted Loki answers 401 and Panel A stays empty.
	if user, pass, ok := strings.Cut(l.Credential, ":"); ok {
		req.SetBasicAuth(user, pass)
	}
	setTenant(req, l.Tenant) // T3.3: X-Scope-OrgID only when a tenant is configured
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Loki's body names the reason ("maximum active stream limit exceeded…" on a full stream cap,
		// AC-D32); the status alone reads the same for every refusal. Bounded: it is a log line.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("loki push returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
