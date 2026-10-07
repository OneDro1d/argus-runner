package argus

// cmp11_test.go -- ARGUS-CMP-11 (, -17): the executor half of tolerance values, load numbers and the
// target of a comparison member. The REAL run loop (RunAllWithTarget) over real scenario files; the SUT is an httptest
// server (chain http steps) or a fake JMeter Runner that writes the .jtl and the capture side files (the JMX itself is
// not run here).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

const tolSection = cmpSection + "- **Tolerance**: $.total abs 0.01\n"

// loadSection is the COMPARE block of a load check: its own claim is a property, its performance a band.
const loadSection = "\n## COMPARE\n- **Reference**: property\n- **Agreement**: 100%\n- **Not Worse Than**: p95 20%; error_rate 0.5pp\n"

func runCmpTarget(t *testing.T, c *config.Config, files map[string]string, mode, target string, r Runner) cmpRun {
	t.Helper()
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(sc, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, body := range files {
		if err := os.WriteFile(filepath.Join(sc, id+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resultsDir := filepath.Join(dir, "results", "local")
	if r == nil {
		r = &capRunner{t: t}
	}
	rr, err := RunAllWithTarget(c, sc, resultsDir, "p", "", "", "", "run-cmp11", r, nil, mode, target)
	if err != nil {
		t.Fatal(err)
	}
	return cmpRun{rr: rr, resultsDir: resultsDir, runID: "run-cmp11"}
}

// loadRunner is a fake JMeter for a load check: one jtl row per elapsed value, labelled with the scenario id, and a
// 500 for the first `errors` rows.
type loadRunner struct {
	t       *testing.T
	elapsed []int
	errors  int
	props   []map[string]string
}

func (r *loadRunner) Run(base, jtl string, props map[string]string, _ time.Duration) error {
	cp := map[string]string{}
	for k, v := range props {
		cp[k] = v
	}
	r.props = append(r.props, cp)
	rows := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"
	for i, e := range r.elapsed {
		code, ok := "200", "true"
		if i < r.errors {
			code, ok = "500", "false"
		}
		rows += "1," + strconv.Itoa(e) + "," + props["scenario.id"] + "," + code + ",OK,t," + ok + ",\n"
	}
	if err := os.WriteFile(jtl, []byte(rows), 0o600); err != nil {
		return err
	}
	return os.WriteFile(jtl+".log", nil, 0o600)
}

// ── tolerance values ─────────────────────────────────────────────────────────────────────────────

func TestCMP11_AChainStepOfATolerantCheckRecordsItsNumbersAndTwoOutputsWithinToleranceHashAlike(t *testing.T) {
	rowFor := func(total string) report.ScenarioResult {
		srv := chainServer(t, `{"id":"a","total":`+total+`}`, nil)
		md := cmpChainMD("CHN-T1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, tolSection)
		run := runCmp(t, &config.Config{}, map[string]string{"CHN-T1": md}, "compare", nil)
		return run.row(t, "CHN-T1")
	}
	a, b := rowFor("1.004"), rowFor("1.0")
	for _, row := range []report.ScenarioResult{a, b} {
		if row.Status != "passed" || len(row.Outputs) != 1 || row.Outputs[0].State != compare.StateRecorded {
			t.Fatalf("a tolerant check on an executor that knows Tolerance must run and record: status %q failure %+v outputs %+v", row.Status, row.Failure, row.Outputs)
		}
	}
	if len(a.Outputs[0].Values) != 1 || a.Outputs[0].Values[0] != (compare.ToleranceValue{Path: "$.total", Rule: 0, Value: 1.004}) {
		t.Fatalf("values = %+v, want [{$.total 0 1.004}]", a.Outputs[0].Values)
	}
	if a.Outputs[0].Hash != b.Outputs[0].Hash {
		t.Errorf("two outputs that differ only inside the tolerance must hash alike (the number travels in values): %s vs %s", a.Outputs[0].Hash, b.Outputs[0].Hash)
	}
	if b.Outputs[0].Values[0].Value != 1.0 {
		t.Errorf("values of the second run = %+v", b.Outputs[0].Values)
	}
}

func TestCMP11_AJMeterCheckOfATolerantRuleRecordsItsNumbers(t *testing.T) {
	r := &capRunner{t: t, bodies: []string{`{"id":"x1","total":2.5}`}}
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + tolSection}, "compare", r)
	row := run.row(t, "CMP-H1")
	if row.Status != "passed" || len(row.Outputs) != 1 || row.Outputs[0].State != compare.StateRecorded {
		t.Fatalf("status %q failure %+v outputs %+v", row.Status, row.Failure, row.Outputs)
	}
	if v := row.Outputs[0].Values; len(v) != 1 || v[0].Path != "$.total" || v[0].Value != 2.5 {
		t.Errorf("values = %+v, want [{$.total 0 2.5}]", v)
	}
}

// design 11.2 item 4: an executor that does not know a key of a newer release never runs the check with the rule ignored.
func TestCMP11_AnE1ExecutorHandedAToleranceOrABandRefusesTheCheckBeforeFiringIt(t *testing.T) {
	prev := executorCompareFloor
	executorCompareFloor = compare.FloorE1
	defer func() { executorCompareFloor = prev }()
	for _, tc := range []struct{ name, section, key string }{
		{"Tolerance", tolSection, "Tolerance"},
		{"Not Worse Than", "\n## COMPARE\n- **Reference**: property\n- **Agreement**: 100%\n- **Not Worse Than**: p95 20%\n", "Not Worse Than"},
	} {
		var hits int32
		srv := chainServer(t, `{"total":1.0}`, &hits)
		md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, tc.section+cmpLoad)
		run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, "compare", nil)
		row := run.row(t, "CHN-C1")
		if row.Status != report.StatusError || row.Failure == nil || !strings.HasPrefix(row.Failure.Observed, "refused before firing: ") ||
			!strings.Contains(row.Failure.Observed, tc.key) || row.Outputs != nil {
			t.Errorf("%s: status %q failure %+v outputs %+v", tc.name, row.Status, row.Failure, row.Outputs)
		}
		if atomic.LoadInt32(&hits) != 0 {
			t.Errorf("%s: the SUT was hit by a check an E1 executor cannot honour", tc.name)
		}
	}
}

// ── load numbers ─────────────────────────────────────────────────────────────────────────────────

func TestCMP11_ALoadCheckWithABandRecordsItsNumbersUnderLoadAndNoOutputAtAll(t *testing.T) {
	r := &loadRunner{t: t, elapsed: []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, errors: 2}
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + loadSection + cmpLoad}, "compare", r)
	row := run.row(t, "CMP-H1")
	if len(row.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want exactly ONE record", row.Outputs)
	}
	o := row.Outputs[0]
	if o.State != compare.StateNotRecorded || o.Reason != compare.ReasonLoadNumbersOnly || o.Hash != "" || o.Parts != (compare.Parts{}) || o.Status != 0 || o.BodyKind != "" {
		t.Errorf("record = %+v: a load record is not_recorded / load_numbers_only with no hash, no parts, no status, no body kind", o)
	}
	if o.Load == nil || o.Load.Samples != 10 || o.Load.P50Ms != 50 || o.Load.P95Ms != 100 || o.Load.P99Ms != 100 || o.Load.ErrorRate != 0.2 {
		t.Fatalf("load = %+v, want samples 10, p50 50, p95 100, p99 100, error_rate 0.2 (the numbers report.LoadStats computed)", o.Load)
	}
	if row.Load == nil || row.Load.P95Ms != 100 || row.Load.Samples != 10 {
		t.Errorf("the run's own load stats moved: %+v", row.Load)
	}
	// design 1.3: the body capture stays OFF for a ## LOAD check
	if len(r.props) != 1 || r.props[0]["output.capture.dir"] != "" {
		t.Errorf("the body-capture property was set for a load check: %v", r.props)
	}
	if _, err := os.Stat(filepath.Join(run.resultsDir, "outputs")); !os.IsNotExist(err) {
		t.Errorf("a load record stores no file: outputs dir exists (%v)", err)
	}
}

func TestCMP11_TheAbsoluteLoadThresholdsStillFailTheRunOnTheirOwn(t *testing.T) {
	r := &loadRunner{t: t, elapsed: []int{900, 900, 900, 900, 900}}
	md := cmpHTTPMD + loadSection + cmpLoad // Target P95 Ms: 500
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": md}, "compare", r)
	row := run.row(t, "CMP-H1")
	if row.Status != "failed" || row.Load == nil || !row.Load.Breached {
		t.Fatalf("status %q load %+v: the declared p95 target must still fail the check", row.Status, row.Load)
	}
	if len(row.Outputs) != 1 || row.Outputs[0].Load == nil || row.Outputs[0].Load.P95Ms != 900 {
		t.Errorf("a failed load check still records its numbers: %+v", row.Outputs)
	}
}

func TestCMP11_ALoadCheckWithoutABandOrOutsideModeCompareRecordsNothing(t *testing.T) {
	for _, tc := range []struct{ name, md, mode string }{
		{"no Not Worse Than", cmpHTTPMD + cmpSection + cmpLoad, "compare"},
		{"build", cmpHTTPMD + loadSection + cmpLoad, "build"},
		{"final", cmpHTTPMD + loadSection + cmpLoad, "final"},
		{"no mode", cmpHTTPMD + loadSection + cmpLoad, ""},
		{"no COMPARE", cmpHTTPMD + cmpLoad, "compare"},
	} {
		r := &loadRunner{t: t, elapsed: []int{10, 20, 30}}
		run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": tc.md}, tc.mode, r)
		if row := run.row(t, "CMP-H1"); row.Outputs != nil {
			t.Errorf("%s: recorded %+v", tc.name, row.Outputs)
		}
	}
}

// ── bytes unchanged in every mode but compare ────────────────────────────────────────────────────

// normalizedRow is a row's JSON with the clock and the random parts of an id removed, so two runs of the same check
// can be compared byte for byte.
func normalizedRow(t *testing.T, row report.ScenarioResult) string {
	t.Helper()
	row.DurationMs, row.CorrelationID = 0, ""
	row.WindowStart, row.WindowEnd = time.Time{}, time.Time{}
	row.Requests = nil
	steps := append([]report.StepResult(nil), row.Steps...)
	for i := range steps {
		steps[i].CorrelationID = ""
	}
	row.Steps = steps
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCMP11_InEveryModeButCompareTheBytesAndTheEvidenceHashOfATolerantOrBandedCheckAreWhatTheyWereWithoutTheSection(t *testing.T) {
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", "ci", "unknown", ""} {
		// a chain check with Tolerance
		srv := chainServer(t, `{"id":"a","total":1.004}`, nil)
		trig := strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL)
		with := runCmp(t, &config.Config{}, map[string]string{"CHN-T1": cmpChainMD("CHN-T1", trig, oneStepExpect, tolSection)}, mode, nil).row(t, "CHN-T1")
		without := runCmp(t, &config.Config{}, map[string]string{"CHN-T1": cmpChainMD("CHN-T1", trig, oneStepExpect, "")}, mode, nil).row(t, "CHN-T1")
		// a load check with Not Worse Than
		lr := func() Runner { return &loadRunner{t: t, elapsed: []int{10, 20, 30, 40}} }
		lwith := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + loadSection + cmpLoad}, mode, lr()).row(t, "CMP-H1")
		lwithout := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + cmpLoad}, mode, lr()).row(t, "CMP-H1")
		for name, pair := range map[string][2]report.ScenarioResult{"Tolerance": {with, without}, "Not Worse Than": {lwith, lwithout}} {
			a, b := normalizedRow(t, pair[0]), normalizedRow(t, pair[1])
			ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
			if a != b {
				t.Errorf("mode %q, %s: the row's bytes changed by declaring the key:\n with    %s\n without %s", mode, name, a, b)
			}
			if hex.EncodeToString(ha[:]) != hex.EncodeToString(hb[:]) {
				t.Errorf("mode %q, %s: the evidence hash changed", mode, name)
			}
			for _, bad := range []string{`"outputs"`, `"values"`, `"load":{"samples"`} {
				if bad == `"load":{"samples"` && name == "Not Worse Than" {
					continue // the run's own load stats are in every load row, with or without the key
				}
				if strings.Contains(a, bad) {
					t.Errorf("mode %q, %s: the row carries %s", mode, name, bad)
				}
			}
		}
	}
}

// ── the target of a member ───────────────────────────────────────────────────────────────────────

func targetsCfg() *config.Config {
	c := httpCfg()
	c.Targets.HTTPTargets = map[string]*config.HTTPTarget{
		"graph": {BaseURL: "http://graph.invalid:9000"},
		"other": {BaseURL: "http://other.invalid:9100"},
	}
	return c
}

func hostOf(p map[string]string) string { return p["http.host"] + ":" + p["http.port"] }

func TestCMP11_ACompareTargetIsUsedForACheckWithNoTargetOfItsOwn(t *testing.T) {
	r := &capRunner{t: t}
	runCmpTarget(t, targetsCfg(), map[string]string{"CMP-H1": cmpHTTPMD}, "compare", "graph", r)
	if len(r.props) != 1 || hostOf(r.props[0]) != "graph.invalid:9000" {
		t.Fatalf("props %v: the check must run against the named target graph.invalid:9000", r.props)
	}
	// with no target the plain slot is used, as before
	r = &capRunner{t: t}
	runCmpTarget(t, targetsCfg(), map[string]string{"CMP-H1": cmpHTTPMD}, "compare", "", r)
	if len(r.props) != 1 || hostOf(r.props[0]) != "sut.invalid:8080" {
		t.Fatalf("props %v: no compare target must leave the default target", r.props)
	}
}

func TestCMP11_ACompareTargetNeverOverridesACheckOwnTarget(t *testing.T) {
	own := strings.Replace(cmpHTTPMD, "- **Layer**: HTTP Ingestion\n", "- **Layer**: HTTP Ingestion\n- **Target**: other\n", 1)
	own = strings.Replace(own, "CMP-H1", "CMP-H2", 1)
	r := &capRunner{t: t}
	runCmpTarget(t, targetsCfg(), map[string]string{"CMP-H1": cmpHTTPMD, "CMP-H2": own}, "compare", "graph", r)
	if len(r.props) != 2 {
		t.Fatalf("the runner was called %d times, want 2", len(r.props))
	}
	got := map[string]string{}
	for _, p := range r.props {
		got[p["scenario.id"]] = hostOf(p)
	}
	if got["CMP-H1"] != "graph.invalid:9000" || got["CMP-H2"] != "other.invalid:9100" {
		t.Errorf("hosts by check = %v, want CMP-H1 on graph (the member's target) and CMP-H2 on other (its own)", got)
	}
}

func TestCMP11_ATargetThatIsNotDeclaredFailsTheWholeRunByNameBeforeAnyCheckFiresAndNeverFallsBack(t *testing.T) {
	r := &capRunner{t: t}
	run := runCmpTarget(t, targetsCfg(), map[string]string{"CMP-H1": cmpHTTPMD, "CMP-H2": strings.Replace(cmpHTTPMD, "CMP-H1", "CMP-H2", 1)}, "compare", "nope", r)
	if len(r.props) != 0 {
		t.Fatalf("the runner fired %d check(s) against %v: an undeclared target must stop the run before any check", len(r.props), r.props)
	}
	for _, id := range []string{"CMP-H1", "CMP-H2"} {
		row := run.row(t, id)
		if row.Status != report.StatusError || row.Failure == nil ||
			!strings.Contains(row.Failure.Observed, `this environment declares no connection target named "nope"`) {
			t.Errorf("%s: status %q failure %+v, want error naming the target", id, row.Status, row.Failure)
		}
	}
}

func TestCMP11_ACompareTargetIsReadOnlyInModeCompare(t *testing.T) {
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", ""} {
		r := &capRunner{t: t}
		runCmpTarget(t, targetsCfg(), map[string]string{"CMP-H1": cmpHTTPMD}, mode, "graph", r)
		if len(r.props) != 1 || hostOf(r.props[0]) != "sut.invalid:8080" {
			t.Errorf("mode %q: props %v: only a compare run reads the member's target", mode, r.props)
		}
	}
}
