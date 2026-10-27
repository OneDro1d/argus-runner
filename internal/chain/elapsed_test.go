package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The `elapsed` claim of a chain http step, against a real httptest server that sleeps.

func sleepyServer(t *testing.T, sleeps ...time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(sleeps) {
			i = len(sleeps) - 1
		}
		time.Sleep(sleeps[i])
		_, _ = w.Write([]byte(`{"v":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func elapsedStep(url string, poll *Poll, c scenario.ElapsedClaim) Step {
	return HTTPStep("t", "GET", url+"/x", nil, "", 200, nil, nil, poll, false, nil, &scenario.MoneySpendLedger{}, WithElapsed(&c))
}

func TestElapsed_UnderTheBoundPasses_AndRecordsTheDuration(t *testing.T) {
	srv, _ := sleepyServer(t, 20*time.Millisecond)
	st := elapsedStep(srv.URL, nil, scenario.ElapsedClaim{MaxMs: 5000}).Run("cid", map[string]string{})
	if st.Status != "passed" {
		t.Fatalf("a 20 ms answer must pass `elapsed < 5000 ms`: %+v", st)
	}
	if st.ElapsedMs == nil || *st.ElapsedMs < 20 || *st.ElapsedMs > 4000 {
		t.Fatalf("a PASSING step must record elapsed_ms (>= the 20 ms the server slept): %v", st.ElapsedMs)
	}
	if !strings.Contains(strings.Join(st.AssertionsEnforced, ";"), "elapsed < 5000 ms") {
		t.Errorf("the claim is listed as enforced (author-only text): %v", st.AssertionsEnforced)
	}
}

func TestElapsed_OverTheBoundFails_NeverErrorNeverTimeout(t *testing.T) {
	srv, _ := sleepyServer(t, 150*time.Millisecond)
	st := elapsedStep(srv.URL, nil, scenario.ElapsedClaim{MaxMs: 40}).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("a 150 ms answer must FAIL `elapsed < 40 ms` (failed, not error): %+v", st)
	}
	if strings.Contains(strings.ToLower(st.Observed), "timed out") || strings.Contains(strings.ToLower(st.Observed), "timeout") {
		t.Errorf("an elapsed miss is not a timeout: %q", st.Observed)
	}
	if st.ElapsedMs == nil || *st.ElapsedMs < 150 {
		t.Fatalf("a failing step records elapsed_ms too: %v", st.ElapsedMs)
	}
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "elapsed < 40 ms" || !strings.HasPrefix(got[0].Observed, "took ") ||
		!strings.HasSuffix(got[0].Observed, ", bound 40 ms") {
		t.Fatalf("the author-only record reads `took <N> ms, bound <M> ms`: %+v", got)
	}
}

// ⛔ the bound is an expected value: it must not be in the step's observed text.
func TestElapsed_TheBoundIsNotInTheBuilderVisibleText(t *testing.T) {
	srv, _ := sleepyServer(t, 150*time.Millisecond)
	st := elapsedStep(srv.URL, nil, scenario.ElapsedClaim{MaxMs: 7}).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("want failed: %+v", st)
	}
	for _, form := range []string{"bound", "< 7", "<= 7", "7 ms", "7ms"} {
		// "7 ms" could be the tail of a duration (e.g. 157 ms): only flag it when it is not preceded by a digit
		if i := strings.Index(st.Observed, form); i >= 0 && !(form == "7 ms" && i > 0 && st.Observed[i-1] >= '0' && st.Observed[i-1] <= '9') {
			t.Errorf("the observed text carries the bound (%q): %q", form, st.Observed)
		}
	}
	if !strings.Contains(st.Observed, "the response took ") || !strings.Contains(st.Observed, "a timing claim did not hold") {
		t.Errorf("the builder-visible text names the duration and the failed timing claim: %q", st.Observed)
	}
}

func TestElapsed_InclusiveBound(t *testing.T) {
	c := scenario.ElapsedClaim{MaxMs: 500, Inclusive: true}
	if !c.Holds(500*time.Millisecond) || c.Holds(500*time.Millisecond+time.Microsecond) {
		t.Error("`<=` holds at the bound and not a microsecond past it")
	}
	lt := scenario.ElapsedClaim{MaxMs: 500}
	if lt.Holds(500*time.Millisecond) || !lt.Holds(499*time.Millisecond+999*time.Microsecond) {
		t.Error("`<` fails at the bound")
	}
}

// With poll the LAST attempt is the one judged: the first answer is slow (it fails the bound, so the poll
// goes on), the second is fast and passes. The recorded duration is the second attempt's, not a total.
func TestElapsed_PollJudgesTheLastAttempt(t *testing.T) {
	srv, n := sleepyServer(t, 200*time.Millisecond, 5*time.Millisecond)
	poll := &Poll{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}
	st := elapsedStep(srv.URL, poll, scenario.ElapsedClaim{MaxMs: 100}).Run("cid", map[string]string{})
	if st.Status != "passed" {
		t.Fatalf("the last (fast) attempt is the one judged: %+v", st)
	}
	if atomic.LoadInt32(n) != 2 {
		t.Errorf("want 2 attempts, got %d", atomic.LoadInt32(n))
	}
	if st.ElapsedMs == nil || *st.ElapsedMs >= 100 {
		t.Errorf("elapsed_ms is the last attempt's (< 100 ms), not the poll's total: %v", st.ElapsedMs)
	}
}

func TestElapsed_PollThatNeverGetsFastFailsOnTheLastAttempt(t *testing.T) {
	srv, _ := sleepyServer(t, 120*time.Millisecond)
	poll := &Poll{Timeout: 300 * time.Millisecond, Interval: 10 * time.Millisecond}
	st := elapsedStep(srv.URL, poll, scenario.ElapsedClaim{MaxMs: 50}).Run("cid", map[string]string{})
	if st.Status != "failed" || st.ElapsedMs == nil || *st.ElapsedMs < 120 || *st.ElapsedMs > 280 {
		t.Fatalf("a failed poll records ONE attempt's duration (~120 ms), not the total (>= 300 ms): %+v %v", st, st.ElapsedMs)
	}
	if strings.Contains(st.Observed, "bound") {
		t.Errorf("the bound leaked into the poll's text: %q", st.Observed)
	}
}

// An http step with no `elapsed` claim still records its duration, pass or fail.
func TestElapsed_EveryHTTPStepRecordsItsDuration(t *testing.T) {
	srv, _ := sleepyServer(t, 10*time.Millisecond)
	pass := HTTPStep("t", "GET", srv.URL+"/x", nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}).Run("cid", map[string]string{})
	fail := HTTPStep("t", "GET", srv.URL+"/x", nil, "", 201, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}).Run("cid", map[string]string{})
	if pass.Status != "passed" || pass.ElapsedMs == nil || fail.Status != "failed" || fail.ElapsedMs == nil {
		t.Fatalf("elapsed_ms is recorded pass or fail: %+v / %+v", pass, fail)
	}
}

// A body claim and an elapsed claim that both miss are both listed.
func TestElapsed_ListedBesideABodyClaimThatMissed(t *testing.T) {
	srv, _ := sleepyServer(t, 120*time.Millisecond)
	c := scenario.ElapsedClaim{MaxMs: 30}
	st := HTTPStep("t", "GET", srv.URL+"/x", nil, "", 200,
		[]mcp.BodyAssert{{Field: "v", Op: mcp.BodyEQOp, Value: "2"}}, nil, nil, false, nil, &scenario.MoneySpendLedger{}, WithElapsed(&c)).Run("cid", map[string]string{})
	got := failedClaimsOf(t, st)
	if st.Status != "failed" || len(got) != 2 {
		t.Fatalf("both claims missed and both are listed: %+v %+v", st, got)
	}
}

// ── ${saved.x} as the reference of `==` and `within`, through the real binding path ─────────────────

func eqWithinStep(url, path string, want ...mcp.BodyAssert) Step {
	return HTTPStep("read2", "GET", url+path, nil, "", 200, want, nil, nil, false, nil, &scenario.MoneySpendLedger{})
}

func TestSavedReference_EqualsAcrossSpellings(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {`{"count": 100}`}, "/b": {`{"count": 100.0}`}, "/c": {`{"count": 101}`}})
	eq := mcp.BodyAssert{Field: "count", Op: mcp.BodyEQOp, Value: "${saved.n}"}
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), eqWithinStep(srv.URL, "/b", eq)})
	if res.Status != "passed" {
		t.Fatalf("100 == saved 100 (spelled 100.0 in the answer) must pass: %+v %+v", res, res.Steps)
	}
	res = Run("cid", []Step{readStep(srv.URL, saveN("count")), eqWithinStep(srv.URL, "/c", eq)})
	if res.Status != "failed" {
		t.Fatalf("101 == saved 100 must fail: %+v %+v", res, res.Steps)
	}
	if !strings.Contains(strings.Join(res.Steps[1].AssertionsEnforced, ";"), "field count == ${saved.n}") {
		t.Errorf("the claim is shown as written: %v", res.Steps[1].AssertionsEnforced)
	}
}

func TestSavedReference_Within(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {`{"count": 1000}`}, "/in": {`{"count": 1019}`}, "/out": {`{"count": 1021}`}})
	within := mcp.BodyAssert{Field: "count", Op: mcp.BodyWithinOp, Tol: "2%", Value: "${saved.n}"}
	if res := Run("cid", []Step{readStep(srv.URL, saveN("count")), eqWithinStep(srv.URL, "/in", within)}); res.Status != "passed" {
		t.Fatalf("1019 is within 2%% of the saved 1000: %+v %+v", res, res.Steps)
	}
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), eqWithinStep(srv.URL, "/out", within)})
	if res.Status != "failed" {
		t.Fatalf("1021 is outside 2%% of the saved 1000: %+v %+v", res, res.Steps)
	}
	assertNoValue(t, res, "within 2% of 1000") // the saved value is never printed beside the claim
}

func TestSavedReference_NonNumberSavedValueFailsByName(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {`{"count": "abc"}`}, "/b": {`{"count": 1}`}})
	eq := mcp.BodyAssert{Field: "count", Op: mcp.BodyEQOp, Value: "${saved.n}"}
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), eqWithinStep(srv.URL, "/b", eq)})
	if res.Status != "failed" || !strings.Contains(res.Steps[1].Observed, "not a number") {
		t.Fatalf("a saved non-number against `==` fails by name: %+v", res.Steps[1])
	}
}
