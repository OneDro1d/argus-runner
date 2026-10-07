package argus

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-C2 (V29-015) — `## CLEANUP` IS EXECUTED, ON EVERY PATH, AND NEVER TOUCHES THE VERDICT.

// recordingCleanupRunner is a Runner that also executes cleanups, recording every call.
type recordingCleanupRunner struct {
	fakeRunner
	calls []string
	fail  error
	slow  time.Duration
}

func (r *recordingCleanupRunner) ExecCleanup(form scenario.CleanupForm, body string, props map[string]string, timeout time.Duration) error {
	r.calls = append(r.calls, string(form)+":"+strings.TrimSpace(body))
	if r.slow > 0 {
		return errors.New("context deadline exceeded: cleanup timed out")
	}
	return r.fail
}

func cleanupScenario(t *testing.T, cleanup string) *scenario.Scenario {
	t.Helper()
	md := strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CLN-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT",
		"### Runnable",
		"- status=202", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP",
		cleanup, "",
	}, "\n")
	return scenario.Parse(md)
}

// EVERY declared block runs, IN ORDER. The old parser took the sql block ELSE the bash block —
// first wins, the rest discarded — so a scenario that declared both cleaned up half of what it made.
func TestCleanup_EveryBlockRunsInOrder(t *testing.T) {
	s := cleanupScenario(t, "```sql\nDELETE FROM t WHERE c = 1;\n```\n\n```bash\nrm -f /tmp/marker\n```")
	r := &recordingCleanupRunner{}
	got := RunCleanup(&config.Config{}, s, "tr-cln", r)

	if len(r.calls) != 2 || !strings.HasPrefix(r.calls[0], "sql:") || !strings.HasPrefix(r.calls[1], "bash:") {
		t.Fatalf("both blocks must run, in the order written: %v", r.calls)
	}
	if len(got) != 2 {
		t.Fatalf("one result per block: %+v", got)
	}
	for i, res := range got {
		if res.Outcome != report.CleanupOK {
			t.Errorf("block %d = %q, want ok", i, res.Outcome)
		}
	}
}

// ⛔ RULE 3 — "already gone" is a SUCCESS. A cleanup that went red for finding nothing would turn
// every green run into a red one, which is how a safety net gets switched off.
func TestCleanup_NothingToDeleteIsSuccess(t *testing.T) {
	s := cleanupScenario(t, "```sql\nDELETE FROM t WHERE c = 'never-existed';\n```")
	got := RunCleanup(&config.Config{}, s, "tr-cln", &recordingCleanupRunner{}) // no error = nothing matched
	if len(got) != 1 || got[0].Outcome != report.CleanupOK {
		t.Fatalf("a cleanup that removes nothing is ok: %+v", got)
	}
	if !strings.Contains(got[0].Observed, "found nothing to remove is a success") {
		t.Errorf("…and the wording must say so, because the next reader will wonder: %q", got[0].Observed)
	}
}

// RULE 4 — a timeout is a FAILED cleanup, never a pass and NEVER SILENCE.
func TestCleanup_TimeoutIsRecordedNotSwallowed(t *testing.T) {
	s := cleanupScenario(t, "```bash\nsleep 600\n```")
	got := RunCleanup(&config.Config{}, s, "tr-cln", &recordingCleanupRunner{slow: time.Second})
	if len(got) != 1 || got[0].Outcome != report.CleanupTimeout {
		t.Fatalf("a timeout must be recorded as its own outcome: %+v", got)
	}
	if got[0].Observed == "" {
		t.Error("never silence — a timed-out cleanup must say what happened")
	}
}

// ⛔ NO CAPTURED OUTPUT reaches the report. A bash cleanup's stdout would land in report.json,
// which agents fetch and paste into chat, and a command line can carry a resolved ${VAR}.
func TestCleanup_NeverCarriesCommandOutput(t *testing.T) {
	s := cleanupScenario(t, "```bash\necho $SECRET_TOKEN\n```")
	r := &recordingCleanupRunner{fail: errors.New("exit 1: psql: FATAL: password authentication failed for user \"argus\" (dsn=postgres://u:hunter2@db)")}
	got := RunCleanup(&config.Config{}, s, "tr-cln", r)
	if len(got) != 1 || got[0].Outcome != report.CleanupFailed {
		t.Fatalf("want a failed cleanup: %+v", got)
	}
	for _, leak := range []string{"hunter2", "postgres://", "psql", "dsn"} {
		if strings.Contains(got[0].Observed, leak) {
			t.Fatalf("the executor's error text reached the report (%q) — cleanup output is NEVER "+
				"captured, because there is no Go-side secret scrubber: %q", leak, got[0].Observed)
		}
	}
}

// The N/A exemption is RECORDED, not skipped: a reader of report.json must be able to see that this
// scenario declared it creates nothing, and why.
func TestCleanup_NAIsRecordedWithItsJustification(t *testing.T) {
	s := cleanupScenario(t, "N/A — this scenario is read-only and creates nothing.")
	r := &recordingCleanupRunner{}
	got := RunCleanup(&config.Config{}, s, "tr-cln", r)
	if len(r.calls) != 0 {
		t.Fatalf("an N/A must execute nothing: %v", r.calls)
	}
	if len(got) != 1 || got[0].Outcome != report.CleanupNotRun {
		t.Fatalf("want one not-run entry: %+v", got)
	}
	if !strings.Contains(got[0].Observed, "read-only") {
		t.Errorf("the justification IS the record and must survive into the report: %q", got[0].Observed)
	}
}

// A Runner that cannot execute cleanups says so OUT LOUD. Silence here is the whole finding.
func TestCleanup_AnExecutorThatCannotRunItSaysSo(t *testing.T) {
	s := cleanupScenario(t, "```sql\nDELETE FROM t;\n```")
	got := RunCleanup(&config.Config{}, s, "tr-cln", &fakeRunner{}) // no ExecCleanup
	if len(got) != 1 || got[0].Outcome != report.CleanupNotRun {
		t.Fatalf("want a not-run entry: %+v", got)
	}
	if !strings.Contains(got[0].Observed, "NOT executed") {
		t.Errorf("it must be explicit that nothing ran: %q", got[0].Observed)
	}
}

// ⛔ RULE 2 — THE VERDICT IS UNTOUCHED, IN EITHER DIRECTION. This is the one that matters most: a
// cleanup that could change a verdict would make every scenario's result depend on teardown code.
func TestCleanup_NeverChangesTheVerdict(t *testing.T) {
	dir := t.TempDir()
	sc := writeCleanupScenario(t, dir, "```bash\nfalse\n```") // a cleanup that always fails
	// The scenario itself PASSES — that is the point: the cleanup must not be able to change it.
	r := &recordingCleanupRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}},
		fail: errors.New("exit 1")}
	rr, err := RunAll(httpConfig(), sc, dir+"/results", "p", "", "", "", "", r)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("CLN-001")
	if res == nil {
		t.Fatal("scenario missing from the report")
	}
	if res.Status != "passed" {
		t.Fatalf("a FAILED cleanup must not fail a passing scenario; status = %q", res.Status)
	}
	if len(res.Cleanup) != 1 || res.Cleanup[0].Outcome != report.CleanupFailed {
		t.Fatalf("…and it must still be REPORTED: %+v", res.Cleanup)
	}
	if len(r.calls) != 1 {
		t.Errorf("the cleanup must have run exactly once: %v", r.calls)
	}
}

func writeCleanupScenario(t *testing.T, dir, cleanup string) string {
	t.Helper()
	sc := dir + "/scenarios/http-ingestion"
	mustMkdirAll(t, sc)
	md := strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CLN-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"GET `${INGESTION_URL}/health`", "",
		"## EXPECT",
		"### Runnable",
		"- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP",
		cleanup, "",
	}, "\n")
	mustWriteFile(t, sc+"/CLN-001.md", md)
	return dir + "/scenarios"
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ⛔ `${correlation_id}` MUST BE SUBSTITUTED IN A CLEANUP BLOCK — FOUND BY A LIVE RUN.
//
// `resolveVars` covered only `${cid}` and `${VAR}` until V31-003; `${correlation_id}` was a
// SEPARATE, explicit substitution done only for a VERIFY query. So a cleanup scoped on `${correlation_id}` —
// the form the authoring skill TEACHES — reached the executor with the literal placeholder and
// scoped nothing.
//
// Measured live 2026-09-11 on the isolated demo SUT: a bash cleanup running
// `echo "cleanup ran for ${correlation_id}" >> marker.txt` wrote `cleanup ran for ` with the id
// missing. Every unit test passed while that was true, because no test asserted on the BODY the
// executor receives.
func TestCleanup_CorrelationIDIsSubstitutedInTheBody(t *testing.T) {
	for _, form := range []string{"sql", "bash"} {
		body := map[string]string{
			"sql":  "```sql\nDELETE FROM t WHERE correlation_id = '${correlation_id}';\n```",
			"bash": "```bash\nrm -f /tmp/${correlation_id}.marker\n```",
		}[form]
		r := &recordingCleanupRunner{}
		RunCleanup(&config.Config{}, cleanupScenario(t, body), "tr-live-42", r)

		if len(r.calls) != 1 {
			t.Fatalf("%s: want one call, got %v", form, r.calls)
		}
		got := r.calls[0]
		if strings.Contains(got, "${correlation_id}") {
			t.Errorf("%s: the literal placeholder reached the executor — it scopes NOTHING: %q", form, got)
		}
		if !strings.Contains(got, "tr-live-42") {
			t.Errorf("%s: the correlation id is missing from the body the executor runs: %q", form, got)
		}
	}
}

// …and the SQL path's property carries it too — the template reads `cleanup.sql`, not the body.
func TestCleanup_TheSQLPropertyCarriesTheResolvedQuery(t *testing.T) {
	s := cleanupScenario(t, "```sql\nDELETE FROM t WHERE correlation_id = '${correlation_id}';\n```")
	props := cleanupProps(&config.Config{}, s, s.Cleanup.Blocks[0], "tr-live-99")
	q := props["cleanup.sql"]
	if strings.Contains(q, "${correlation_id}") || !strings.Contains(q, "tr-live-99") {
		t.Fatalf("cleanup.sql must carry the RESOLVED query — the template reads this property, not "+
			"the block: %q", q)
	}
}
