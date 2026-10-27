package chain

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// http_bodycap_test.go -: a chain http step never judges, or saves from,
// a response body longer than maxHTTPBodyBytes as though the cut prefix were the whole body.

const bodycapMarker = "BODYCAPMARK7731"

// bodyOf returns a body of exactly n bytes that starts with prefix and is padded with 'a'.
func bodyOf(n int, prefix, suffix string) string {
	pad := n - len(prefix) - len(suffix)
	return prefix + string(bytes.Repeat([]byte("a"), pad)) + suffix
}

func bodycapServer(t *testing.T, status int, body string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func runBodycap(ts *httptest.Server, wantStatus int, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, opts ...HTTPOption) report.StepResult {
	step := HTTPStep("check", "GET", ts.URL+"/x", nil, "", wantStatus, want, save, poll, false,
		nil, &scenario.MoneySpendLedger{}, opts...)
	return step.Run("cid", map[string]string{})
}

func assertOverLimitError(t *testing.T, res report.StepResult) {
	t.Helper()
	if res.Status != report.StatusError {
		t.Fatalf("an over-limit body that the step must read must end error, got %q: %+v", res.Status, res)
	}
	if !strings.Contains(res.Observed, strconv.Itoa(maxHTTPBodyBytes)) {
		t.Errorf("Observed must name the limit in bytes (%d): %q", maxHTTPBodyBytes, res.Observed)
	}
	if !strings.Contains(res.Observed, "not judged") {
		t.Errorf("Observed must say the body was not judged: %q", res.Observed)
	}
	if strings.Contains(res.Observed, bodycapMarker) || strings.Contains(res.Observed, "aaaa") {
		t.Errorf("Observed must carry no byte of the body: %q", res.Observed)
	}
}

// Rule 1: a body claim that the prefix would satisfy.
func TestHTTPStep_BodyOverLimit_ContainsClaimOnPrefixIsError(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes+1, bodycapMarker, ""), nil)
	defer ts.Close()
	res := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: bodycapMarker}}, nil, nil)
	assertOverLimitError(t, res)
}

// Rule 1: the false green - a NEGATIVE claim with the forbidden text past the cut.
func TestHTTPStep_BodyOverLimit_NotContainsPastTheCutIsError(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes+100, "", "FORBIDDENTEXT"), nil)
	defer ts.Close()
	res := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyNotContainsOp, Value: "FORBIDDENTEXT"}}, nil, nil)
	assertOverLimitError(t, res)
}

// Rule 1: a JSON claim on a valid JSON document longer than the cap is not a "not JSON" content failure.
func TestHTTPStep_BodyOverLimit_ValidJSONClaimIsErrorNotFailed(t *testing.T) {
	doc := `{"ok":true,"pad":"` + string(bytes.Repeat([]byte("a"), maxHTTPBodyBytes)) + `"}`
	ts := bodycapServer(t, 200, doc, nil)
	defer ts.Close()
	res := runBodycap(ts, 200, []mcp.BodyAssert{{Field: "ok", Op: mcp.BodyEqualsOp, Value: "true"}}, nil, nil)
	assertOverLimitError(t, res)
	if res.Status == "failed" {
		t.Fatalf("must not read failed: %+v", res)
	}
}

// Rule 1: a save alone (no body claim) reads the body.
func TestHTTPStep_BodyOverLimit_SaveOnlyIsError(t *testing.T) {
	doc := `{"id":"abc","pad":"` + string(bytes.Repeat([]byte("a"), maxHTTPBodyBytes)) + `"}`
	ts := bodycapServer(t, 200, doc, nil)
	defer ts.Close()
	res := runBodycap(ts, 200, nil, map[string]scenario.SaveSpec{"gid": {Path: "id"}}, nil)
	assertOverLimitError(t, res)
	if len(res.Captured) != 0 {
		t.Errorf("nothing may be saved from a body that was not read whole: %+v", res.Captured)
	}
}

// Rule 1: with no status claim at all, a body claim still makes the step error.
func TestHTTPStep_BodyOverLimit_NoStatusClaimBodyClaimIsError(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes+1, bodycapMarker, ""), nil)
	defer ts.Close()
	res := runBodycap(ts, 0, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: bodycapMarker}}, nil, nil)
	assertOverLimitError(t, res)
}

// Rule 2: status-only claim on a large download passes.
func TestHTTPStep_BodyOverLimit_StatusOnlyStillPasses(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes*2, "", ""), nil)
	defer ts.Close()
	res := runBodycap(ts, 200, nil, nil, nil)
	if res.Status != "passed" {
		t.Fatalf("a large download with status == 200 must still pass: %+v", res)
	}
}

// Rule 2: an elapsed claim reads the clock, not the body.
func TestHTTPStep_BodyOverLimit_ElapsedOnlyStillPasses(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes*2, "", ""), nil)
	defer ts.Close()
	res := runBodycap(ts, 200, nil, nil, nil, WithElapsed(&scenario.ElapsedClaim{MaxMs: 60000}))
	if res.Status != "passed" {
		t.Fatalf("a large download with a met elapsed claim must still pass: %+v", res)
	}
}

// Rule 3: a missed status claim keeps status failed, the status reason and the body excerpt.
func TestHTTPStep_BodyOverLimit_StatusMissStaysFailedWithExcerpt(t *testing.T) {
	ts := bodycapServer(t, 500, bodyOf(maxHTTPBodyBytes+5, "upstream exploded", ""), nil)
	defer ts.Close()
	res := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "x"}}, nil, nil)
	if res.Status != "failed" {
		t.Fatalf("a missed status claim must stay failed, got %q: %+v", res.Status, res)
	}
	if !strings.Contains(res.Observed, "status 500") || !strings.Contains(res.Observed, "status claim did not hold") {
		t.Errorf("the status miss must be the reason: %q", res.Observed)
	}
	if !strings.Contains(res.BodyExcerpt, "upstream exploded") {
		t.Errorf("the capped body excerpt must still be given to the author: %q", res.BodyExcerpt)
	}
}

// Rule 4: a poll returns the error on the attempt that saw the body, it does not poll to the timeout.
func TestHTTPStep_BodyOverLimit_PollStopsOnFirstAttempt(t *testing.T) {
	var hits int32
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes+1, bodycapMarker, ""), &hits)
	defer ts.Close()
	start := time.Now()
	res := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "never-there"}}, nil,
		&Poll{Timeout: 20 * time.Second, Interval: 50 * time.Millisecond})
	assertOverLimitError(t, res)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("the request must be sent once, was sent %d times", n)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("a poll must not run to its timeout on an over-limit body")
	}
}

// Rule 5: a body of exactly the cap is whole and judged as before.
func TestHTTPStep_BodyOfExactlyTheLimitIsJudged(t *testing.T) {
	ts := bodycapServer(t, 200, bodyOf(maxHTTPBodyBytes, "", "ENDMARK"), nil)
	defer ts.Close()
	pass := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "ENDMARK"}}, nil, nil)
	if pass.Status != "passed" {
		t.Fatalf("a body of exactly %d bytes is whole and its claim must pass: %+v", maxHTTPBodyBytes, pass)
	}
	miss := runBodycap(ts, 200, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "NOPE"}}, nil, nil)
	if miss.Status != "failed" {
		t.Fatalf("a body of exactly %d bytes is whole and a missed claim must read failed: %+v", maxHTTPBodyBytes, miss)
	}
}

// Rule 6 is pinned inside assertOverLimitError (no body byte in Observed) for every error case above.
