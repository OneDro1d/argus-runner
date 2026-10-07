package argus

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-C2 (V29-015) — `## CLEANUP` IS EXECUTED, ON EVERY PATH, AND NEVER TOUCHES THE VERDICT.
//
// `Scenario.Cleanup` was parsed and read by NOTHING. `grep -rni "cleanup" internal/argus/` returned
// nothing — no defer, no teardown hook, on any path — while four documents told authors the section
// was required and enforced, and every operator was shipped a `cleanup_enabled: true` switch that no
// code read. Audited across 122 example files: 32 create data on a real system that nothing anywhere
// deletes. Twelve of those are non-chain scenarios, and for those twelve this is the ONLY channel
// there is.
//
// THE SIX EXECUTION RULES, all the owner's ruling:
//
//  1. RUNS AFTER THE SCENARIO, ON EVERY PATH — passed, failed or errored. A SQL cleanup talks to
//     the database, not to the SUT, so it can still succeed when the SUT itself is unreachable.
//     Attempt it; record what happened.
//  2. ⛔ NEVER CHANGES THE VERDICT, in either direction. A green cleanup cannot rescue a failed
//     scenario; a red cleanup cannot fail a passing one.
//  3. ⛔ MUST TOLERATE "ALREADY GONE". A cleanup that finds nothing reports SUCCESS. One that went
//     red for this reason would turn every green run into a red one — the classic way a safety net
//     gets switched off.
//  4. ITS OWN TIME BUDGET. A timeout is a FAILED cleanup, never a pass and never silence.
//  5. NO PRIVILEGE ESCALATION — the same credentials, network reach and working directory as the
//     scenario itself, never more. Both forms run through the executor the run already uses.
//  6. NO OFF SWITCH. `cleanup_enabled` is deleted and no `--no-cleanup` flag is added. An operator
//     who can silently disable cleanup recreates this finding.
//
// ⛔ AND NO OUTPUT IS CAPTURED. A bash cleanup's output would land in report.json, which agents
// fetch and paste into chat, and there is no Go-side secret scrubber. The row offers two answers —
// scrub by VALUE, or do not capture at all — and this build takes the second: a key-name filter is
// not redaction, this project has leaked live tokens twice by printing a structure and filtering
// top-level keys, and a scrubber is exactly the kind of thing that is right until the day it is not.
// What is kept is the outcome and the duration.

// CleanupExecutor is the OPTIONAL Runner capability that executes a cleanup block. A Runner that
// does not implement it makes every runnable cleanup `not-run`, with the reason said out loud —
// never silently skipped.
type CleanupExecutor interface {
	// ExecCleanup runs one block to completion or to its timeout. The error is the OUTCOME, and it
	// must never carry captured output.
	ExecCleanup(form scenario.CleanupForm, body string, props map[string]string, timeout time.Duration) error
}

// cleanupTimeout is the per-block budget (rule 4). Short on purpose: a cleanup is a delete keyed on
// one correlation id, and a cleanup that needs longer is a scenario-design problem, not a budget one.
const cleanupTimeout = 30 * time.Second

// RunCleanup executes a scenario's `## CLEANUP` blocks in the order written and returns one result
// per block. ⛔ IT NEVER RETURNS A VERDICT: the caller attaches the results to the report and does
// not consult them.
func RunCleanup(c *config.Config, s *scenario.Scenario, corr string, r Runner) []report.CleanupResult {
	if s == nil || len(s.Cleanup.Blocks) == 0 {
		return nil
	}
	var out []report.CleanupResult
	for _, b := range s.Cleanup.Blocks {
		if b.Form == scenario.CleanupNA {
			// The honest exemption. It is RECORDED, not skipped — a reader of report.json must be
			// able to see that this scenario declared it creates nothing, and why.
			out = append(out, report.CleanupResult{
				Form: string(b.Form), Outcome: report.CleanupNotRun,
				// ⚠ The body already BEGINS "N/A — …", so prefixing "declared N/A — " produced
				// `declared N/A — N/A — …` in all 33 entries of the first live report. Say it once.
				Observed: "declared " + firstLine(b.Body),
			})
			continue
		}
		ce, ok := r.(CleanupExecutor)
		if !ok {
			out = append(out, report.CleanupResult{
				Form: string(b.Form), Outcome: report.CleanupNotRun,
				Observed: "this executor cannot run a " + string(b.Form) + " cleanup — the block was NOT executed",
			})
			continue
		}
		props := cleanupProps(c, s, b, corr)
		start := time.Now()
		err := ce.ExecCleanup(b.Form, resolveCleanup(b.Body, corr), props, cleanupTimeout)
		res := report.CleanupResult{
			Form: string(b.Form), Outcome: report.CleanupOK, DurationMs: int(time.Since(start).Milliseconds()),
		}
		switch {
		case err == nil:
			// Rule 3: nothing to delete is a SUCCESS, and the wording says so, because the next
			// person to read a green cleanup line will wonder.
			res.Observed = "ran to completion (a cleanup that found nothing to remove is a success)"
		case isTimeout(err):
			res.Outcome = report.CleanupTimeout
			res.Observed = "exceeded its " + cleanupTimeout.String() + " budget and was stopped"
		default:
			res.Outcome = report.CleanupFailed
			// ⛔ NO CAPTURED OUTPUT. The error TEXT from the executor may echo a command line, and a
			// command line can carry a resolved ${VAR}. Only the shape of the failure is reported.
			res.Observed = "did not complete (its output is deliberately not captured — see the run log)"
		}
		out = append(out, res)
	}
	return out
}

// cleanupProps builds the properties a cleanup block needs. For SQL it is the SAME database
// connection a `Database State` VERIFY uses — no new configuration, which is a requirement.
func cleanupProps(c *config.Config, s *scenario.Scenario, b scenario.CleanupBlock, corr string) map[string]string {
	props := map[string]string{
		"scenario.id":       s.ID,
		"correlation.id":    corr,
		"cleanup.timeout.s": strconv.Itoa(int(cleanupTimeout.Seconds())),
	}
	if b.Form == scenario.CleanupSQL {
		if db := c.Targets.Database; db != nil {
			props["db.url"], props["db.user"], props["db.password"] = db.JDBCURL, db.Username, db.Password
		}
		props["cleanup.sql"] = resolveCleanup(b.Body, corr)
	}
	return props
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func isTimeout(err error) bool {
	return err != nil && (os.IsTimeout(err) || strings.Contains(strings.ToLower(err.Error()), "timeout") ||
		strings.Contains(strings.ToLower(err.Error()), "timed out"))
}

// cleanupJTL is where a sql cleanup's .jtl goes. It is written and never read: the verdict is the
// exit status, and the file exists only because JMeter insists on one.
func cleanupJTL(resultsDir, id string) string {
	return filepath.Join(resultsDir, "cleanup__"+id+".jtl")
}

// resolveCleanup substitutes the variables a CLEANUP block may use.
//
// ⛔ `${correlation_id}` WAS NOT HANDLED BY resolveVars UNTIL V31-003, AND THAT IS THE BUG THE LIVE
// RUN FOUND. The explicit ReplaceAll below stays: resolveVars fills it too since V31-003, so it is
// redundant, harmless, and CLEANUP must not change.
//
// So a cleanup block scoped on `${correlation_id}` reached the executor with the LITERAL
// placeholder — scoping nothing. Measured live on 2026-09-11: a bash cleanup writing
// `echo "cleanup ran for ${correlation_id}"` produced the file `cleanup ran for ` with the id
// missing, which is how this was caught.
//
// ⚠ AND THE AUTHORING SKILL TEACHES THAT EXACT FORM — "Use ${correlation_id} to scope it to this
// run: DELETE FROM <t> WHERE correlation_id = '${correlation_id}';". A product that documents a
// placeholder it does not substitute is the same defect class as the round itself, written by the
// round itself.
func resolveCleanup(body, corr string) string {
	return resolveVars(strings.ReplaceAll(body, "${correlation_id}", corr), corr)
}
