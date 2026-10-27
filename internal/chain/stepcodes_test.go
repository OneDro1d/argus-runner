package chain

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-F2.1 — a chain step records the code it got in its own field: an http step the HTTP status code of the response
// it judged (observed_status), a db step the SQLSTATE of a query the database refused (observed_sqlstate). A step
// that got no answer has neither field.

// statusSeqServer answers each request with the next status of codes (the last one repeats).
func statusSeqServer(t *testing.T, codes ...int) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(codes) {
			i = len(codes) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(codes[i])
		_, _ = w.Write([]byte(`{"id":"x1","v":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func codeStep(url string, wantStatus int, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, opts ...HTTPOption) Step {
	return HTTPStep("s", "GET", url, nil, "", wantStatus, want, save, poll, false, nil, &scenario.MoneySpendLedger{}, opts...)
}

// stepJSON is the step as report.json writes it.
func stepJSON(t *testing.T, st report.StepResult) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNoCodes: a step that got no answer carries neither field, and neither key is written.
func assertNoCodes(t *testing.T, what string, st report.StepResult) {
	t.Helper()
	if st.ObservedStatus != 0 || st.ObservedSQLState != "" {
		t.Errorf("%s: no answer, so no code: observed_status %d, observed_sqlstate %q (%+v)", what, st.ObservedStatus, st.ObservedSQLState, st)
	}
	if j := stepJSON(t, st); strings.Contains(j, "observed_status") || strings.Contains(j, "observed_sqlstate") {
		t.Errorf("%s: neither key may be written: %s", what, j)
	}
}

// ── http ─────────────────────────────────────────────────────────────────────────────────────────

func TestStepCodes_HTTP_APassingStepRecordsItsStatus(t *testing.T) {
	srv, _ := statusSeqServer(t, 200)
	st := codeStep(srv.URL+"/x", 200, nil, nil, nil).Run("cid", map[string]string{})
	if st.Status != "passed" || st.ObservedStatus != 200 {
		t.Fatalf("a passing http step records the status it got (200): %+v", st)
	}
	if j := stepJSON(t, st); !strings.Contains(j, `"observed_status":200`) || strings.Contains(j, "observed_sqlstate") {
		t.Errorf("report.json carries observed_status 200 and no observed_sqlstate: %s", j)
	}
}

// The status claim says 200, the app answers 500: the field holds what the app answered, never the expected status.
func TestStepCodes_HTTP_AFailedStatusClaimRecordsTheAnsweredStatusNeverTheExpectedOne(t *testing.T) {
	srv, _ := statusSeqServer(t, 500)
	st := codeStep(srv.URL+"/x", 200, nil, nil, nil).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("setup: status = 200 against a 500 must fail: %+v", st)
	}
	if st.ObservedStatus != 500 {
		t.Fatalf("observed_status must be the app's 500 (never the claimed 200), got %d", st.ObservedStatus)
	}
}

// A body claim that misses on a 200 still records the 200.
func TestStepCodes_HTTP_ABodyClaimMissRecordsTheStatus(t *testing.T) {
	srv, _ := statusSeqServer(t, 201)
	st := codeStep(srv.URL+"/x", 201, []mcp.BodyAssert{{Field: "v", Op: mcp.BodyEQOp, Value: "2"}}, nil, nil).Run("cid", map[string]string{})
	if st.Status != "failed" || st.ObservedStatus != 201 {
		t.Fatalf("a body-claim miss on a 201 records 201: %+v", st)
	}
}

// A polled step that never passes: the field is the status of the LAST attempt (the one judged), not the first.
func TestStepCodes_HTTP_AFailedPollRecordsTheJudgedAttemptsStatus(t *testing.T) {
	srv, n := statusSeqServer(t, 500, 503)
	poll := &Poll{Timeout: 300 * time.Millisecond, Interval: 10 * time.Millisecond}
	st := codeStep(srv.URL+"/x", 200, nil, nil, poll).Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.Contains(st.Observed, "poll timed out") {
		t.Fatalf("setup: the poll must time out: %+v", st)
	}
	if atomic.LoadInt32(n) < 2 {
		t.Fatalf("setup: the poll must make at least 2 attempts, made %d", atomic.LoadInt32(n))
	}
	if st.ObservedStatus != 503 {
		t.Fatalf("observed_status is the last (judged) attempt's 503, never the first attempt's 500: got %d", st.ObservedStatus)
	}
}

// A polled step that passes on a later attempt records that attempt's status.
func TestStepCodes_HTTP_APollThatPassesRecordsThePassingAttemptsStatus(t *testing.T) {
	srv, n := statusSeqServer(t, 500, 202)
	poll := &Poll{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}
	st := codeStep(srv.URL+"/x", 202, nil, nil, poll).Run("cid", map[string]string{})
	if st.Status != "passed" || atomic.LoadInt32(n) != 2 {
		t.Fatalf("setup: the second attempt must pass: %+v after %d attempt(s)", st, atomic.LoadInt32(n))
	}
	if st.ObservedStatus != 202 {
		t.Fatalf("observed_status is the passing attempt's 202, not the earlier 500: got %d", st.ObservedStatus)
	}
}

// The response arrived and the claims held, then a save failed: the step got an answer, so the status is recorded.
func TestStepCodes_HTTP_ASaveThatFailsAfterAResponseRecordsTheStatus(t *testing.T) {
	srv, _ := statusSeqServer(t, 201)
	st := codeStep(srv.URL+"/x", 201, nil, map[string]scenario.SaveSpec{"gid": {Path: "no.such.field"}}, nil).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("setup: a save from a missing field fails the step: %+v", st)
	}
	if st.ObservedStatus != 201 {
		t.Fatalf("a save that failed after a 201 records 201, got %d", st.ObservedStatus)
	}
}

// A body over the cap that the step would read ends `error`, after a response: the status is recorded.
func TestStepCodes_HTTP_AnOverLimitBodyRecordsTheStatus(t *testing.T) {
	ts := bodycapServer(t, 203, bodyOf(maxHTTPBodyBytes+1, `{"id":"`, `"}`), nil)
	defer ts.Close()
	st := runBodycap(ts, 203, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "zzz"}}, nil, nil)
	assertOverLimitError(t, st)
	if st.ObservedStatus != 203 {
		t.Fatalf("an over-limit body is an answer: observed_status 203, got %d", st.ObservedStatus)
	}
}

// In a whole chain, an `error` step ends the chain and chain.Run writes a new record for it (VR12-CH2). An over-limit
// body is an `error` that DID get a response: its status survives into that record. A step that got no
// answer gets the same kind of record with no field.
func TestStepCodes_HTTP_TheChainsStopRecordKeepsTheStatusOfAStepThatGotAResponse(t *testing.T) {
	ts := bodycapServer(t, 203, bodyOf(maxHTTPBodyBytes+1, `{"id":"`, `"}`), nil)
	defer ts.Close()
	over := HTTPStep("big", "GET", ts.URL+"/x", nil, "", 203, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "zzz"}}, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := Run("cid", []Step{over})
	if len(res.Steps) != 1 || res.Steps[0].Status != report.StepNotMeasured {
		t.Fatalf("setup: chain.Run stops on an error step with a not-measured record: %+v", res.Steps)
	}
	if res.Steps[0].ObservedStatus != 203 {
		t.Fatalf("the stop record keeps the status of the response the step got (203), got %d", res.Steps[0].ObservedStatus)
	}
	gone := codeStep(closedURL(t)+"/x", 200, nil, nil, nil)
	res = Run("cid", []Step{gone})
	if len(res.Steps) != 1 || res.Steps[0].Status != report.StepNotMeasured {
		t.Fatalf("setup: a transport error ends not-measured: %+v", res.Steps)
	}
	assertNoCodes(t, "the stop record of a step with no answer", res.Steps[0])
}

// A transport error is not a status code: no field.
func TestStepCodes_HTTP_ATransportErrorRecordsNothing(t *testing.T) {
	st := codeStep(closedURL(t)+"/x", 200, nil, nil, nil).Run("cid", map[string]string{})
	if st.Status != report.StatusError {
		t.Fatalf("setup: a refused connection is error: %+v", st)
	}
	assertNoCodes(t, "transport error", st)
}

// Results built before any request was sent carry no field.
func TestStepCodes_HTTP_ResultsBuiltBeforeAResponseRecordNothing(t *testing.T) {
	srv, n := statusSeqServer(t, 200)
	// an unresolved ${saved.<var>} in the url
	st := codeStep(srv.URL+"/item/${saved.nope}", 200, nil, nil, nil).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("setup: an unbound url fails by name: %+v", st)
	}
	assertNoCodes(t, "url binding error", st)
	// an unresolved ${saved.<var>} in a claim
	st = codeStep(srv.URL+"/x", 200, []mcp.BodyAssert{{Field: "v", Op: mcp.BodyEQOp, Value: "${saved.nope}"}}, nil, nil).Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.HasPrefix(st.Observed, "content assertion: ") {
		t.Fatalf("setup: an unbound claim fails by name: %+v", st)
	}
	assertNoCodes(t, "claim binding error", st)
	if got := atomic.LoadInt32(n); got != 0 {
		t.Fatalf("setup: nothing may be sent for a binding error, the app saw %d request(s)", got)
	}
}

// The chain's ## TIMEOUT cut the request before an answer: no field.
func TestStepCodes_HTTP_AChainDeadlineCutRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	step := codeStep(srv.URL+"/x", 200, nil, nil, nil)
	st := step.RunUntil("cid", map[string]string{}, time.Now().Add(100*time.Millisecond))
	if st.Status != "failed" || !strings.Contains(st.Observed, "## TIMEOUT deadline") {
		t.Fatalf("setup: the chain's deadline must cut the request: %+v", st)
	}
	assertNoCodes(t, "chain deadline", st)
}

// A guarded run refuses a redirect to another host. The app answered with a 3xx, but the step's result is the policy
// refusal: the issue lists policy refusals among the results with no field.
func TestStepCodes_HTTP_ARefusedRedirectRecordsNothing(t *testing.T) {
	other := closedURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+"/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	st := codeStep(srv.URL+"/x", 200, nil, nil, nil, WithMoneyHandling(true)).Run("cid", map[string]string{})
	if st.Status != report.StatusError || !strings.HasPrefix(st.Observed, MoneyResolvedRefusal) {
		t.Fatalf("setup: a guarded step refuses a redirect to another host: %+v", st)
	}
	assertNoCodes(t, "refused redirect", st)
}

// An `unreachable` step that DID get a response failed because one arrived: it records that status. Its
// pass (no response) records nothing.
func TestStepCodes_HTTP_AnUnreachableStepThatGotAResponseRecordsItsStatus(t *testing.T) {
	srv, _ := statusSeqServer(t, 204)
	st := unreachableStep(srv.URL).Run("cid", map[string]string{})
	if st.Status != "failed" || st.ObservedStatus != 204 {
		t.Fatalf("an unreachable step answered with 204 fails and records 204: %+v", st)
	}
	pass := unreachableStep(closedURL(t)).Run("cid", map[string]string{})
	if pass.Status != "passed" {
		t.Fatalf("setup: a refused connection passes `unreachable`: %+v", pass)
	}
	assertNoCodes(t, "unreachable pass", pass)
}

// ── only a three-digit status code is recorded ───────────────────────────────────────────────────

// rawStatusServer answers every request with the raw status line given (Go's own server cannot write "000", "099" or
// "+42"), the body given, and closes the connection.
func rawStatusServer(t *testing.T, statusLine, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				_, _ = fmt.Fprintf(c, "%s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					statusLine, len(body), body)
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

// Every site that records a status records it only when it is 100-999. No step here has a status claim, so a step
// that wrote its expected status (0) instead of the answered one is caught by the 999 rows.
func TestStepCodes_HTTP_OnlyAThreeDigitStatusIsRecorded(t *testing.T) {
	big := bodyOf(maxHTTPBodyBytes+1, `{"id":"`, `"}`)
	for _, tc := range []struct {
		line string
		want int
	}{{"HTTP/1.1 000 Zero", 0}, {"HTTP/1.1 099 Odd", 0}, {"HTTP/1.1 +42 Plus", 0}, {"HTTP/1.1 999 Top", 999}, {"HTTP/1.1 201 Created", 201}} {
		small := rawStatusServer(t, tc.line, `{"id":"x1","v":1}`)
		over := rawStatusServer(t, tc.line, big)
		sites := map[string]report.StepResult{
			"pass, no status claim": codeStep(small+"/x", 0, nil, nil, nil).Run("cid", map[string]string{}),
			"fail, body claim missed": codeStep(small+"/x", 0, []mcp.BodyAssert{{Field: "v", Op: mcp.BodyEQOp, Value: "2"}}, nil, nil).
				Run("cid", map[string]string{}),
			"save failed": codeStep(small+"/x", 0, nil, map[string]scenario.SaveSpec{"g": {Path: "no.such"}}, nil).Run("cid", map[string]string{}),
			"body over the limit": codeStep(over+"/x", 0, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "zzz"}}, nil, nil).
				Run("cid", map[string]string{}),
			"unreachable got a response": unreachableStep(small).Run("cid", map[string]string{}),
		}
		for site, st := range sites {
			if st.Status == "" || strings.HasPrefix(st.Observed, "http request failed") {
				t.Fatalf("%s / %s: setup: the step must get the response: %+v", tc.line, site, st)
			}
			if st.ObservedStatus != tc.want {
				t.Errorf("%s / %s: observed_status = %d, want %d", tc.line, site, st.ObservedStatus, tc.want)
			}
			if j := stepJSON(t, st); tc.want == 0 && strings.Contains(j, "observed_status") {
				t.Errorf("%s / %s: a status outside 100-999 is not written: %s", tc.line, site, j)
			}
		}
	}
}

func TestStepCodes_ObservedHTTPStatusKeepsOnly100To999(t *testing.T) {
	for code, want := range map[int]int{math.MinInt: 0, -7: 0, 0: 0, 99: 0, 100: 100, 101: 101, 200: 200, 599: 599, 998: 998,
		999: 999, 1000: 0, 123456789: 0, math.MaxInt: 0} {
		if got := observedHTTPStatus(code); got != want {
			t.Errorf("observedHTTPStatus(%d) = %d, want %d", code, got, want)
		}
	}
}

// The money-path rule refuses before anything is sent: no field, on each of its three doors of an http step (the
// resolved path, the spend amount, max_per_run), and on the same doors and the binding error of an `unreachable` step.
func TestStepCodes_HTTP_MoneyPathRefusalsRecordNothing(t *testing.T) {
	srv, n := statusSeqServer(t, 200)
	resolved := codeStep(srv.URL+"/api/v1/items/${saved.id}", 200, nil, nil, nil, WithMoneyHandling(true)).
		Run("cid", map[string]string{"id": "orders"})
	if resolved.Status != report.StatusError || !strings.HasPrefix(resolved.Observed, MoneyResolvedRefusal) {
		t.Fatalf("setup: a resolved order path is refused: %+v", resolved)
	}
	assertNoCodes(t, "resolved-path refusal", resolved)
	spend := HTTPStep("spend", "POST", srv.URL+"/order", nil, `{"pay_amount": 999}`, 200, nil, nil, nil, false,
		orderAllow(), &scenario.MoneySpendLedger{}).Run("cid", map[string]string{})
	if spend.Status != report.StatusError || !strings.HasPrefix(spend.Observed, MoneyWriteRefusal) {
		t.Fatalf("setup: an over-max_amount spend is refused: %+v", spend)
	}
	assertNoCodes(t, "spend refusal", spend)
	if got := atomic.LoadInt32(n); got != 0 {
		t.Fatalf("setup: nothing may be sent on a refusal, the app saw %d request(s)", got)
	}

	// max_per_run (2): the third spend of one run is refused before it is sent
	ledger := &scenario.MoneySpendLedger{}
	var third report.StepResult
	for i := 0; i < 3; i++ {
		third = HTTPStep("spend", "POST", srv.URL+"/order", nil, `{"pay_amount": 10}`, 200, nil, nil, nil, false,
			orderAllow(), ledger).Run("cid", map[string]string{})
	}
	if third.Status != report.StatusError || !strings.Contains(third.Observed, "max_per_run") || atomic.LoadInt32(n) != 2 {
		t.Fatalf("setup: the third spend is refused by max_per_run after 2 were sent: %+v (sent %d)", third, atomic.LoadInt32(n))
	}
	assertNoCodes(t, "max_per_run refusal", third)

	// the `unreachable` step's doors before anything is sent
	gone := closedURL(t)
	bind := UnreachableHTTPStep("u", "GET", gone+"/x/${saved.nope}", nil, "", nil, &scenario.MoneySpendLedger{}).
		Run("cid", map[string]string{})
	if bind.Status != "failed" {
		t.Fatalf("setup: an unbound url fails an unreachable step by name: %+v", bind)
	}
	assertNoCodes(t, "unreachable: binding error", bind)
	uResolved := UnreachableHTTPStep("u", "GET", srv.URL+"/api/v1/items/${saved.id}", nil, "", nil, &scenario.MoneySpendLedger{},
		WithMoneyHandling(true)).Run("cid", map[string]string{"id": "orders"})
	if uResolved.Status != report.StatusError || !strings.HasPrefix(uResolved.Observed, MoneyResolvedRefusal) {
		t.Fatalf("setup: an unreachable step to a resolved order path is refused: %+v", uResolved)
	}
	assertNoCodes(t, "unreachable: resolved-path refusal", uResolved)
	uSpend := UnreachableHTTPStep("u", "POST", gone+"/order", nil, `{"pay_amount": 999}`, orderAllow(), &scenario.MoneySpendLedger{}).
		Run("cid", map[string]string{})
	if uSpend.Status != report.StatusError || !strings.HasPrefix(uSpend.Observed, MoneyWriteRefusal) {
		t.Fatalf("setup: an unreachable step's over-max_amount spend is refused: %+v", uSpend)
	}
	assertNoCodes(t, "unreachable: spend refusal", uSpend)
	uLedger := &scenario.MoneySpendLedger{}
	var uThird report.StepResult
	for i := 0; i < 3; i++ {
		uThird = UnreachableHTTPStep("u", "POST", gone+"/order", nil, `{"pay_amount": 10}`, orderAllow(), uLedger).
			Run("cid", map[string]string{})
	}
	if uThird.Status != report.StatusError || !strings.Contains(uThird.Observed, "max_per_run") {
		t.Fatalf("setup: an unreachable step's third spend is refused by max_per_run: %+v", uThird)
	}
	assertNoCodes(t, "unreachable: max_per_run refusal", uThird)
}

// A redirect chain that never ends: Go's client gives up after 10 hops. 3xx answers arrived, but the step judged no
// response (a transport error to the step, as the issue lists): no field.
func TestStepCodes_HTTP_ARedirectChainThatDoesNotEndRecordsNothing(t *testing.T) {
	var hops int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()
	st := codeStep(srv.URL+"/loop", 200, nil, nil, nil).Run("cid", map[string]string{})
	if st.Status != report.StatusError || atomic.LoadInt32(&hops) < 10 {
		t.Fatalf("setup: a redirect loop ends error after 10 hops: %+v (%d hops)", st, atomic.LoadInt32(&hops))
	}
	assertNoCodes(t, "redirect loop", st)
}

// An `unreachable` step that fails WITHOUT a response (a name that does not resolve) records nothing.
func TestStepCodes_HTTP_AnUnreachableStepThatFailedWithoutAResponseRecordsNothing(t *testing.T) {
	withUnreachableKnobs(t, 2*time.Second, func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "typo.invalid", IsNotFound: true}
	})
	st := unreachableStep("http://typo.invalid:8080/x").Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.Contains(st.Observed, "DNS") {
		t.Fatalf("setup: a DNS miss fails the claim: %+v", st)
	}
	assertNoCodes(t, "unreachable failed on DNS", st)
}

// chain.Run's stop record of an `error` step keeps the codes the step itself recorded, in both of its forms: the
// step's own sentence (OwnError) and the not-measured record.
func TestStepCodes_TheChainsStopRecordKeepsBothCodesInBothForms(t *testing.T) {
	for _, own := range []bool{true, false} {
		st := Step{Name: "e", OwnError: own, Run: func(string, map[string]string) report.StepResult {
			return report.StepResult{Status: report.StatusError, Observed: "the step's own words", ObservedStatus: 207, ObservedSQLState: "XX000"}
		}}
		res := Run("cid", []Step{st})
		if len(res.Steps) != 1 {
			t.Fatalf("own=%v: setup: one record: %+v", own, res.Steps)
		}
		if got := res.Steps[0]; got.ObservedStatus != 207 || got.ObservedSQLState != "XX000" {
			t.Errorf("own=%v: the stop record (%s) must keep observed_status 207 and observed_sqlstate XX000, got %d / %q",
				own, got.Status, got.ObservedStatus, got.ObservedSQLState)
		}
	}
}

// ── db ───────────────────────────────────────────────────────────────────────────────────────────

func TestStepCodes_DB_ARefusedQueryRecordsItsSQLSTATE(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{err: &DBServerError{SQLState: "42P01", Message: `relation "orderz" does not exist`}}}}
	st := runDB(t, f, DBSpec{}, claims(t, "body has row_count == 0"), nil, nil, false)
	if st.Status != "failed" || st.ObservedSQLState != "42P01" {
		t.Fatalf("a refused query records its SQLSTATE 42P01: %+v", st)
	}
	j := stepJSON(t, st)
	if !strings.Contains(j, `"observed_sqlstate":"42P01"`) || strings.Contains(j, "observed_status") {
		t.Errorf("report.json carries observed_sqlstate 42P01 and no observed_status: %s", j)
	}
}

// Only exactly 5 characters, each 0-9 or A-Z, are recorded. Anything else is not: the step still fails as before.
func TestStepCodes_DB_ACodeThatIsNotFiveCharactersOf0to9AtoZIsNotRecorded(t *testing.T) {
	for _, code := range []string{"", "4201", "42P012", "42p01", "42P0!", "42 01", "42P0/", "42P0:", "42P0@", "42P0[",
		"\x0042P0", "42P0١", "４２P01", "42P01\n", "42\"01"} {
		f := &fakeDB{results: []fakeResult{{err: &DBServerError{SQLState: code, Message: "m"}}}}
		st := runDB(t, f, DBSpec{}, claims(t, "body has row_count == 0"), nil, nil, false)
		if st.Status != "failed" {
			t.Fatalf("%q: setup: a refusal is still failed: %+v", code, st)
		}
		if st.ObservedSQLState != "" {
			t.Errorf("%q is not a SQLSTATE and must not be recorded, got %q", code, st.ObservedSQLState)
		}
	}
	for _, code := range []string{"00000", "ZZZZZ", "09AZ9", "57014", "25006"} {
		f := &fakeDB{results: []fakeResult{{err: &DBServerError{SQLState: code, Message: "m"}}}}
		if st := runDB(t, f, DBSpec{}, claims(t, "body has row_count == 0"), nil, nil, false); st.ObservedSQLState != code {
			t.Errorf("%q is a SQLSTATE and must be recorded, got %q", code, st.ObservedSQLState)
		}
	}
}

// No refusal, no SQLSTATE: a pass, a claim failure, a save failure, the row cap, and every way the database did not
// answer record nothing.
func TestStepCodes_DB_NoRefusalRecordsNothing(t *testing.T) {
	pass := runDB(t, &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PAID"})}}}, DBSpec{},
		claims(t, "body has row_count == 1"), nil, nil, false)
	if pass.Status != "passed" {
		t.Fatalf("setup: pass: %+v", pass)
	}
	assertNoCodes(t, "db pass", pass)

	miss := runDB(t, &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PENDING"})}}}, DBSpec{},
		claims(t, "body has rows.0.status equals PAID"), nil, nil, false)
	if miss.Status != "failed" || len(miss.FailedClaims) != 1 {
		t.Fatalf("setup: claim failure: %+v", miss)
	}
	assertNoCodes(t, "db claim failure", miss)

	save := runDB(t, &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PAID"})}}}, DBSpec{},
		claims(t, "body has row_count == 1"), map[string]scenario.SaveSpec{"x": {Path: "rows.0.nope"}}, nil, false)
	if save.Status != "failed" {
		t.Fatalf("setup: save failure: %+v", save)
	}
	assertNoCodes(t, "db save failure", save)

	over := runDB(t, &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, []any{"a"}, []any{"b"})}}}, DBSpec{MaxRows: 1},
		claims(t, "body has row_count >= 1"), nil, nil, false)
	if over.Status != report.StatusError {
		t.Fatalf("setup: row cap: %+v", over)
	}
	assertNoCodes(t, "db row cap", over)

	conn := runDB(t, &fakeDB{dialErr: errors.New("connection refused"), results: []fakeResult{{}}}, DBSpec{},
		claims(t, "body has row_count >= 1"), nil, nil, false)
	if conn.Status != report.StatusError || !strings.HasPrefix(conn.Observed, "cannot connect to the database") {
		t.Fatalf("setup: connection failure: %+v", conn)
	}
	assertNoCodes(t, "db connection failure", conn)

	for name, qerr := range map[string]error{"connection lost": errors.New("the connection to the database failed"), "deadline": context.DeadlineExceeded} {
		st := runDB(t, &fakeDB{results: []fakeResult{{err: qerr}}}, DBSpec{}, claims(t, "body has row_count >= 1"), nil, nil, false)
		if st.Status != report.StatusError {
			t.Fatalf("setup: %s: %+v", name, st)
		}
		assertNoCodes(t, "db "+name, st)
	}
}
