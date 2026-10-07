package argus

// / — a failed HTTP body check names WHICH `body …` bullet failed, to
// the TEST hat only. a tester (Documenso run 2026-10-02): "A failed content check does not say which check
// failed." The template already knew; it wrote a message the runner threw away.
//
// Custody (VR-C8): `observed` is shown to both hats and stays the fixed bodyAssertObserved sentence,
// byte for byte. The failing bullet rides report.Failure.FailedBodyCheck, built from a STRUCTURED index
// the template emits (`[argus-body-check=N]`), mapped back to the bullet from the parsed scenario —
// never from the template's free text (a name parsed out of a value string carries the value).
//
// The template half runs the REAL JSR223 script extracted from templates/http-ingestion.jmx under
// Groovy (the body_equals_template_test.go technique); the run half feeds RunAll a runner that runs
// that same script with the props RunAll itself derived, so the whole path is real except JMeter's
// HTTP sampler.

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

const (
	fbcFieldA = "alphaFieldQ1"
	fbcFieldB = "bravoFieldQ2"
	fbcFieldC = "charlieFieldQ3"
	// the SUT's real values
	fbcGotA = "real-a-7Hk"
	fbcGotB = "real-b-8Jm"
	fbcGotC = "real-c-9Ln"
	// the planted wrong expectation
	fbcWrong = "EXPECTEDSENTINEL-4Zq"
)

// runBodyAssertProps runs the template's body-assertion script with the given props against body.
func runBodyAssertProps(t *testing.T, props map[string]string, body string) bodyAssertVerdict {
	t.Helper()
	cp := groovyClasspath(t)
	in, _ := json.Marshal(map[string]any{"script": httpIngestionBodyAssertScript(t), "props": props, "body": body, "code": "200"})
	harness, _ := filepath.Abs("testdata/body_assert_harness.groovy")
	cmd := exec.Command("java", "-cp", cp, "groovy.ui.GroovyMain", harness)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("groovy harness failed: %v\n%s\n%s", err, out.String(), errb.String())
	}
	last := ""
	for _, ln := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			last = ln
		}
	}
	var v bodyAssertVerdict
	if err := json.Unmarshal([]byte(last), &v); err != nil {
		t.Fatalf("harness output unreadable: %v\n%s\n%s", err, out.String(), errb.String())
	}
	return v
}

func fbcBody() string {
	return fmt.Sprintf(`{%q:%q,%q:%q,%q:%q}`, fbcFieldA, fbcGotA, fbcFieldB, fbcGotB, fbcFieldC, fbcGotC)
}

// threeEquals is the numbered prop set for three `equals` checks; the one at wrong (1-based) expects
// fbcWrong, the others expect what the SUT really answers.
func threeEquals(wrong int) map[string]string {
	fields := []string{fbcFieldA, fbcFieldB, fbcFieldC}
	gots := []string{fbcGotA, fbcGotB, fbcGotC}
	p := map[string]string{"expect.body.count": "3"}
	for i := 1; i <= 3; i++ {
		n := fmt.Sprint(i)
		v := gots[i-1]
		if i == wrong {
			v = fbcWrong
		}
		p["expect.body."+n+".field"] = fields[i-1]
		p["expect.body."+n+".op"] = "equals"
		p["expect.body."+n+".value"] = v
	}
	return p
}

// assertNoValueInMessage: the template's message must still never carry an asserted value, a field
// name or the body (VR-C8); the new token is an index and nothing else.
func assertNoValueInMessage(t *testing.T, msg string) {
	t.Helper()
	for _, leak := range []string{fbcWrong, fbcFieldA, fbcFieldB, fbcFieldC, fbcGotA, fbcGotB, fbcGotC} {
		if strings.Contains(msg, leak) {
			t.Errorf("the template's failure message carries %q: %q", leak, msg)
		}
	}
}

// Plant ONE wrong check per run, at each position in turn: the template names that position, and only it.
func TestHTTPIngestionTemplate_NamesTheFailingBodyCheck(t *testing.T) {
	for wrong := 1; wrong <= 3; wrong++ {
		t.Run(fmt.Sprintf("check %d wrong", wrong), func(t *testing.T) {
			v := runBodyAssertProps(t, threeEquals(wrong), fbcBody())
			if !v.Failure || !strings.Contains(v.Message, "BODY-ASSERT-FAIL") {
				t.Fatalf("want a BODY-ASSERT-FAIL failure, got failure=%v %q", v.Failure, v.Message)
			}
			want := fmt.Sprintf("[argus-body-check=%d]", wrong)
			if !strings.HasSuffix(v.Message, want) {
				t.Errorf("the failure must END with %q, got %q", want, v.Message)
			}
			for other := 1; other <= 3; other++ {
				if other != wrong && strings.Contains(v.Message, fmt.Sprintf("argus-body-check=%d", other)) {
					t.Errorf("check %d failed but the message names check %d: %q", wrong, other, v.Message)
				}
			}
			assertNoValueInMessage(t, v.Message)
		})
	}
	// all three hold: no failure, no token
	p := threeEquals(0)
	if v := runBodyAssertProps(t, p, fbcBody()); v.Failure || strings.Contains(v.Message, "argus-body-check") {
		t.Errorf("three holding checks must pass with no token, got failure=%v %q", v.Failure, v.Message)
	}
}

// Every failing branch of the numbered loop carries the index: field absent, non-JSON answer, unknown op.
func TestHTTPIngestionTemplate_EveryFailingBranchCarriesTheIndex(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]string
		body  string
	}{
		{"field absent at check 2", func() map[string]string {
			p := threeEquals(0)
			p["expect.body.2.field"] = "missingFieldZ9"
			return p
		}(), fbcBody()},
		{"non-JSON answer, first scoped check is 2", map[string]string{
			"expect.body.count":   "2",
			"expect.body.1.field": "", "expect.body.1.op": "contains", "expect.body.1.value": "plain",
			"expect.body.2.field": fbcFieldB, "expect.body.2.op": "equals", "expect.body.2.value": fbcWrong,
		}, "plain text answer"},
		{"unsupported op at check 2", func() map[string]string {
			p := threeEquals(0)
			p["expect.body.2.op"] = "frobnicate"
			return p
		}(), fbcBody()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := runBodyAssertProps(t, c.props, c.body)
			if !v.Failure || !strings.HasSuffix(v.Message, "[argus-body-check=2]") {
				t.Fatalf("want a failure ending [argus-body-check=2], got failure=%v %q", v.Failure, v.Message)
			}
			assertNoValueInMessage(t, v.Message)
			if strings.Contains(v.Message, "missingFieldZ9") {
				t.Errorf("the message names the asserted field: %q", v.Message)
			}
		})
	}
}

// templateRunner is a Runner that runs the REAL http-ingestion body-assertion script with the props
// RunAll derived, against a canned SUT answer with status 200, and writes the .jtl row JMeter would.
type templateRunner struct {
	t    *testing.T
	body string
}

func (r *templateRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	if templateBase != "http-ingestion" {
		r.t.Fatalf("templateRunner: unexpected template %q", templateBase)
	}
	v := runBodyAssertProps(r.t, props, r.body)
	succ := "true"
	if v.Failure {
		succ = "false"
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"timeStamp", "elapsed", "label", "responseCode", "responseMessage", "threadName", "success", "failureMessage"})
	_ = w.Write([]string{"1781024939842", "42", props["scenario.id"], "200", "OK", "http-ingestion 1-1", succ, v.Message})
	w.Flush()
	return os.WriteFile(jtlPath, buf.Bytes(), 0o644)
}

func threeBulletScenario(id string, wrong ...int) (md string, bullets []string) {
	fields := []string{fbcFieldA, fbcFieldB, fbcFieldC}
	gots := []string{fbcGotA, fbcGotB, fbcGotC}
	for i := 1; i <= 3; i++ {
		v := gots[i-1]
		for _, w := range wrong {
			if i == w {
				v = fbcWrong
			}
		}
		bullets = append(bullets, "body has "+fields[i-1]+" equals "+v)
	}
	lines := []string{
		"# Scenario: " + id, "",
		"## Metadata",
		"- **ID**: " + id,
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http", "",
		"## TRIGGER",
		"GET \x60${INGESTION_URL}/api/v1/things\x60", "",
		"## EXPECT",
		"### Runnable",
		"- status=200",
	}
	for _, b := range bullets {
		lines = append(lines, "- "+b)
	}
	lines = append(lines, "", "## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return strings.Join(lines, "\n"), bullets
}

func runThreeBullet(t *testing.T, wrong ...int) (*report.ScenarioResult, []string) {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	const id = "BODY-030"
	md, bullets := threeBulletScenario(id, wrong...)
	writeScenarioMD(t, scDir, "http-ingestion", id, md)
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &templateRunner{t: t, body: fbcBody()})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find(id)
	if res == nil {
		t.Fatal("scenario not in the report")
	}
	return res, bullets
}

// Required test (1) and (2): three body bullets, only bullet 2 fails. The TEST hat's failure names
// bullet 2 — not 1, not 3 — and `observed` is exactly today's fixed sentence.
func TestRunAll_FailedBodyCheck_NamesBullet2Only(t *testing.T) {
	res, bullets := runThreeBullet(t, 2)
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("want failed with a failure record, got %q %+v", res.Status, res.Failure)
	}
	// (2) observed is byte-for-byte unchanged
	if res.Failure.Observed != bodyAssertObserved {
		t.Errorf("observed changed:\n got %q\nwant %q", res.Failure.Observed, bodyAssertObserved)
	}
	// (1) the failing bullet is named, by position and as written
	fb := res.Failure.FailedBodyCheck
	if fb == nil {
		t.Fatalf(" the test-hat failure does not say which body check failed: %+v", res.Failure)
	}
	if fb.Index != 2 || fb.Bullet != bullets[1] {
		t.Errorf("failed_body_check = %+v, want index 2, bullet %q", *fb, bullets[1])
	}
	for _, other := range []string{bullets[0], bullets[2]} {
		if fb.Bullet == other {
			t.Errorf("failed_body_check names a bullet that held: %q", other)
		}
	}
	// the expected value is still where it was (the test hat's joined EXPECT)
	if res.Failure.Expected == nil || !strings.Contains(*res.Failure.Expected, fbcWrong) {
		t.Errorf("failure.expected must still carry the joined EXPECT: %v", res.Failure.Expected)
	}
}

// Plant one wrong check per scenario, at each position: each run names its own bullet, observed never moves.
func TestRunAll_FailedBodyCheck_EachPosition(t *testing.T) {
	for wrong := 1; wrong <= 3; wrong++ {
		t.Run(fmt.Sprintf("bullet %d wrong", wrong), func(t *testing.T) {
			res, bullets := runThreeBullet(t, wrong)
			if res.Failure == nil || res.Failure.Observed != bodyAssertObserved {
				t.Fatalf("observed must be the fixed sentence, got %+v", res.Failure)
			}
			if fb := res.Failure.FailedBodyCheck; fb == nil || fb.Index != wrong || fb.Bullet != bullets[wrong-1] {
				t.Errorf("failed_body_check = %+v, want index %d bullet %q", fb, wrong, bullets[wrong-1])
			}
		})
	}
	// all hold: passed, no failure record at all
	res, _ := runThreeBullet(t, 0)
	if res.Status != "passed" || res.Failure != nil {
		t.Errorf("three holding checks must pass with no failure, got %q %+v", res.Status, res.Failure)
	}
}

// Two bullets fail (2 and 3): the template stops at the FIRST failing check, so the report names bullet 2
// and only bullet 2. Bullet 3 is not evaluated, so it is not claimed to have held or failed.
func TestRunAll_FailedBodyCheck_TwoFailingNamesTheFirst(t *testing.T) {
	res, bullets := runThreeBullet(t, 2, 3)
	if res.Status != "failed" || res.Failure == nil || res.Failure.Observed != bodyAssertObserved {
		t.Fatalf("want failed with the fixed observed sentence, got %q %+v", res.Status, res.Failure)
	}
	if fb := res.Failure.FailedBodyCheck; fb == nil || fb.Index != 2 || fb.Bullet != bullets[1] {
		t.Errorf("two failing bullets: failed_body_check = %+v, want the FIRST, index 2 bullet %q", fb, bullets[1])
	}
}

// The Go half alone: the index maps to the bullet from the PARSED scenario, never from the message
// text, and anything that is not exactly the trailing token names nothing.
func TestFailedBodyCheckFrom(t *testing.T) {
	runnable := []string{
		"status=200",
		"body has a equals 1",
		"header X-Thing is present", // not a body bullet: not counted
		"body contains hello",
		"body has b matching ^x",
	}
	cases := []struct {
		name, msg string
		want      *report.FailedBodyCheck
	}{
		{"index 1", "lbl: 200 OK BODY-ASSERT-FAIL: response body did not satisfy the expected body assertion [argus-body-check=1]",
			&report.FailedBodyCheck{Index: 1, Bullet: "body has a equals 1"}},
		{"index 2 skips the non-body bullet", "x BODY-ASSERT-FAIL: y [argus-body-check=2]",
			&report.FailedBodyCheck{Index: 2, Bullet: "body contains hello"}},
		{"index 3", "x BODY-ASSERT-FAIL: y [argus-body-check=3]",
			&report.FailedBodyCheck{Index: 3, Bullet: "body has b matching ^x"}},
		{"no token (an older template)", "x BODY-ASSERT-FAIL: response body did not satisfy the expected body assertion", nil},
		{"out of range", "x BODY-ASSERT-FAIL: y [argus-body-check=4]", nil},
		{"zero", "x BODY-ASSERT-FAIL: y [argus-body-check=0]", nil},
		{"token not at the end", "x [argus-body-check=1] BODY-ASSERT-FAIL: y", nil},
		{"not a number", "x BODY-ASSERT-FAIL: y [argus-body-check=a]", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := failedBodyCheckFrom(c.msg, runnable)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("failedBodyCheckFrom(%q) = %+v, want %+v", c.msg, got, c.want)
			}
		})
	}
	// The positions agree with the props DeriveProps writes (expect.body.N, from ParseBodyAsserts).
	asserts, _ := ParseBodyAsserts(runnable)
	if n := len(bodyCheckBullets(runnable)); n != len(asserts) {
		t.Errorf("bodyCheckBullets counts %d body checks, ParseBodyAsserts %d — the index would point at the wrong bullet", n, len(asserts))
	}
}
