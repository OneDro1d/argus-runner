package chain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE "logs" CHAIN STEP: COUNT the lines of the executor's own Loki, judged by the `body …`
// claims on ONE JSON document:
//
//	{"count": <n>, "lines": [ "<line>", … ]}
//
// `count` is every matching line; `lines` holds the first scenario.LogsLinesKept in time order, each cut at
// scenario.LogsLineMaxBytes on a rune boundary. "A line exists" is `body has count >= 1`, "no line exists" is
// `body has count == 0`.
//
// ⛔ NO PARALLEL EXECUTOR AND NO SECOND CLAIM GRAMMAR (as the db step): a Step for the SAME chain.Run, claims by
// scenario.ParseBodyAsserts judged by mcp.BodyAssertsMissReason, `save` through saveFromHTTP.
//
// ⛔ A COUNT IS NEVER A SILENT LOWER BOUND. The query asks for obsquery.LogsLineLimit lines; a read that returns
// that many may have been cut by Loki, so the step is `error` (naming the way out), not a count.
//
// ⛔ AN UNREACHABLE LOG STORE NEVER MAKES `count == 0` HOLD. A transport failure, a non-200 answer and an answer
// that cannot be decoded are `error`, never retried by a poll, and the claims are not judged at all.
//
// ⛔ THE LINES ARE THE SYSTEM'S OWN WORDS. They reach an author only (failed_claims, saved values). Observed carries
// the count and nothing of a line, of `contains` or of `matching`; each kept line passes the token redaction and the
// credential scrub BEFORE it is kept.

// LogsSpec is one logs step as the executor resolved it.
type LogsSpec struct {
	// Scope is scenario.LogsScopeCorrelation or scenario.LogsScopeSource.
	Scope string
	// Labels are the source scope's stream labels (equality, rendered as quoted literals).
	Labels map[string]string
	// Contains is the literal line filter; the run's own id is already filled in, a ${saved.<var>} is bound at run time.
	Contains string
	// Matching is the RE2 line filter.
	Matching string
	// Window is how far back from the moment of each query to look; 0 = back to LogsLookBack before ChainStart.
	Window time.Duration
	// Settle is waited before the FIRST query: a log line is shipped after the request that caused it returns.
	Settle time.Duration
	// ChainStart is when the chain was assembled, a few milliseconds before its first step.
	ChainStart time.Time
	// Scrub cleans a message or a line of the executor's credentials (nil = none).
	Scrub func(string) string
	// Now and Sleep are the clock; nil = time.Now and time.Sleep. Tests inject theirs, so nothing waits.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// LogsStep builds the logs step. want is the step's `body …` claims; save, poll and always are an http step's.
// reader is the executor's Loki (obsquery.Loki in production).
func LogsStep(name string, spec LogsSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool, reader obsquery.LogsReader) Step {
	scrub := func(s string) string {
		if spec.Scrub != nil {
			s = spec.Scrub(s)
		}
		return s
	}
	now, sleep := spec.Now, spec.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	needs := func(vars map[string]string) error {
		if _, err := bindParam(spec.Contains, vars); err != nil {
			return err
		}
		return needsBodyAsserts(want, vars)
	}
	enforced := func() []string { return EnforcedAssertions(mcp.Expect{Body: want}) }

	run := func(cid string, vars map[string]string, chainDeadline time.Time) report.StepResult {
		contains, err := bindParam(spec.Contains, vars)
		if err != nil {
			// An authoring error (an unresolved ${saved.<var>}), never retried and nothing is sent.
			return report.StepResult{Status: "failed", Observed: err.Error()}
		}
		if spec.Contains != "" && contains == "" {
			// ⛔ a filter that bound to nothing would match EVERY line, and the count would be the whole stream's
			return report.StepResult{Status: "failed", Observed: "`contains` is empty once its ${saved.<var>} is bound - " +
				"an empty filter matches every line, so nothing was queried"}
		}
		boundWant, cerr := bindBodyAsserts(want, vars)
		if cerr != nil {
			return report.StepResult{Status: "failed", Observed: "content assertion: " + cerr.Error()}
		}
		// an executor reading an environment's SHARED Loki builds its queries with the cluster +
		// namespace selector and query-time parsing (reader is then an obsquery.LogsQueryBuilder); every other
		// reader - the instance's own Loki, a fake - gets exactly the queries it always got.
		var query string
		var qerr error
		qb, hasBuilder := reader.(obsquery.LogsQueryBuilder)
		switch {
		case spec.Scope == scenario.LogsScopeSource && hasBuilder:
			query, qerr = qb.SourceQuery(spec.Labels, contains, spec.Matching)
		case spec.Scope == scenario.LogsScopeSource:
			query, qerr = obsquery.SourceLogQL(spec.Labels, contains, spec.Matching)
		case hasBuilder:
			query, qerr = qb.CorrelationQuery(cid, contains, spec.Matching)
		default:
			query = obsquery.CorrelationLogQL(cid, contains, spec.Matching)
		}
		if qerr != nil {
			return report.StepResult{Status: "failed", Observed: "logs query could not be built: " + qerr.Error()}
		}

		timeout, interval := time.Duration(0), time.Second
		if poll != nil {
			timeout, interval = poll.Timeout, poll.Interval
			// (the floor on the interval, scenario.DBPollIntervalMin, is refused at both doors - the step trusts them)
		}
		deadline := now().Add(timeout)
		if spec.Settle > 0 {
			sleep(spec.Settle) // once, before the FIRST query: what stands between "no such line" and "not arrived yet"
		}
		attempts := 0
		for {
			attempts++
			at := now()
			from := at.Add(-spec.Window)
			if spec.Window <= 0 {
				from = spec.ChainStart.Add(-scenario.LogsLookBack)
			}
			read := reader.ReadLogs(query, from, at)
			if read.Err != nil {
				// FAIL CLOSED: not an empty answer, not retried.
				return report.StepResult{Status: report.StatusError, Observed: "the log store did not answer: " + scrub(read.Err.Error())}
			}
			if read.NoStream {
				// ⛔: a shared Loki that shows NO stream for the selector has not shown the line to be
				// absent. Never judged (a `count == 0` would hold), polled while a poll has time (the shipper batches
				// for a second), else `error` - the harness could not tell, which is not the system's answer.
				if poll == nil || now().After(deadline) {
					return report.StepResult{Status: report.StatusError, Observed: "the shared log store shows no stream at all for this step's " +
						"cluster/namespace selector in the window, so a count of 0 would not mean the line is absent - check observability.loki.shared_cluster " +
						"and the SUT namespace, and that the shared log shipper delivers this namespace's pods"}
				}
				sleep(interval)
				continue
			}
			if read.Unparsed != nil {
				// ⛔ (live check 2026-10-19): the selector holds lines but the query's parser stage reads
				// NONE of them, so a label filter (level, correlation_id, event_type) could not have matched anything and a
				// `count == 0` would hold for that reason alone. Polled while a poll has time (the SUT's first parseable line
				// may not have shipped yet), else `error`. Counts only: no line, no contains, no matching.
				if poll == nil || now().After(deadline) {
					return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
						"the shared log store holds %d line(s) for this step's selector in the window, but none of them parses as %s, "+
							"so level / correlation_id / event_type cannot be derived from this SUT's lines and a count of 0 on them would hold "+
							"for that reason alone - declare the SUT's real line format (observability.loki.log_format: logfmt, or "+
							"observability.loki.log_regex for plain text), or select without those labels", read.Unparsed.Lines, read.Unparsed.Format)}
				}
				sleep(interval)
				continue
			}
			if read.Limit > 0 && len(read.Lines) >= read.Limit {
				return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
					"the query matched %d lines or more, which is as many as one read returns, so the count would be a lower bound "+
						"and the claims judged on part of the answer - narrow `contains` / `matching` or `window`", read.Limit)}
			}
			doc, derr := logsDocument(read.Lines, cid, scrub)
			if derr != nil {
				return report.StepResult{Status: report.StatusError, Observed: derr.Error()}
			}

			miss, nonNumeric := mcp.BodyAssertsMissReason(string(doc), boundWant)
			if !miss {
				st := report.StepResult{Status: "passed", Observed: fmt.Sprintf("log query matched %d line(s)", len(read.Lines))}
				st.AssertionsEnforced = enforced()
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
				if len(save) > 0 {
					captured := make(map[string]string, len(save))
					for varName, sp := range save {
						val, fail := saveFromHTTP(doc, varName, sp)
						if fail != "" {
							return report.StepResult{Status: "failed", Observed: fail}
						}
						recordCapture(captured, varName, sp, val)
					}
					st.Captured = captured
				}
				return st
			}
			reason := "the count did not satisfy the scenario's body assertion(s)"
			if nonNumeric {
				reason = "a numeric comparison in the scenario's body assertion(s) found a non-numeric observed value"
			}
			lastObserved := fmt.Sprintf("log query matched %d line(s) but %s %s", len(read.Lines), reason, chainClaimsNote)
			lastFailed, lastOmitted := failedBodyClaims(string(doc), want, boundWant, vars, scrub)

			if poll == nil || now().After(deadline) {
				st := report.StepResult{Status: "failed", Observed: lastObserved, FailedClaims: lastFailed, FailedClaimsOmitted: lastOmitted}
				if poll != nil {
					st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)); last observed: %s", timeout, attempts, lastObserved)
				}
				st.AssertionsEnforced = enforced()
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
				return st
			}
			sleep(interval)
		}
	}

	return Step{
		Name:     name,
		Always:   always,
		OwnError: true,
		Needs: func(vars map[string]string) error {
			if always {
				return nil // a cleanup step attempts regardless - see ChainStep.Always
			}
			return needs(vars)
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			return run(cid, vars, time.Time{})
		},
		RunUntil: run,
	}
}

// logsDocument builds the ONE JSON document the claims are judged against. Each kept line is redacted (the
// credential scrub, then the token redaction the sandbox reader uses, with the run's own id held out of it so a
// claim can still look for it) and only then cut, so a token is never cut into a piece that no longer looks like one.
func logsDocument(lines []obsquery.SandboxLine, cid string, scrub func(string) string) ([]byte, error) {
	kept := make([]string, 0, scenario.LogsLinesKept)
	for i := 0; i < len(lines) && i < scenario.LogsLinesKept; i++ {
		kept = append(kept, cutOnRune(redactLogLine(lines[i].Line, cid, scrub), scenario.LogsLineMaxBytes))
	}
	doc, err := jsonText(struct {
		Count int      `json:"count"`
		Lines []string `json:"lines"`
	}{len(lines), kept})
	if err != nil {
		return nil, fmt.Errorf("the lines cannot be placed in the document: %v", err)
	}
	return doc, nil
}

// redactLogLine is the credential scrub, then obsquery.RedactTokens on every piece of the line that is not the
// run's own correlation id.
func redactLogLine(line, cid string, scrub func(string) string) string {
	line = scrub(line)
	if cid == "" || !strings.Contains(line, cid) {
		return obsquery.RedactTokens(line)
	}
	parts := strings.Split(line, cid)
	for i := range parts {
		parts[i] = obsquery.RedactTokens(parts[i])
	}
	return strings.Join(parts, cid)
}

// cutOnRune cuts s to at most max bytes without splitting a rune.
func cutOnRune(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
