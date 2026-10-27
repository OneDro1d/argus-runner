package obsquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Push-side limits. Each is justified against Loki's own defaults.
const (
	// lokiPushMaxBodyBytes caps ONE push body. Loki's distributor wraps a JSON body in
	// io.LimitReader(max_recv_msg_size+1) (default 100 MiB) BEFORE its size check, so an oversized body
	// is cut off mid-stream and reported as "unexpected end of input" (the 730 MB run). 1 MiB is a
	// quarter of the smallest default on the path (gRPC 4 MiB) and a sixth of the per-tenant burst
	// (ingestion_burst_size_mb 6), so a batch is never the thing a limit refuses.
	lokiPushMaxBodyBytes = 1 << 20
	// lokiPushRequestTimeout bounds ONE batch POST. The old 4 s covered a tiny body; a 1 MiB body
	// to a loaded Loki needs more room, and 10 s is still far under the whole-push bound below.
	lokiPushRequestTimeout = 10 * time.Second
	// lokiRateLimitRetries is how many times ONE batch is retried after Loki's per-tenant ingestion
	// rate limit (a 429 whose reason says "rate limit"). Any other refusal is not retried.
	lokiRateLimitRetries = 3
	// lokiBodyOverhead is the size of an empty push body, {"streams":[]}, with room to spare.
	lokiBodyOverhead = len(`{"streams":[]}`) + 16
)

// lokiPushTotalBudget bounds the WHOLE push, every batch and every retry. PushRequestEvents runs
// synchronously inside the run, before the result upload, so a slow Loki must not hold the result for
// minutes: 30 s is generous for a push that normally takes milliseconds, and at Loki's default 4 MB/s
// ingestion rate it still carries ~120 MB (~600k aggregated lines). Past it the push stops and says how
// far it got. A var so a test can shrink it.
var lokiPushTotalBudget = 30 * time.Second

// lokiRateLimitBackoff is the wait before the first rate-limit retry (doubled for each further one).
// A var so a test need not sleep.
var lokiRateLimitBackoff = time.Second

// requestEventLine is one aggregated line: how many requests of one outcome one scenario fired in one
// 1-second bucket. Lines stored by executors before carry no count field, and every
// reader treats such a line as 1.
type requestEventLine struct {
	RunID         string `json:"run_id"`
	ScenarioID    string `json:"scenario_id"`
	CorrelationID string `json:"correlation_id"`
	Outcome       string `json:"outcome"`
	Count         int64  `json:"count"`
}

// requestEventKey is the aggregation key: one line per (scenario, correlation id, outcome, second).
type requestEventKey struct {
	outcome  string
	scenario string
	corr     string
	sec      int64 // fire time, floored to the second
}

// PushRequestEvents ships the runner's request events to Loki so Panel A can plot the genuine
// within-run distribution via sum(sum_over_time({job="argus-runner",event_type="request"} | json count
// | label_format n=... | unwrap n [$__interval])), split by `outcome`.
//
// AGGREGATED: one line per (scenario_id, correlation_id, outcome, 1-second bucket of
// fire time) carrying "count":N, stamped at the bucket start. One line per request meant ~3.85M lines
// and a ~730 MB body on a real load run, which Loki cut off mid-stream. The stream
// LABELS are unchanged (job, event_type, outcome, argus_instance, project, cluster): no new label, so
// cardinality stays flat. A request's time is therefore known to the second, not the millisecond.
//
// The lines are sent in order as several pushes, each at most lokiPushMaxBodyBytes, and the whole push
// is bounded by lokiPushTotalBudget. A failed batch returns an error saying how many lines got through.
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

	// Aggregate straight from the samples: memory is bounded by the number of DISTINCT keys, never by
	// the number of requests. scenario_id + correlation_id come from the owning ScenarioResult; run_id
	// is the run handle.
	counts := map[requestEventKey]int64{}
	for _, lay := range rep.Layers {
		for _, s := range lay.Scenarios {
			for _, smp := range s.Requests {
				switch smp.Outcome {
				case report.OutcomeSuccess, report.OutcomeFailed, report.OutcomeError:
				default:
					continue
				}
				counts[requestEventKey{outcome: smp.Outcome, scenario: s.ID, corr: s.CorrelationID, sec: floorDiv(smp.AtMs, 1000)}]++
			}
		}
	}
	if len(counts) == 0 {
		return nil // the run fired no request (e.g. a dead rig) — nothing to plot
	}
	byOutcome := map[string][]requestEventKey{}
	for k := range counts {
		byOutcome[k.outcome] = append(byOutcome[k.outcome], k)
	}

	// Batches are filled in order, outcome by outcome and second by second, so every stream's entries
	// arrive in time order across the pushes as well as within one.
	var batches [][]lokiPushStream
	var cur []lokiPushStream
	curSize := lokiBodyOverhead
	totalLines := 0
	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
		}
		cur, curSize = nil, lokiBodyOverhead
	}
	// Deterministic outcome order keeps the push stable (test-friendly).
	for _, outcome := range []string{report.OutcomeSuccess, report.OutcomeFailed, report.OutcomeError} {
		keys := byOutcome[outcome]
		if len(keys) == 0 {
			continue
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if a.sec != b.sec {
				return a.sec < b.sec
			}
			if a.scenario != b.scenario {
				return a.scenario < b.scenario
			}
			return a.corr < b.corr
		})
		labels := map[string]string{
			"job":            "argus-runner",
			"event_type":     "request",
			"outcome":        outcome,
			"argus_instance": instance,
			"project":        project,
			"cluster":        cluster,
		}
		hdr, err := json.Marshal(lokiPushStream{Stream: labels, Values: [][2]string{}})
		if err != nil {
			return err
		}
		open := false // is this outcome's stream already the last one in the current batch?
		// Loki requires per-stream entries in non-decreasing timestamp order. Keys are sorted by
		// bucket; force STRICTLY increasing nanoseconds (bump same-bucket lines by +1ns) so the push is
		// accepted regardless of the bundled Loki's out-of-order/duplicate policy — a nanosecond nudge
		// is invisible on a chart.
		var lastNs int64 = -1
		for _, k := range keys {
			ns := k.sec * 1_000_000_000
			if ns <= lastNs {
				ns = lastNs + 1
			}
			lastNs = ns
			line, err := json.Marshal(requestEventLine{RunID: rep.RunID, ScenarioID: k.scenario, CorrelationID: k.corr, Outcome: outcome, Count: counts[k]})
			if err != nil {
				return err
			}
			val := [2]string{strconv.FormatInt(ns, 10), string(line)}
			vb, err := json.Marshal(val)
			if err != nil {
				return err
			}
			need := len(vb) + 1 // the value and its comma
			if !open {
				need += len(hdr) + 1
			}
			if len(cur) > 0 && curSize+need > lokiPushMaxBodyBytes {
				flush()
				open = false
			}
			if !open {
				cur = append(cur, lokiPushStream{Stream: labels, Values: make([][2]string, 0, 64)})
				curSize += len(hdr) + 1
				open = true
			}
			cur[len(cur)-1].Values = append(cur[len(cur)-1].Values, val)
			curSize += len(vb) + 1
			totalLines++
		}
	}
	flush()

	ctx, cancel := context.WithTimeout(context.Background(), lokiPushTotalBudget)
	defer cancel()
	sent := 0
	for i, streams := range batches {
		n := 0
		for _, s := range streams {
			n += len(s.Values)
		}
		body, err := json.Marshal(lokiPushBody{Streams: streams})
		if err != nil {
			return fmt.Errorf("%d/%d lines sent (batch %d/%d not built): %w", sent, totalLines, i+1, len(batches), err)
		}
		if len(body) > lokiPushMaxBodyBytes {
			// Never send what Loki would cut off mid-stream: a sizing bug must fail loudly.
			return fmt.Errorf("%d/%d lines sent (batch %d/%d is %d bytes, over the %d budget)", sent, totalLines, i+1, len(batches), len(body), lokiPushMaxBodyBytes)
		}
		if err := postWithRateLimitRetry(ctx, l, body); err != nil {
			return fmt.Errorf("%d/%d lines sent (batch %d/%d failed): %w", sent, totalLines, i+1, len(batches), err)
		}
		sent += n
	}
	return nil
}

// floorDiv is a/b rounded toward negative infinity (a fire time before the epoch still buckets down).
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// postWithRateLimitRetry posts one batch. Loki's per-tenant ingestion rate limit (a 429 naming "rate
// limit") is retried with doubling backoff, inside ctx; every other refusal, and a 429 about something
// else (a full stream cap, AC-D32), is returned at once.
func postWithRateLimitRetry(ctx context.Context, l *Loki, body []byte) error {
	wait := lokiRateLimitBackoff
	for attempt := 0; ; attempt++ {
		err := postLokiPush(ctx, l, body, lokiPushRequestTimeout)
		var pe *PushStatusError
		if err == nil || !errors.As(err, &pe) || pe.Status != http.StatusTooManyRequests ||
			!strings.Contains(strings.ToLower(pe.Body), "rate limit") || attempt >= lokiRateLimitRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// postLokiPush POSTs one push body to the Loki l names: PushURL verbatim when a hosted Loki declares one,
// else BaseURL + /loki/api/v1/push; Basic Auth only when a credential is configured; X-Scope-OrgID only
// when a tenant is. One function for every push, so the events and the SUT-log collector cannot drift.
func postLokiPush(ctx context.Context, l *Loki, body []byte, timeout time.Duration) error {
	// a hosted Loki (export) declares its own push endpoint, which need not share a
	// host with the query url the executor was given — push_url is used as-is.
	url := strings.TrimRight(l.BaseURL, "/") + "/loki/api/v1/push"
	if l.PushURL != "" {
		url = l.PushURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Loki's body names the reason ("maximum active stream limit exceeded…" on a full stream cap,
		// AC-D32); the status alone reads the same for every refusal. Bounded: it is a log line.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &PushStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return nil
}

// PushStatusError is a Loki push answered with a non-2xx status. Error() keeps the text
// PushRequestEvents has always returned.
type PushStatusError struct {
	Status int
	Body   string
}

func (e *PushStatusError) Error() string {
	return fmt.Sprintf("loki push returned %d: %s", e.Status, e.Body)
}
