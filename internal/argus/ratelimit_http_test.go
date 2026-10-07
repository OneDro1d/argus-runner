package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// throttling429Runner writes the .jtl JMeter produces for an HTTP 429: the status code in
// `responseCode`, and the Retry-After the SUT sent in the failureMessage marker the
// http-ingestion JSR223 assertion writes (templates/http-ingestion.jmx).
type throttling429Runner struct {
	throttleFirst int // how many of the first runs answer 429; -1 = always
	retryAfterHdr string
	runs          int
}

func (f *throttling429Runner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	f.runs++
	id := props["scenario.id"]
	code, msg, succ, fmsg := props["expect.status"], "OK", "true", ""
	if code == "" {
		code = "202"
	}
	if f.throttleFirst < 0 || f.runs <= f.throttleFirst {
		code, msg, succ = "429", "Too Many Requests", "false"
		fmsg = "RATE-LIMITED: retry_after=" + f.retryAfterHdr
	}
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,42," + id + "," + code + "," + msg + "," + templateBase + " " + id + " 1-1," + succ + "," + fmsg + "\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

// httpRLCfg is an HTTP SUT that declares a rate limit (per: second, so the fallback window is 1s).
func httpRLCfg(t *testing.T, declare bool) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: order-service\ntargets:\n  http:\n    base_url: http://order-api:8080\n"
	if declare {
		body += "rate_limit:\n  requests: 5\n  per: second\n  signature:\n    body_contains: '\"error\":\"rate_limited\"'\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// VR10-R1-12: on the HTTP path the signature is the STATUS CODE 429 + Retry-After (body_contains is
// not consulted — SA §0.14 R1-e: the status is unambiguous). The scenario is `errored`, naming the
// rate limit and the Retry-After value, and the run pauses and retries once exactly as on MCP.
func TestRunAll_HTTP429IsErroredAndPauses(t *testing.T) {
	slept := fakeSleep(t)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")

	fr := &throttling429Runner{throttleFirst: 1, retryAfterHdr: "30"} // the first attempt only
	rr, err := RunAll(httpRLCfg(t, true), scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 30*time.Second {
		t.Fatalf("pauses = %v; want one 30s pause (the SUT's Retry-After)", *slept)
	}
	if fr.runs != 2 {
		t.Fatalf("jmeter runs = %d; want 2 (the original + exactly ONE retry)", fr.runs)
	}
	one, _ := rr.Report.Find("ORD-001")
	if one == nil || one.Status != "passed" {
		t.Fatalf("ORD-001 = %+v; the retry succeeded, so the row carries the RETRY's outcome", one)
	}
	if one.RateLimitedRetries != 1 || one.PausedMs != 30000 {
		t.Errorf("the retry and the wait must be recorded: retries=%d paused_ms=%d", one.RateLimitedRetries, one.PausedMs)
	}
	if rr.Report.Summary.RateLimitPauses != 1 {
		t.Errorf("Summary.RateLimitPauses = %d; want 1", rr.Report.Summary.RateLimitPauses)
	}
}

// The not-measured HTTP row: still throttled on the retry → `errored` naming the limit, never
// `failed`, and never counted as a pass.
func TestRunAll_HTTP429StaysNotMeasured(t *testing.T) {
	fakeSleep(t)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")

	rr, err := RunAll(httpRLCfg(t, true), scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "",
		&throttling429Runner{throttleFirst: -1, retryAfterHdr: "30"})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := rr.Report.Find("ORD-001")
	if one == nil || one.Status != report.StatusError || !one.RateLimited {
		t.Fatalf("ORD-001 = %+v; want errored + rate-limited", one)
	}
	if one.Failure == nil || !strings.Contains(one.Failure.Observed, "rate-limited") || !strings.Contains(one.Failure.Observed, "30s") {
		t.Errorf("observed must name the rate limit and the Retry-After value: %+v", one.Failure)
	}
	if rr.Report.Summary.Failed != 0 || rr.Report.Summary.Errored != 1 || rr.Report.Summary.RateLimitedScenarios != 1 {
		t.Errorf("summary = %+v; a throttle is never a product failure", rr.Report.Summary)
	}
}

// SA §0.14 R1-f: Retry-After is an HTTP header, so it may be an HTTP-date as well as seconds. Both
// are parsed; a header we cannot read falls back to the declared window rather than not pausing.
func TestHTTPRetryAfter_SecondsOrHTTPDate(t *testing.T) {
	future := time.Now().UTC().Add(45 * time.Second).Format(http1123)
	for _, c := range []struct {
		raw  string
		want time.Duration
		near bool
	}{
		{"30", 30 * time.Second, false},
		{future, 45 * time.Second, true},
		{"", time.Second, false},        // nothing usable → the declared window (per: second)
		{"garbage", time.Second, false}, // ditto
	} {
		got := parseRetryAfter(c.raw, time.Second)
		if c.near {
			if got < 43*time.Second || got > 46*time.Second {
				t.Errorf("Retry-After %q → %s; want ≈45s", c.raw, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("Retry-After %q → %s; want %s", c.raw, got, c.want)
		}
	}
}

// VR10-R1-3 on the HTTP path: with NO rate_limit block a 429 stays exactly what it is today — a
// failed scenario. No heuristic reclassifies a verdict on its own.
func TestRunAll_HTTP429UndeclaredStaysFailed(t *testing.T) {
	slept := fakeSleep(t)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")

	rr, err := RunAll(httpRLCfg(t, false), scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "",
		&throttling429Runner{throttleFirst: -1, retryAfterHdr: "30"})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := rr.Report.Find("ORD-001")
	if one == nil || one.Status != "failed" || one.RateLimited {
		t.Fatalf("ORD-001 = %+v; an undeclared SUT's 429 must stay failed", one)
	}
	if len(*slept) != 0 {
		t.Fatalf("an undeclared SUT paused the run: %v", *slept)
	}
}

// A scenario that DELIBERATELY tests the limiter (`status=429`) is MEASURED and green — the SUT did
// exactly the right thing. Reclassifying it would delete the whole Rate Limiting test layer.
func TestRunAll_ExpectedHTTP429IsAPassNotAThrottle(t *testing.T) {
	slept := fakeSleep(t)
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "rate-limiting", "RATE-001", "status=429")

	rr, err := RunAll(httpRLCfg(t, true), scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "",
		&throttling429Runner{throttleFirst: -1, retryAfterHdr: "30"})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := rr.Report.Find("RATE-001")
	if one == nil || one.Status != "passed" || one.RateLimited {
		t.Fatalf("RATE-001 = %+v; a scenario that EXPECTS 429 got what it asked for", one)
	}
	if len(*slept) != 0 {
		t.Fatalf("the run paused for a 429 it was testing for: %v", *slept)
	}
}
