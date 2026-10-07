package mcp

import (
	"strings"
	"testing"
	"time"
)

func toolErr(text string) CallResult {
	return CallResult{IsError: true, Content: []ContentBlock{{Type: "text", Text: text}}}
}

var memstoreSig = &RateLimitSpec{
	BodyContains:    `"error":"rate_limited"`,
	RetryAfterField: "retry_after",
	DefaultPause:    time.Minute,
}

// VR10-R1-2 + VR10-R1-4 + VR10-R1-3, as one table: the signature HIT is a rate limit; the
// near-miss (a genuine product error whose text merely mentions "rate limit") is NOT; and with no
// declaration at all nothing is ever reclassified.
func TestJudge_RateLimitTable(t *testing.T) {
	cases := []struct {
		name       string
		res        CallResult
		spec       *RateLimitSpec
		want       bool
		wantRetry  time.Duration
		wantPassed bool
	}{
		{"signature hit, retry_after read",
			toolErr(`{"error":"rate_limited","retry_after":48}`), memstoreSig, true, 48 * time.Second, false},
		{"signature hit, no retry_after field in the body → the declared window",
			toolErr(`{"error":"rate_limited"}`), memstoreSig, true, time.Minute, false},
		{"near miss: a real product error that merely mentions a rate limit stays failed",
			toolErr(`{"error":"not_found","message":"the rate limit document was not found"}`), memstoreSig, false, 0, false},
		{"no declaration: the same throttle body is judged exactly as today",
			toolErr(`{"error":"rate_limited","retry_after":48}`), nil, false, 0, false},
		{"a declaration with an empty signature never matches anything",
			toolErr(`{"error":"rate_limited"}`), &RateLimitSpec{DefaultPause: time.Minute}, false, 0, false},
		{"a SUCCESSFUL call is never a throttle, whatever its text says",
			CallResult{Content: []ContentBlock{{Type: "text", Text: `{"error":"rate_limited"}`}}}, memstoreSig, false, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := JudgeWithRateLimit(c.res, Expect{ErrorPlane: PlaneNone}, c.spec)
			if v.RateLimited != c.want {
				t.Fatalf("RateLimited = %v; want %v (observed %q)", v.RateLimited, c.want, v.Observed)
			}
			if v.RetryAfter != c.wantRetry {
				t.Errorf("RetryAfter = %s; want %s", v.RetryAfter, c.wantRetry)
			}
			if v.Pass != c.wantPassed {
				t.Errorf("Pass = %v; want %v", v.Pass, c.wantPassed)
			}
		})
	}
}

// VR10-R1-6: the observed line names the rate limit AND the retry-after value — the operator must
// be able to tell a throttle from a defect by reading the report.
func TestJudge_RateLimitedObservedNamesTheLimitAndTheWait(t *testing.T) {
	v := JudgeWithRateLimit(toolErr(`{"error":"rate_limited","retry_after":48}`), Expect{ErrorPlane: PlaneNone}, memstoreSig)
	want := "SUT rate-limited this request (retry_after 48s) — the scenario was not measured"
	if v.Observed != want {
		t.Fatalf("observed = %q; want %q", v.Observed, want)
	}
	// VR-C8: reality only — it must not echo anything the scenario asserted.
	if strings.Contains(v.Observed, "isError") {
		t.Errorf("observed leaked the plane vocabulary: %q", v.Observed)
	}
}

// A throttle is not the tool-plane error a scenario asked for: a scenario that EXPECTS
// result.isError:true must NOT go green on a rate-limit refusal — nothing was measured.
func TestJudge_ThrottleDoesNotSatisfyAnExpectedToolError(t *testing.T) {
	v := JudgeWithRateLimit(toolErr(`{"error":"rate_limited","retry_after":5}`), Expect{ErrorPlane: PlaneTool}, memstoreSig)
	if v.Pass {
		t.Fatal("a rate-limit refusal was accepted as the expected tool-plane error — that is a false green")
	}
	if !v.RateLimited {
		t.Fatal("the throttle was not recognised on a tool-plane scenario")
	}
}

// VR10-R1-3: Judge (the 2-arg form every existing caller uses) is unchanged — no heuristic.
func TestJudge_TwoArgFormNeverRateLimits(t *testing.T) {
	v := Judge(toolErr(`{"error":"rate_limited","retry_after":48}`), Expect{ErrorPlane: PlaneNone})
	if v.RateLimited {
		t.Fatal("the undeclared path reclassified a verdict on its own")
	}
	if v.Observed != "responder returned result.isError:true" {
		t.Errorf("undeclared behaviour changed: observed = %q", v.Observed)
	}
}

// A JSON-RPC protocol error is the protocol plane, not a throttle, even if its message says so:
// the declared signature is matched against the TOOL plane only (SA §1.4.R1).
func TestJudge_ProtocolErrorIsNeverAThrottle(t *testing.T) {
	r := CallResult{JSONRPCError: &RPCError{Code: -32603, Message: `{"error":"rate_limited"}`}}
	if v := JudgeWithRateLimit(r, Expect{ErrorPlane: PlaneNone}, memstoreSig); v.RateLimited {
		t.Fatal("a protocol-plane error was classified as a throttle")
	}
}

// The retry_after field is read whether the SUT writes it as a number or as a string.
func TestJudge_RetryAfterNumberOrString(t *testing.T) {
	for _, body := range []string{`{"error":"rate_limited","retry_after":30}`, `{"error":"rate_limited","retry_after":"30"}`, `{"error":"rate_limited", "retry_after" : 30 }`} {
		v := JudgeWithRateLimit(toolErr(body), Expect{ErrorPlane: PlaneNone}, memstoreSig)
		if v.RetryAfter != 30*time.Second {
			t.Errorf("body %s → RetryAfter %s; want 30s", body, v.RetryAfter)
		}
	}
}

// V31-005 (VR13-EE) GUARD — a throttle still wins over an error step's body checks.
//
// The judge now evaluates a body check once the expected error plane matches, and the order of the
// branches is what keeps that from swallowing a rate limit: the throttle is recognised BEFORE any
// plane is considered, so a refusal is still `errored` and nothing is measured. Without this guard,
// a later reordering could make a rate-limited scenario report a content miss — a SUT failure where
// there was only a closed door.
func TestJudge_ThrottleStillWinsOverAnErrorStepBody(t *testing.T) {
	v := JudgeWithRateLimit(toolErr(`{"error":"rate_limited","retry_after":5}`),
		Expect{ErrorPlane: PlaneTool, Body: []BodyAssert{{Op: BodyContainsOp, Value: "never-there"}}},
		memstoreSig)
	if v.Pass {
		t.Fatal("a rate-limit refusal was accepted — that is a false green")
	}
	if !v.RateLimited {
		t.Fatal("the throttle was not recognised on an error-plane scenario carrying body checks")
	}
	if v.Plane == PlaneBody {
		t.Errorf("the throttle was reported as a content miss (%q) — nothing was measured", v.Plane)
	}
}
