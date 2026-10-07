package argus

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Spec 26 P1 (A1, observe only): the run loop stamps each scenario's window, reads the sandbox
// evidence ONCE after the loop, and attaches a sandbox_policy block to every row — without ever
// touching a verdict.

const testPad = 2 * time.Second

func openshellConfig() *config.Config {
	c := httpConfig()
	c.Observability.OpenShell = &config.OpenShellObs{
		Source: "loki", Selector: `{job="openshell-gateway"}`, Sandbox: "sb-1", WindowPad: testPad.String(),
	}
	return c
}

type evCall struct {
	selector, sandbox string
	from, to          time.Time
}

// fakeEvidence records every read and answers with what respond returns (a denial inside the
// window by default).
type fakeEvidence struct {
	mu      sync.Mutex
	calls   []evCall
	respond func(from, to time.Time) obsquery.SandboxRead
}

func (f *fakeEvidence) SandboxLines(selector, sandbox string, from, to time.Time) obsquery.SandboxRead {
	f.mu.Lock()
	f.calls = append(f.calls, evCall{selector, sandbox, from, to})
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(from, to)
	}
	return deniedInside(from, to)
}

// deniedInside answers one allowed and one denied line, both at the middle of the padded window
// [from, to - pad]. The line is the denied CONNECT of OpenShell's docs
// (observability/ocsf-json-export.mdx:120-150 at v0.1.2) with `time`, `metadata` and `container`
// added — a doc example, not a capture.
func deniedInside(from, to time.Time) obsquery.SandboxRead {
	mid := from.Add(to.Add(-testPad).Sub(from) / 2)
	ts := func(t time.Time) string { return fmt.Sprint(t.UnixNano()) }
	allowed := fmt.Sprintf(`{"class_uid":4001,"activity_name":"Open","action_id":1,"action":"Allowed","time":%d,"metadata":{"version":"1.8.0"},"dst_endpoint":{"domain":"api.github.com","port":443},"container":{"uid":"sb-1"}}`, mid.UnixMilli())
	denied := fmt.Sprintf(`{"class_uid":4001,"activity_name":"Open","action_id":2,"action":"Denied","disposition":"Blocked","status_detail":"no matching policy","time":%d,"metadata":{"version":"1.8.0"},"dst_endpoint":{"domain":"httpbin.org","port":443},"actor":{"process":{"name":"/usr/bin/curl"}},"firewall_rule":{"name":"-"},"container":{"uid":"sb-1"}}`, mid.UnixMilli())
	return obsquery.SandboxRead{Limit: obsquery.SandboxLineLimit, Lines: []obsquery.SandboxLine{
		{TS: ts(mid), Line: allowed}, {TS: ts(mid), Line: denied},
	}}
}

// stubSleep replaces the one wait with a recorder for the test's lifetime.
func stubSleep(t *testing.T) *[]time.Time {
	t.Helper()
	var slept []time.Time
	prev := sandboxPolicySleepUntil
	sandboxPolicySleepUntil = func(until time.Time) { slept = append(slept, until) }
	t.Cleanup(func() { sandboxPolicySleepUntil = prev })
	return &slept
}

// noCount fails unless an unavailable block serialises "denied_count":null and "events":[]: spec
// 26 §4 and §7, unavailable never reads as "no denials", so it carries no count at all.
func noCount(t *testing.T, id string, p *report.SandboxPolicy) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"denied_count":null`) || !strings.Contains(string(b), `"events":[]`) {
		t.Errorf("%s: an unavailable block must carry \"denied_count\":null and \"events\":[]: %s", id, b)
	}
}

func rows(rep *report.Report) []report.ScenarioResult {
	var out []report.ScenarioResult
	for _, l := range rep.Layers {
		out = append(out, l.Scenarios...)
	}
	return out
}

// Invariant 1: no block, no query, no wait, no field.
func TestRunAll_SandboxPolicyOff_NoBlockNoQueryNoWait(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	slept := stubSleep(t)
	ev := &fakeEvidence{}
	rr, err := RunAllWithEvidence(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{"ORD-001": true}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.calls) != 0 || len(*slept) != 0 {
		t.Fatalf("feature off: want 0 reads and 0 waits, got %d reads, %d waits", len(ev.calls), len(*slept))
	}
	for _, r := range rows(rr.Report) {
		if r.SandboxPolicy != nil {
			t.Fatalf("feature off: %s carries a sandbox_policy block", r.ID)
		}
	}
}

// Invariant 2: denials are SHOWN and the verdict is unchanged — the same statuses, failures and
// tallies as the same run with the feature off.
func TestRunAll_SandboxPolicyNeverChangesVerdict(t *testing.T) {
	run := func(c *config.Config, ev obsquery.SandboxEvidence) *report.Report {
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
		writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")
		rr, err := RunAllWithEvidence(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{"ORD-001": true}}, ev)
		if err != nil {
			t.Fatal(err)
		}
		return rr.Report
	}
	stubSleep(t)
	off := run(httpConfig(), nil)
	on := run(openshellConfig(), &fakeEvidence{})
	if on.Summary != off.Summary {
		t.Fatalf("tallies changed: off %+v, on %+v", off.Summary, on.Summary)
	}
	offRows, onRows := rows(off), rows(on)
	for i := range offRows {
		a, b := offRows[i], onRows[i]
		if a.ID != b.ID || a.Status != b.Status || (a.Failure == nil) != (b.Failure == nil) ||
			(a.Failure != nil && a.Failure.Observed != b.Failure.Observed) {
			t.Errorf("%s: verdict changed: off {%s %+v} on {%s %+v}", a.ID, a.Status, a.Failure, b.Status, b.Failure)
		}
		if b.SandboxPolicy == nil || b.SandboxPolicy.DeniedCount == nil || *b.SandboxPolicy.DeniedCount != 1 || b.SandboxPolicy.Coverage != report.CoverageComplete {
			t.Errorf("%s: the denial must be SHOWN (the test proves nothing otherwise): %+v", b.ID, b.SandboxPolicy)
		}
	}
	if offRows[0].Status != "passed" || offRows[1].Status != "failed" {
		t.Fatalf("fixture: want one pass and one fail, got %s / %s", offRows[0].Status, offRows[1].Status)
	}
}

// clockRunner records when the scenario and its cleanup ran.
type clockRunner struct {
	fakeRunner
	mu                sync.Mutex
	runAt, cleanupAts []time.Time
}

func (r *clockRunner) Run(templateBase, jtlPath string, props map[string]string, d time.Duration) error {
	time.Sleep(3 * time.Millisecond)
	r.mu.Lock()
	r.runAt = append(r.runAt, time.Now())
	r.mu.Unlock()
	time.Sleep(3 * time.Millisecond)
	return r.fakeRunner.Run(templateBase, jtlPath, props, d)
}

func (r *clockRunner) ExecCleanup(form scenario.CleanupForm, body string, props map[string]string, timeout time.Duration) error {
	time.Sleep(3 * time.Millisecond)
	r.mu.Lock()
	r.cleanupAts = append(r.cleanupAts, time.Now())
	r.mu.Unlock()
	return nil
}

// The window brackets the scenario's own execution and NOT its cleanup ( a cleanup talks to
// the database, not to the agent), and the read covers the padded window plus one pad of settle.
func TestRunAll_SandboxPolicyWindowBracketsTheScenario(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "CLN-001", strings.Join([]string{
		"# Scenario: CLN-001", "", "## Metadata", "- **ID**: CLN-001", "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT", "### Runnable", "- status=202", "",
		"## CLEANUP", "```sql\nDELETE FROM orders WHERE correlation_id = '${correlation_id}';\n```", "",
	}, "\n"))
	stubSleep(t)
	r := &clockRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}}}
	ev := &fakeEvidence{}
	rr, err := RunAllWithEvidence(openshellConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", r, ev)
	if err != nil {
		t.Fatal(err)
	}
	row := rows(rr.Report)[0]
	if len(r.runAt) != 1 || len(r.cleanupAts) != 1 {
		t.Fatalf("fixture: want one run and one cleanup, got %d / %d", len(r.runAt), len(r.cleanupAts))
	}
	if row.WindowStart.IsZero() || row.WindowStart.After(r.runAt[0]) || row.WindowEnd.Before(r.runAt[0]) {
		t.Errorf("the window [%s, %s] must hold the scenario's run at %s", row.WindowStart, row.WindowEnd, r.runAt[0])
	}
	if !r.cleanupAts[0].After(row.WindowEnd) {
		t.Errorf("the cleanup at %s must fall AFTER the window's end %s", r.cleanupAts[0], row.WindowEnd)
	}
	if len(ev.calls) != 1 {
		t.Fatalf("want one read, got %d", len(ev.calls))
	}
	c := ev.calls[0]
	if c.selector != `{job="openshell-gateway"}` || c.sandbox != "sb-1" ||
		!c.from.Equal(row.WindowStart.Add(-testPad)) || !c.to.Equal(row.WindowEnd.Add(2*testPad)) {
		t.Errorf("read = %+v; want selector, sb-1, [start-pad, end+2pad] = [%s, %s]", c, row.WindowStart.Add(-testPad), row.WindowEnd.Add(2*testPad))
	}
	if w := row.SandboxPolicy.Window; w == nil || w.From != row.WindowStart.Add(-testPad).UTC().Format(obsquery.RFC3339Milli) {
		t.Errorf("the block's window must be the padded window, got %+v", w)
	}
}

// (finding A4): one wait for the whole run, past the last padded window plus one pad — not
// one pad per scenario.
func TestRunAll_SandboxPolicyWaitsOncePastLastWindow(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	for _, id := range []string{"ORD-001", "ORD-002", "ORD-003"} {
		writeScenario(t, scDir, "http-ingestion", id, "status=202")
	}
	slept := stubSleep(t)
	ev := &fakeEvidence{}
	rr, err := RunAllWithEvidence(openshellConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 {
		t.Fatalf("want exactly one wait for the run, got %d", len(*slept))
	}
	var lastEnd time.Time
	for _, r := range rows(rr.Report) {
		if r.WindowEnd.After(lastEnd) {
			lastEnd = r.WindowEnd
		}
	}
	if want := lastEnd.Add(2 * testPad); !(*slept)[0].Equal(want) {
		t.Errorf("waited until %s, want the last window's end + 2·pad = %s", (*slept)[0], want)
	}
	if len(ev.calls) != 3 {
		t.Errorf("want one read per scenario, got %d", len(ev.calls))
	}
}

func TestRunAll_SandboxPolicySourceDownIsUnavailableEverywhere(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")
	stubSleep(t)
	down := &fakeEvidence{respond: func(_, _ time.Time) obsquery.SandboxRead {
		return obsquery.SandboxRead{Limit: obsquery.SandboxLineLimit, Err: fmt.Errorf("the Loki request failed: dial tcp 10.0.0.1:3100: connection refused")}
	}}
	rr, err := RunAllWithEvidence(openshellConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{"ORD-001": true}}, down)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows(rr.Report) {
		p := r.SandboxPolicy
		if p == nil || p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "connection refused") {
			t.Errorf("%s: a source that is down must be unavailable with its reason, got %+v", r.ID, p)
		}
		noCount(t, r.ID, p)
	}
	if rr.Report.Summary.Passed != 1 || rr.Report.Summary.Failed != 1 {
		t.Errorf("statuses must be unchanged: %+v", rr.Report.Summary)
	}
}

// The block is on, but nobody wired a reader (a direct RunAll caller): every row says so.
func TestRunAll_SandboxPolicyNoReaderIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	slept := stubSleep(t)
	rr, err := RunAll(openshellConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{"ORD-001": true}})
	if err != nil {
		t.Fatal(err)
	}
	p := rows(rr.Report)[0].SandboxPolicy
	if p == nil || p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "no evidence reader was wired") {
		t.Fatalf("block on with no reader must be unavailable, got %+v", p)
	}
	noCount(t, "ORD-001", p)
	if len(*slept) != 0 {
		t.Errorf("nothing to read, so nothing to wait for: %d waits", len(*slept))
	}
}

// A sandbox id that fails the charset rule is never written into a block, here on the path that
// never reads (no reader wired), as on the read path (obsquery).
func TestRunAll_SandboxPolicyUnsafeIDNotWritten(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	stubSleep(t)
	c := openshellConfig()
	c.Observability.OpenShell.Sandbox = `sb"} or {x="1`
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{pass: map[string]bool{"ORD-001": true}})
	if err != nil {
		t.Fatal(err)
	}
	if p := rows(rr.Report)[0].SandboxPolicy; p == nil || p.Sandbox != "" {
		t.Fatalf("an unsafe sandbox id must not reach the block, got %+v", p)
	}
}

// block on ⇒ every row carries one, including the rows of a run the executor preflight
// short-circuited before any scenario ran.
func TestRunAll_ShortCircuitRowsCarryUnavailableBlock(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")
	slept := stubSleep(t)
	ev := &fakeEvidence{}
	r := healthFailRunner{&fakeRunner{pass: map[string]bool{"ORD-001": true, "ORD-002": true}}}
	rr, err := RunAllWithEvidence(openshellConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", r, ev)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Errored != 2 {
		t.Fatalf("fixture: the preflight must short-circuit, got %+v", rr.Report.Summary)
	}
	for _, row := range rows(rr.Report) {
		p := row.SandboxPolicy
		if p == nil || p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "short-circuited before any scenario ran") || p.Window != nil {
			t.Errorf("%s: want an unavailable block with the short-circuit reason and no window, got %+v", row.ID, p)
		}
		noCount(t, row.ID, p)
	}
	if len(ev.calls) != 0 || len(*slept) != 0 {
		t.Errorf("no scenario ran, so nothing is read or waited for: %d reads, %d waits", len(ev.calls), len(*slept))
	}
}
