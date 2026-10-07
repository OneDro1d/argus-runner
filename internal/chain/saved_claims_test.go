package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// / — a numeric claim against a saved value, ${saved.<var>} bound
// into an http step's claims, and `save` from a text body by regex. Every test here runs a real
// httptest server through chain.Run, so the binding happens where it happens in production.

// secretN is the saved value. No text a builder can read may ever contain it.
const secretN = "987654"

// seqServer answers each path from a list of bodies, one per request; the last body repeats.
type seqServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newSeqServer(t *testing.T, seqs map[string][]string) *seqServer {
	t.Helper()
	s := &seqServer{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		i := s.hits[r.URL.Path]
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		list := seqs[r.URL.Path]
		if len(list) == 0 {
			w.WriteHeader(404)
			return
		}
		if i >= len(list) {
			i = len(list) - 1
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(list[i]))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *seqServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func saveN(path string) map[string]scenario.SaveSpec {
	return map[string]scenario.SaveSpec{"n": {Path: path}}
}

func readStep(url string, save map[string]scenario.SaveSpec) Step {
	return HTTPStep("read1", "GET", url+"/a", nil, "", 200, nil, save, nil, false, nil, &scenario.MoneySpendLedger{})
}

func gtSaved(url, path string, poll *Poll, field string) Step {
	want := []mcp.BodyAssert{{Field: field, Op: mcp.BodyGTOp, Value: "${saved.n}"}}
	return HTTPStep("read2", "GET", url+path, nil, "", 200, want, nil, poll, false, nil, &scenario.MoneySpendLedger{})
}

func assertNoValue(t *testing.T, res report.ScenarioResult, secret string) {
	t.Helper()
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), secret) {
		t.Errorf("a builder-readable report text contains the saved value %q: %s", secret, b)
	}
}

// assertObservedOnly: — a numeric threshold is not a credential, so the number the
// counter held IS shown as the failed claim's observed value, and nowhere else (the claim stays the
// placeholder, the step's own text names no value).
func assertObservedOnly(t *testing.T, st report.StepResult) {
	t.Helper()
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Observed != secretN || strings.Contains(got[0].Claim, secretN) {
		t.Errorf("want one failed claim with observed %s and the claim as written, got %+v", secretN, got)
	}
	if strings.Contains(st.Observed, secretN) {
		t.Errorf("the step's own observed text names the saved value: %q", st.Observed)
	}
	// failed_claims (author-only) is the ONE place the number may appear: every other field of the step,
	// assertions_enforced and expected included, is still held to the old whole-step rule.
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "failed_claims")
	if rest, _ := json.Marshal(m); strings.Contains(string(rest), secretN) {
		t.Errorf("the saved value is printed outside failed_claims: %s", rest)
	}
}

func TestSavedThreshold_CounterRose_Passes(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"count": 987654}`}, "/b": {`{"count": 987660}`}})
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), gtSaved(srv.URL, "/b", nil, "count")})
	if res.Status != "passed" {
		t.Fatalf("the counter rose, the chain must pass: %+v %+v", res, res.Steps)
	}
	enf := strings.Join(res.Steps[1].AssertionsEnforced, "; ")
	if !strings.Contains(enf, "field count > ${saved.n}") {
		t.Errorf("the enforced list must show the claim as written, got %q", enf)
	}
	assertNoValue(t, res, secretN)
}

func TestSavedThreshold_CounterDidNotRise_Fails(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"count": 987654}`}, "/b": {`{"count": 987654}`}})
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), gtSaved(srv.URL, "/b", nil, "count")})
	if res.Status != "failed" || res.Steps[1].Status != "failed" {
		t.Fatalf("an unchanged counter is not > the saved value: %+v %+v", res, res.Steps)
	}
	assertObservedOnly(t, res.Steps[1])
}

func TestSavedThreshold_Polled_RisesOnThirdAttempt_Passes(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"count": 987654}`},
		"/b": {`{"count": 987654}`, `{"count": 987654}`, `{"count": 987700}`}})
	poll := &Poll{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), gtSaved(srv.URL, "/b", poll, "count")})
	if res.Status != "passed" {
		t.Fatalf("the polled step must pass once the counter rises: %+v %+v", res, res.Steps)
	}
	if got := srv.count("/b"); got != 3 {
		t.Errorf("want 3 attempts, got %d", got)
	}
	assertNoValue(t, res, secretN)
}

func TestSavedThreshold_Polled_NeverRises_FailsAfterTimeout(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"count": 987654}`}, "/b": {`{"count": 987654}`}})
	poll := &Poll{Timeout: 150 * time.Millisecond, Interval: 10 * time.Millisecond}
	res := Run("cid", []Step{readStep(srv.URL, saveN("count")), gtSaved(srv.URL, "/b", poll, "count")})
	if res.Status != "failed" || !strings.Contains(res.Steps[1].Observed, "poll timed out") {
		t.Fatalf("a counter that never rises must time the poll out: %+v", res.Steps[1])
	}
	assertObservedOnly(t, res.Steps[1])
}

func TestSavedThreshold_NonNumericSavedValue_FailsByName(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"state": "ready-987654"}`}, "/b": {`{"count": 5}`}})
	res := Run("cid", []Step{readStep(srv.URL, saveN("state")), gtSaved(srv.URL, "/b", nil, "count")})
	st := res.Steps[1]
	if st.Status != "failed" {
		t.Fatalf("a non-numeric saved value must FAIL the step: %+v", st)
	}
	if !strings.Contains(st.Observed, "the saved value of `n` is not a number") {
		t.Errorf("the failure must say so by name, got %q", st.Observed)
	}
	if srv.count("/b") != 0 {
		t.Errorf("nothing may be sent once the claim cannot be bound; /b was hit %d time(s)", srv.count("/b"))
	}
	assertNoValue(t, res, "ready-987654")
}

func TestSavedThreshold_VariableNobodySaved(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": 5}`}})
	res := Run("cid", []Step{gtSaved(srv.URL, "/b", nil, "count")})
	st := res.Steps[0]
	if st.Status == "passed" || !strings.Contains(st.Observed, "saved.n") {
		t.Fatalf("an unsaved variable must not pass and must be named: %+v", st)
	}
	if srv.count("/b") != 0 {
		t.Error("the request must not be sent")
	}
	// an `always` step skips the not-measured gate: it must FAIL by name at run time instead.
	want := []mcp.BodyAssert{{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"}}
	alw := HTTPStep("c", "GET", srv.URL+"/b", nil, "", 200, want, nil, nil, true, nil, &scenario.MoneySpendLedger{})
	got := alw.Run("cid", map[string]string{})
	if got.Status != "failed" || !strings.Contains(got.Observed, "saved.n") {
		t.Errorf("an always step referencing an unsaved variable must fail by name: %+v", got)
	}
}

func TestSavedClaimValue_ContainsIsBound(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {`{"id": "abc123"}`}, "/b": {`{"name": "item-abc123"}`}, "/c": {`{"name": "item-zzz"}`}})
	mk := func(path string) Step {
		want := []mcp.BodyAssert{{Field: "name", Op: mcp.BodyContainsOp, Value: "${saved.id}"}}
		return HTTPStep("read2", "GET", srv.URL+path, nil, "", 200, want, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	}
	save := map[string]scenario.SaveSpec{"id": {Path: "id"}}
	ok := Run("cid", []Step{readStep(srv.URL, save), mk("/b")})
	if ok.Status != "passed" {
		t.Fatalf("`body has name containing ${saved.id}` must compare against the saved value: %+v", ok.Steps)
	}
	if enf := strings.Join(ok.Steps[1].AssertionsEnforced, "; "); strings.Contains(enf, "abc123") || !strings.Contains(enf, "${saved.id}") {
		t.Errorf("the enforced list must show the claim as written, got %q", enf)
	}
	bad := Run("cid", []Step{readStep(srv.URL, save), mk("/c")})
	if bad.Status != "failed" {
		t.Fatalf("a body that lacks the saved value must fail: %+v", bad.Steps)
	}
	assertNoValue(t, bad, "abc123")
}

// ── P4 — regex save ───────────────────────────────────────────────────────────────────────────────

const promText = "# HELP msgbus_deduped_total dedups\n# TYPE msgbus_deduped_total counter\nmsgbus_deduped_total 42\nmsgbus_deduped_total 99\n"

func regexSave(re string) map[string]scenario.SaveSpec {
	return map[string]scenario.SaveSpec{"n": {Regex: re}}
}

func TestRegexSave_TakesTheFirstCaptureGroupOfTheFirstMatch(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {promText}})
	st := readStep(srv.URL, regexSave(`msgbus_deduped_total (\d+)`)).Run("cid", map[string]string{})
	if st.Status != "passed" || st.Captured["n"] != "42" {
		t.Fatalf("want n=42 (group 1 of the first match), got %+v", st)
	}
}

func TestRegexSave_NoMatchFailsByName(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {promText}})
	st := readStep(srv.URL, regexSave(`other_total (\d+)`)).Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.Contains(st.Observed, "save failed: the regex for `n` matched nothing") {
		t.Fatalf("got %+v", st)
	}
}

func TestRegexSave_InvalidRegexFailsByNameAtRunTime(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/a": {promText}})
	for _, re := range []string{`total (`, `total \d+`, `(total) (\d+)`} {
		st := readStep(srv.URL, regexSave(re)).Run("cid", map[string]string{})
		if st.Status != "failed" || !strings.Contains(st.Observed, "save failed") || !strings.Contains(st.Observed, `"n"`) {
			t.Errorf("regex %q: a bad regex must fail by name even if validation was bypassed: %+v", re, st)
		}
	}
}

// the counter story end to end: read a Prometheus counter, read it again, it must have gone up.
func TestRegexSave_ThenNumericAgainstSaved_EndToEnd(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{
		"/a": {"msgbus_deduped_total 10\n"},
		"/b": {`{"msgbus_deduped_total": 11}`}})
	res := Run("cid", []Step{
		readStep(srv.URL, regexSave(`msgbus_deduped_total (\d+)`)),
		gtSaved(srv.URL, "/b", nil, "msgbus_deduped_total")})
	if res.Status != "passed" {
		t.Fatalf("%+v", res.Steps)
	}
}

func TestRegexSave_OnAnMCPStepReadsTheContentText(t *testing.T) {
	srv := fakeCaptureMCP(t, promText)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	res := Run("cid", []Step{
		MCPStep("scrape", cl, "metrics", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, regexSave(`msgbus_deduped_total (\d+)`)),
	})
	if res.Status != "passed" || res.Steps[0].Captured["n"] != "42" {
		t.Fatalf("mcp regex save: %+v", res.Steps[0])
	}
	none := Run("cid", []Step{
		MCPStep("scrape", cl, "metrics", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, regexSave(`absent (\d+)`)),
	})
	if none.Status != "failed" || !strings.Contains(none.Steps[0].Observed, "the regex for `n` matched nothing") {
		t.Fatalf("mcp regex save no-match: %+v", none.Steps[0])
	}
}

// the numeric comparison also works on an mcp step, bound from the store.
func TestSavedThreshold_OnAnMCPStep(t *testing.T) {
	srv := fakeCaptureMCP(t, `{"count": 12}`)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	want := mcp.Expect{ErrorPlane: mcp.PlaneNone,
		Body: []mcp.BodyAssert{{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"}}}
	step := MCPStep("b", cl, "t", `{}`, want, nil)
	if got := step.Run("cid", map[string]string{"n": "11"}); got.Status != "passed" {
		t.Fatalf("12 > 11: %+v", got)
	}
	got := step.Run("cid", map[string]string{"n": "12"})
	if got.Status != "failed" {
		t.Fatalf("12 > 12 must fail: %+v", got)
	}
	got = step.Run("cid", map[string]string{"n": "soon-987654"})
	if got.Status != "failed" || !strings.Contains(got.Observed, "the saved value of `n` is not a number") ||
		strings.Contains(got.Observed, "987654") {
		t.Fatalf("mcp: non-numeric saved value: %+v", got)
	}
	if enf := strings.Join(step.Run("cid", map[string]string{"n": "11"}).AssertionsEnforced, ";"); !strings.Contains(enf, "${saved.n}") || strings.Contains(enf, "11") {
		t.Errorf("mcp: enforced must show the claim as written, got %q", enf)
	}
}
