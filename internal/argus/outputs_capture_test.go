package argus

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ARGUS-CMP-3 (/-05): in run mode `compare`, and only there, a check that declares
// `## COMPARE` records the output it judged. These tests drive the REAL run loop (RunAllWithMode) over
// real scenario files; the SUT is an httptest server (chain http steps) or a fake JMeter Runner that
// writes the side files the template's JSR223 block writes (the JMX itself is not run here).

const cmpSection = "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n- **Mask**: $.id\n"

func cmpChainMD(id, trigger, expect, compareBlock string) string {
	return strings.Join([]string{
		"# Scenario: c", "", "## Metadata", "- **ID**: " + id, "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json", trigger, "```", "",
		"## EXPECT", expect, "## TIMEOUT", "60s", "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", compareBlock, "",
	}, "\n")
}

const cmpLoad = "\n## LOAD\n- **Users**: 2\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 5\n- **Target P95 Ms**: 500\n- **Max Error Rate**: 0.1\n"

const cmpHTTPMD = "# Scenario: h\n\n## Metadata\n- **ID**: CMP-H1\n- **Layer**: HTTP Ingestion\n\n## TRIGGER\nPOST `/api/v1/orders`\n\n" +
	"## EXPECT\n### Runnable\n- status=200\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — a unit-test fixture; it creates nothing.\n"

type cmpRun struct {
	rr         *RunResult
	resultsDir string
	runID      string
}

func (r cmpRun) row(t *testing.T, id string) report.ScenarioResult {
	t.Helper()
	for _, l := range r.rr.Report.Layers {
		for _, s := range l.Scenarios {
			if s.ID == id {
				return s
			}
		}
	}
	t.Fatalf("no row for %s", id)
	return report.ScenarioResult{}
}

func runCmp(t *testing.T, c *config.Config, files map[string]string, mode string, r Runner) cmpRun {
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
	rr, err := RunAllWithMode(c, sc, resultsDir, "p", "", "", "", "run-cmp3", r, nil, mode)
	if err != nil {
		t.Fatal(err)
	}
	return cmpRun{rr: rr, resultsDir: resultsDir, runID: "run-cmp3"}
}

func outputFilePath(r cmpRun, name string) string {
	return filepath.Join(r.resultsDir, "outputs", r.runID, name)
}

// capRunner is a fake JMeter: it records the props it was given and writes a .jtl plus, when the
// capture property is set, the side files the http templates' JSR223 block writes.
type capRunner struct {
	t       *testing.T
	props   []map[string]string
	status  int
	headers string
	bodies  []string
	log     string
}

func (r *capRunner) Run(base, jtl string, props map[string]string, _ time.Duration) error {
	cp := map[string]string{}
	for k, v := range props {
		cp[k] = v
	}
	r.props = append(r.props, cp)
	status := r.status
	if status == 0 {
		status = 200
	}
	rows := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"
	bodies := r.bodies
	if len(bodies) == 0 {
		bodies = []string{`{"id":"x1","n":1}`}
	}
	for i := range bodies {
		rows += "1,5,CMP-H1," + strconv.Itoa(status) + ",OK,t,true,\n"
		if dir := props["output.capture.dir"]; dir != "" {
			n := strconv.Itoa(i + 1)
			hdr := r.headers
			if hdr == "" {
				hdr = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
			}
			for suffix, data := range map[string]string{".status": strconv.Itoa(status), ".headers": hdr, ".body": bodies[i]} {
				if err := os.WriteFile(filepath.Join(dir, n+suffix), []byte(data), 0o600); err != nil {
					return err
				}
			}
		}
	}
	if err := os.WriteFile(jtl, []byte(rows), 0o600); err != nil {
		return err
	}
	return os.WriteFile(jtl+".log", []byte(r.log), 0o600)
}

func httpCfg() *config.Config {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid:8080"}
	return c
}

func chainServer(t *testing.T, body string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const oneStepTrigger = `{"steps":[{"type":"http","name":"create","method":"POST","url":"BASE/things"}]}`
const oneStepExpect = "### Runnable\n- step create: status=200\n"

func TestCompareMode_ChainHTTPStepRecordsDigestAndStoredFile(t *testing.T) {
	srv := chainServer(t, `{"id":"ws-77","n":1}`, nil)
	md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, cmpSection)
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, "compare", nil)
	row := run.row(t, "CHN-C1")
	if row.Status != "passed" {
		t.Fatalf("status %q %+v", row.Status, row.Failure)
	}
	if len(row.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want one record", row.Outputs)
	}
	o := row.Outputs[0]
	if o.State != compare.StateRecorded || o.Step != "create" || o.Sample != 1 || o.Status != 200 || len(o.Hash) != 64 || o.BodyKind != compare.KindJSON || o.MasksApplied != 1 {
		t.Fatalf("record %+v", o)
	}
	p := outputFilePath(run, "CHN-C1__create.1.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("stored file mode %v", st.Mode().Perm())
	}
	if ds, _ := os.Stat(filepath.Dir(p)); ds == nil || ds.Mode().Perm()&0o077 != 0 {
		t.Errorf("outputs/<run> directory is readable by group or world")
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"id":"<masked>"`) || strings.Contains(string(b), "ws-77") {
		t.Errorf("the stored body must be canonical and masked: %s", b)
	}
	if !json.Valid(b) {
		t.Errorf("stored file is not JSON: %s", b)
	}
	// the digest in the row is the digest BuildRecord gives the same response: one canonicaliser
	spec := (&compare.Rules{Reference: compare.RefMeasured, Output: compare.OutputSel{Status: true, Body: true}, Mask: []string{"$.id"}}).Spec()
	want, _ := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(`{"id":"ws-77","n":1}`)}, spec, "create", 1)
	if o.Hash != want.Hash {
		t.Errorf("hash %s differs from compare.BuildRecord's %s", o.Hash, want.Hash)
	}
	rb, _ := json.Marshal(row)
	if strings.Contains(string(rb), "ws-77") {
		t.Errorf("the row (report.json) carries the body: %s", rb)
	}
}

func TestCompareMode_OnlyModeCompareRecords(t *testing.T) {
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", "ci", "unknown", ""} {
		srv := chainServer(t, `{"id":"a"}`, nil)
		md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, cmpSection)
		run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, mode, nil)
		row := run.row(t, "CHN-C1")
		if row.Status != "passed" {
			t.Fatalf("mode %q: status %q", mode, row.Status)
		}
		if row.Outputs != nil {
			t.Errorf("mode %q recorded outputs: %+v", mode, row.Outputs)
		}
		if _, err := os.Stat(filepath.Join(run.resultsDir, "outputs")); !os.IsNotExist(err) {
			t.Errorf("mode %q created an outputs directory (%v)", mode, err)
		}
	}
}

func TestCompareMode_CheckWithoutCompareSectionRecordsNothing(t *testing.T) {
	srv := chainServer(t, `{"id":"a"}`, nil)
	md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, "")
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, "compare", nil)
	if row := run.row(t, "CHN-C1"); row.Outputs != nil {
		t.Errorf("a check with no ## COMPARE recorded %+v", row.Outputs)
	}
	if _, err := os.Stat(filepath.Join(run.resultsDir, "outputs")); !os.IsNotExist(err) {
		t.Errorf("an outputs directory exists (%v)", err)
	}
}

func TestCompareMode_StepsRuleDefaultsToEveryNonAlwaysHTTPStep(t *testing.T) {
	srv := chainServer(t, `{"ok":true}`, nil)
	trig := strings.ReplaceAll(`{"steps":[
		{"type":"http","name":"create","method":"POST","url":"BASE/a"},
		{"type":"http","name":"read","method":"GET","url":"BASE/b"},
		{"type":"http","name":"cleanup","method":"DELETE","url":"BASE/c","always":true}]}`, "BASE", srv.URL)
	expect := "### Runnable\n- step create: status=200\n- step read: status=200\n- step cleanup: status=200\n"
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, cmpSection)}, "compare", nil)
	var steps []string
	for _, o := range run.row(t, "CHN-C1").Outputs {
		steps = append(steps, o.Step)
	}
	if strings.Join(steps, ",") != "create,read" {
		t.Errorf("default Steps recorded %v, want create,read (never the always step)", steps)
	}

	named := cmpSection + "- **Steps**: read\n"
	run = runCmp(t, &config.Config{}, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, named)}, "compare", nil)
	steps = nil
	for _, o := range run.row(t, "CHN-C1").Outputs {
		steps = append(steps, o.Step)
	}
	if strings.Join(steps, ",") != "read" {
		t.Errorf("**Steps**: read recorded %v", steps)
	}
}

func TestCompareMode_PollRecordsTheLastAttemptOnly(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		st := "pending"
		if c >= 3 {
			st = "running"
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"state":"` + st + `"}`))
	}))
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"poll","method":"GET","url":"` + srv.URL + `/w","poll":{"timeout":"2s","interval":"10ms"}}]}`
	expect := "### Runnable\n- step poll: status=200\n- step poll: body has state containing running\n"
	block := "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, block)}, "compare", nil)
	row := run.row(t, "CHN-C1")
	if len(row.Outputs) != 1 || atomic.LoadInt32(&n) != 3 {
		t.Fatalf("outputs %d, attempts %d: want one record for 3 attempts", len(row.Outputs), n)
	}
	b, _ := os.ReadFile(outputFilePath(run, "CHN-C1__poll.1.json"))
	if !strings.Contains(string(b), "running") || strings.Contains(string(b), "pending") {
		t.Errorf("the stored output is not the judged (last) attempt: %s", b)
	}
}

func TestCompareMode_ScrubRemovesSavedValuesConfigCredentialsAndTokens(t *testing.T) {
	const savedTok = "Zq9-saved-value-8841"
	const cfgSecret = "hunter2-config-password"
	const bearer = "bearer-secret-from-config-77"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = w.Write([]byte(`{"tok":"` + savedTok + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"echo":"` + savedTok + `","pw":"` + cfgSecret + `","b":"` + bearer + `","jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz","keep":"hello"}`))
	}))
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"login","method":"POST","url":"` + srv.URL + `/l","save":{"tok":"tok"}},` +
		`{"type":"http","name":"read","method":"GET","url":"` + srv.URL + `/r"}]}`
	expect := "### Runnable\n- step login: status=200\n- step read: status=200\n"
	block := "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"
	c := &config.Config{}
	c.Targets.Database = &config.DBTarget{Password: cfgSecret}
	c.Targets.Auth = &config.AuthTarget{BearerToken: bearer}
	run := runCmp(t, c, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, block)}, "compare", nil)
	got := ""
	for _, f := range []string{"CHN-C1__login.1.json", "CHN-C1__read.1.json"} {
		b, err := os.ReadFile(outputFilePath(run, f))
		if err != nil {
			t.Fatal(err)
		}
		got += string(b)
	}
	for _, leak := range []string{savedTok, cfgSecret, bearer, "dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz"} {
		if strings.Contains(got, leak) {
			t.Errorf("stored output still carries %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "${saved.tok}") || !strings.Contains(got, `"keep":"hello"`) {
		t.Errorf("scrub must put the saved placeholder back and leave ordinary text alone:\n%s", got)
	}
	// the hash is over the scrubbed form: scrubbing is before hashing, so equal secrets hash equal
	if o := run.row(t, "CHN-C1").Outputs; len(o) != 2 || o[0].Hash == "" {
		t.Fatalf("records %+v", o)
	}
}

func TestCompareMode_JMeterPathScrubsConfigCredentialsAndTokensToo(t *testing.T) {
	const pw = "hunter2-config-password"
	c := httpCfg()
	c.Targets.Database = &config.DBTarget{Password: pw}
	r := &capRunner{t: t, bodies: []string{`{"echo":"` + pw + `","jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz","keep":"hello"}`}}
	run := runCmp(t, c, map[string]string{"CMP-H1": cmpHTTPMD + "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"}, "compare", r)
	b, err := os.ReadFile(outputFilePath(run, "CMP-H1.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), pw) || strings.Contains(string(b), "dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz") || !strings.Contains(string(b), `"keep":"hello"`) {
		t.Errorf("the JMeter path did not scrub (or scrubbed ordinary text): %s", b)
	}
	// and the digest is over the scrubbed form: the same response with a different password hashes alike
	r2 := &capRunner{t: t, bodies: []string{`{"echo":"another-password-entirely","jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1rwXyz","keep":"hello"}`}}
	c2 := httpCfg()
	c2.Targets.Database = &config.DBTarget{Password: "another-password-entirely"}
	run2 := runCmp(t, c2, map[string]string{"CMP-H1": cmpHTTPMD + "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"}, "compare", r2)
	if run.row(t, "CMP-H1").Outputs[0].Hash != run2.row(t, "CMP-H1").Outputs[0].Hash {
		t.Errorf("two members that differ only in their own credential must hash alike once scrubbed")
	}
}

func TestCompareMode_ProblemInTheSectionIsRefusedBeforeFiring(t *testing.T) {
	var hits int32
	srv := chainServer(t, `{"id":"a"}`, &hits)
	bad := "\n## COMPARE\n- **Reference**: measured\n- **Bogus**: 1\n"
	md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, bad)
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, "compare", nil)
	row := run.row(t, "CHN-C1")
	if row.Status != report.StatusError || row.Failure == nil || !strings.HasPrefix(row.Failure.Observed, "refused before firing: ") {
		t.Fatalf("status %q failure %+v", row.Status, row.Failure)
	}
	if !strings.Contains(row.Failure.Observed, "Bogus") {
		t.Errorf("the refusal must name the key: %q", row.Failure.Observed)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("the SUT was hit %d time(s) by a refused check", hits)
	}
	// every other mode runs it exactly as before: the section is inert
	for _, mode := range []string{"build", "final", ""} {
		atomic.StoreInt32(&hits, 0)
		run = runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, mode, nil)
		if row := run.row(t, "CHN-C1"); row.Status != "passed" || atomic.LoadInt32(&hits) != 1 {
			t.Errorf("mode %q: status %q hits %d, want passed/1", mode, row.Status, hits)
		}
	}
}

func TestCompareMode_AKeyThisExecutorDoesNotKnowIsRefused(t *testing.T) {
	// ARGUS-CMP-11: this executor now IS E2 and knows Tolerance, so the executor of the release before (E1) stands in for
	// "an executor that does not know the key" (TestCMP11_AnE1ExecutorHandedAToleranceOrABandRefusesTheCheckBeforeFiringIt
	// holds the same for both E2 keys).
	prev := executorCompareFloor
	executorCompareFloor = compare.FloorE1
	defer func() { executorCompareFloor = prev }()
	var hits int32
	srv := chainServer(t, `{"total":1.0}`, &hits)
	e2 := cmpSection + "- **Tolerance**: $.total abs 0.01\n"
	md := cmpChainMD("CHN-C1", strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL), oneStepExpect, e2)
	run := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": md}, "compare", nil)
	row := run.row(t, "CHN-C1")
	if row.Status != report.StatusError || row.Failure == nil || !strings.HasPrefix(row.Failure.Observed, "refused before firing: ") ||
		!strings.Contains(row.Failure.Observed, "does not know") || !strings.Contains(row.Failure.Observed, "Tolerance") {
		t.Fatalf("status %q failure %+v", row.Status, row.Failure)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("the SUT was hit by a refused check")
	}
}

// ── JMeter capture ────────────────────────────────────────────────────────────────────────────

func TestCompareMode_JMeterCaptureReadsSideFilesBuildsRecordsAndDeletesRawFiles(t *testing.T) {
	r := &capRunner{t: t, bodies: []string{`{"id":"x1","n":1}`, `{"id":"x2","n":2}`}}
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + cmpSection}, "compare", r)
	row := run.row(t, "CMP-H1")
	if row.Status != "passed" {
		t.Fatalf("status %q %+v", row.Status, row.Failure)
	}
	if len(r.props) != 1 || r.props[0]["output.capture.dir"] == "" {
		t.Fatalf("the capture property was not set for a compare run: %v", r.props)
	}
	if len(row.Outputs) != 2 || row.Outputs[0].Sample != 1 || row.Outputs[1].Sample != 2 || row.Outputs[0].State != compare.StateRecorded {
		t.Fatalf("outputs %+v", row.Outputs)
	}
	if row.Outputs[0].Step != "" {
		t.Errorf("a JMeter sample has no step: %+v", row.Outputs[0])
	}
	spec := (&compare.Rules{Reference: compare.RefMeasured, Output: compare.OutputSel{Status: true, Body: true}, Mask: []string{"$.id"}}).Spec()
	want, _ := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(`{"id":"x1","n":1}`)}, spec, "", 1)
	if row.Outputs[0].Hash != want.Hash {
		t.Errorf("JMeter-path hash %s differs from BuildRecord %s", row.Outputs[0].Hash, want.Hash)
	}
	for _, f := range []string{"CMP-H1.1.json", "CMP-H1.2.json"} {
		if _, err := os.Stat(outputFilePath(run, f)); err != nil {
			t.Errorf("stored file %s: %v", f, err)
		}
	}
	// the raw side files are gone on the success path
	if left := leftoverRawFiles(t, run.resultsDir); len(left) != 0 {
		t.Errorf("raw capture files left behind: %v", left)
	}
}

func TestCompareMode_JMeterRawFilesAreDeletedWhenTheRunFails(t *testing.T) {
	r := &failingCapRunner{capRunner: capRunner{t: t}}
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + cmpSection}, "compare", r)
	if left := leftoverRawFiles(t, run.resultsDir); len(left) != 0 {
		t.Errorf("raw capture files left behind after a failed run: %v", left)
	}
	if !r.wrote {
		t.Fatal("the fake runner never wrote a capture file, the test proves nothing")
	}
}

type failingCapRunner struct {
	capRunner
	wrote bool
}

func (r *failingCapRunner) Run(base, jtl string, props map[string]string, d time.Duration) error {
	_ = r.capRunner.Run(base, jtl, props, d)
	r.wrote = props["output.capture.dir"] != ""
	return os.ErrDeadlineExceeded // the run itself failed after the side files were written
}

func leftoverRawFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		for _, suf := range []string{".status", ".headers", ".body"} {
			if strings.HasSuffix(p, suf) {
				out = append(out, p)
			}
		}
		return nil
	})
	return out
}

func TestCompareMode_JMeterNoCapturePropertyOutsideCompareOrWithoutCompareOrWithLoad(t *testing.T) {
	cases := []struct {
		name, md, mode string
		want           bool
	}{
		{"compare + COMPARE", cmpHTTPMD + cmpSection, "compare", true},
		{"build + COMPARE", cmpHTTPMD + cmpSection, "build", false},
		{"final + COMPARE", cmpHTTPMD + cmpSection, "final", false},
		{"no mode + COMPARE", cmpHTTPMD + cmpSection, "", false},
		{"compare, no COMPARE", cmpHTTPMD, "compare", false},
		{"compare + COMPARE + LOAD", cmpHTTPMD + cmpSection + cmpLoad, "compare", false},
	}
	for _, tc := range cases {
		r := &capRunner{t: t}
		run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": tc.md}, tc.mode, r)
		if len(r.props) == 0 {
			t.Errorf("%s: the runner was never called", tc.name)
			continue
		}
		got := r.props[0]["output.capture.dir"] != ""
		if got != tc.want {
			t.Errorf("%s: output.capture.dir set = %v, want %v", tc.name, got, tc.want)
		}
		if !tc.want {
			if row := run.row(t, "CMP-H1"); row.Outputs != nil {
				t.Errorf("%s: recorded %+v", tc.name, row.Outputs)
			}
		}
	}
}

func TestCompareMode_ALayerThatRecordsNothingYetSaysNotRecorded(t *testing.T) {
	md := strings.Replace(cmpHTTPMD, "HTTP Ingestion", "Database State", 1) + cmpSection
	run := runCmp(t, httpCfg(), map[string]string{"CMP-H1": md}, "compare", &capRunner{t: t})
	row := run.row(t, "CMP-H1")
	if len(row.Outputs) != 1 || row.Outputs[0].State != compare.StateNotRecorded || row.Outputs[0].Reason != compare.ReasonLayerNotSupported || row.Outputs[0].Hash != "" {
		t.Fatalf("outputs %+v: a layer with no capture must say not_recorded/layer_not_supported, never a hash", row.Outputs)
	}
}

func mustParse(t *testing.T, md string) *scenario.Scenario {
	t.Helper()
	s := scenario.Parse(md)
	if s == nil {
		t.Fatal("fixture did not parse")
	}
	return s
}

func TestDerivePropsWithCapture_SetsThePropertyOnlyWhereTheContractSaysSo(t *testing.T) {
	c := httpCfg()
	s := mustParse(t, cmpHTTPMD+cmpSection)
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", "", "ci"} {
		p, err := DerivePropsWithCapture(c, s, "tr-x", mode, "/cap")
		if err != nil || p["output.capture.dir"] != "" {
			t.Errorf("mode %q: dir %q err %v", mode, p["output.capture.dir"], err)
		}
	}
	p, err := DerivePropsWithCapture(c, s, "tr-x", "compare", "/cap")
	if err != nil || p["output.capture.dir"] != "/cap" {
		t.Errorf("compare: dir %q err %v", p["output.capture.dir"], err)
	}
	plain := mustParse(t, cmpHTTPMD)
	if p, _ := DerivePropsWithCapture(c, plain, "tr-x", "compare", "/cap"); p["output.capture.dir"] != "" {
		t.Errorf("a check with no ## COMPARE got the property")
	}
	load := mustParse(t, cmpHTTPMD+cmpSection+cmpLoad)
	if load.Load == nil {
		t.Fatal("fixture: LOAD did not parse")
	}
	if p, _ := DerivePropsWithCapture(c, load, "tr-x", "compare", "/cap"); p["output.capture.dir"] != "" {
		t.Errorf("a check declaring ## LOAD got the property")
	}
	if p, _ := DeriveProps(c, s, "tr-x"); p["output.capture.dir"] != "" {
		t.Errorf("plain DeriveProps must never set it")
	}
}

// ── the canary (design 1.5: never logged) ─────────────────────────────────────────────────────

func TestCanary_ABodyAndAnUndeclaredHeaderReachTheOutputFileOnly(t *testing.T) {
	const canary = "zebra-quartz-meadow-lantern"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Leak", canary)
		_, _ = w.Write([]byte(`{"note":"` + canary + `"}`))
	}))
	defer srv.Close()

	var logs bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prevLog)
	stdout := captureStd(t)

	block := "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"
	trig := strings.ReplaceAll(oneStepTrigger, "BASE", srv.URL)
	chainRun := runCmp(t, &config.Config{}, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, oneStepExpect, block)}, "compare", nil)
	jm := &capRunner{t: t, bodies: []string{`{"note":"` + canary + `"}`}, headers: "HTTP/1.1 200 OK\r\nX-Leak: " + canary + "\r\n"}
	jmRun := runCmp(t, httpCfg(), map[string]string{"CMP-H1": cmpHTTPMD + block}, "compare", jm)

	outs := stdout()
	if strings.Contains(outs, canary) {
		t.Errorf("the canary reached stdout/stderr: %q", outs)
	}
	if strings.Contains(logs.String(), canary) {
		t.Errorf("the canary reached the logger: %q", logs.String())
	}
	found := 0
	for _, run := range []cmpRun{chainRun, jmRun} {
		rb, _ := json.Marshal(run.rr.Report)
		if strings.Contains(string(rb), canary) {
			t.Errorf("the canary reached report.json: %s", rb)
		}
		_ = filepath.Walk(filepath.Dir(run.resultsDir), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(p)
			if !strings.Contains(string(b), canary) {
				return nil
			}
			rel, _ := filepath.Rel(run.resultsDir, p)
			if !strings.HasPrefix(rel, "outputs"+string(filepath.Separator)) {
				t.Errorf("the canary is in %s (not an output file)", rel)
			} else {
				found++
				if strings.Contains(string(b), "X-Leak") || strings.Contains(strings.ToLower(string(b)), "x-leak") {
					t.Errorf("an undeclared header is in the output file: %s", b)
				}
			}
			return nil
		})
	}
	if found != 2 {
		t.Errorf("the canary body is expected in exactly the 2 output files, found in %d", found)
	}
	t.Logf("CANARY %q: output files %d; logger %d bytes, stdout/stderr %d bytes, .jtl and .jtl.log and report.json: none", canary, found, logs.Len(), len(outs))
}

func captureStd(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	restored := false
	restore := func() {
		if !restored {
			restored = true
			os.Stdout, os.Stderr = oldOut, oldErr
			_ = w.Close()
			<-done
		}
	}
	t.Cleanup(restore)
	return func() string { restore(); return buf.String() }
}
