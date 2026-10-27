package chain

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The logs step's RUNNER, exercised against an httptest Loki through the real obsquery.Loki
// (so the LogQL, the time range, the tenant header and the credential are what really goes on the wire). No real
// Loki, no cluster, and no test waits: the clock and the sleep are injected.

const testCID = "tr-20261011T101530123-LGS-1-abcdef12"

type lokiReq struct {
	query, start, end, limit, tenant, user, pass string
	hasAuth                                      bool
}

// fakeLoki answers each query_range from answers in order (the last one repeats) and records what it was asked.
type fakeLoki struct {
	mu      sync.Mutex
	srv     *httptest.Server
	answers []func(w http.ResponseWriter)
	reqs    []lokiReq
	events  *[]string
}

func newFakeLoki(t *testing.T, events *[]string, answers ...func(w http.ResponseWriter)) *fakeLoki {
	t.Helper()
	f := &fakeLoki{answers: answers, events: events}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		user, pass, ok := r.BasicAuth()
		f.reqs = append(f.reqs, lokiReq{query: q.Get("query"), start: q.Get("start"), end: q.Get("end"), limit: q.Get("limit"),
			tenant: r.Header.Get(obsquery.TenantHeader), user: user, pass: pass, hasAuth: ok})
		if f.events != nil {
			*f.events = append(*f.events, "query")
		}
		i := len(f.reqs) - 1
		if i >= len(f.answers) {
			i = len(f.answers) - 1
		}
		f.answers[i](w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLoki) loki() *obsquery.Loki { return &obsquery.Loki{BaseURL: f.srv.URL} }

func (f *fakeLoki) requests() []lokiReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lokiReq(nil), f.reqs...)
}

// nLines answers with n lines "line-<i>", ts 1000+i.
func nLines(n int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		vals := make([]string, n)
		for i := range vals {
			vals[i] = fmt.Sprintf(`["%d","line-%d"]`, 1000+i, i)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[`+strings.Join(vals, ",")+`]}]}}`)
	}
}

// oneLine answers with exactly one line of the given text.
func oneLine(text string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"status":"success","data":{"result":[{"values":[["1000",%s]]}]}}`, strconv.Quote(text))
	}
}

func httpStatus(code int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { http.Error(w, "server text SERVER-FREE-TEXT", code) }
}

func garbage(w http.ResponseWriter) { fmt.Fprint(w, "this is not json") }

// clock is the injected time: Sleep advances it and records the call.
type clock struct {
	now    time.Time
	sleeps []time.Duration
	events *[]string
}

func (c *clock) Now() time.Time { return c.now }
func (c *clock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	if c.events != nil {
		*c.events = append(*c.events, "sleep "+d.String())
	}
}

var (
	t0 = time.Date(2026, 10, 11, 10, 15, 30, 0, time.UTC) // when the chain was assembled
	t1 = t0.Add(7 * time.Second)                          // the clock at the first query
)

func newClock() *clock { return &clock{now: t1} }

func logsClaims(t *testing.T, bullets ...string) []mcp.BodyAssert {
	t.Helper()
	a, err := scenario.LogsStepClaims(bullets)
	if err != nil {
		t.Fatalf("claims %q: %v", bullets, err)
	}
	return a
}

func logsSpec(c *clock, s LogsSpec) LogsSpec {
	if s.Scope == "" {
		s.Scope = scenario.LogsScopeCorrelation
	}
	s.ChainStart = t0
	s.Now, s.Sleep = c.Now, c.Sleep
	return s
}

func runLogs(f *fakeLoki, spec LogsSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll) report.StepResult {
	return LogsStep("l", spec, want, save, poll, false, f.loki()).Run(testCID, map[string]string{})
}

func TestLogsStep_PassesAndReportsTheCount(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(3))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count >= 1", "body has count == 3", "body has lines.0 containing line-0"), nil, nil)
	if res.Status != "passed" || res.Observed != "log query matched 3 line(s)" {
		t.Fatalf("want passed / `log query matched 3 line(s)`, got %q / %q", res.Status, res.Observed)
	}
	if len(res.AssertionsEnforced) != 3 {
		t.Fatalf("the claims judged are listed, got %v", res.AssertionsEnforced)
	}
}

// ── the exact LogQL ─────────────────────────────────────────────────────────────────────────────────

func TestLogsStep_TheExactLogQLSent(t *testing.T) {
	excl := `{service=~".+",service!~"` + obsquery.ObsInfraExclusion + `"} |= "` + testCID + `"`
	cases := []struct {
		name string
		spec LogsSpec
		vars map[string]string
		want string
	}{
		{"correlation", LogsSpec{}, nil, excl},
		{"correlation + contains", LogsSpec{Contains: "order placed"}, nil, excl + ` |= "order placed"`},
		{"correlation + contains + matching", LogsSpec{Contains: "placed", Matching: `level=(warn|error)`}, nil, excl + ` |= "placed" |~ "level=(warn|error)"`},
		{"source, two labels sorted, + matching", LogsSpec{Scope: scenario.LogsScopeSource,
			Labels: map[string]string{"namespace": "prod", "app": "orders"}, Matching: `ERROR .*`}, nil, `{app="orders",namespace="prod"} |~ "ERROR .*"`},
		{"a saved value in contains is quoted", LogsSpec{Scope: scenario.LogsScopeSource, Labels: map[string]string{"app": "o"}, Contains: "order ${saved.oid}"},
			map[string]string{"oid": `o-"1"\x`}, `{app="o"} |= ` + strconv.Quote(`order o-"1"\x`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeLoki(t, nil, nLines(1))
			vars := c.vars
			if vars == nil {
				vars = map[string]string{}
			}
			res := LogsStep("l", logsSpec(newClock(), c.spec), logsClaims(t, "body has count >= 1"), nil, nil, false, f.loki()).Run(testCID, vars)
			if res.Status != "passed" {
				t.Fatalf("%s: %q", res.Status, res.Observed)
			}
			reqs := f.requests()
			if len(reqs) != 1 || reqs[0].query != c.want {
				t.Fatalf("the decoded query parameter:\n got %v\nwant %s", reqs, c.want)
			}
		})
	}
}

// A label value and a `contains` holding a quote, a backslash, a backtick and a newline: the query stays ONE
// well-formed selector plus filters, and the decoded literals are the values.
func TestLogsStep_HostileLabelValueAndContainsStayOneSelector(t *testing.T) {
	hostile := "a\"b\\c`d\ne} |= \"zzz"
	f := newFakeLoki(t, nil, nLines(1))
	spec := logsSpec(newClock(), LogsSpec{Scope: scenario.LogsScopeSource, Labels: map[string]string{"app": hostile}, Contains: hostile})
	if res := runLogs(f, spec, logsClaims(t, "body has count >= 1"), nil, nil); res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	got := f.requests()[0].query
	if want := `{app=` + strconv.Quote(hostile) + `} |= ` + strconv.Quote(hostile); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("a raw newline reached the query: %q", got)
	}
	// the quoted form decodes back to exactly the value: nothing was lost or reinterpreted
	rest := strings.TrimPrefix(got, `{app=`)
	lit, err := strconv.QuotedPrefix(rest)
	if err != nil {
		t.Fatalf("the label value is not one quoted literal: %v", err)
	}
	if v, _ := strconv.Unquote(lit); v != hostile {
		t.Fatalf("label value decoded to %q", v)
	}
	if !strings.HasPrefix(rest[len(lit):], "} |= ") {
		t.Fatalf("after the label literal comes the closing brace and the filter, got %q", rest[len(lit):])
	}
}

// ── the time range, and the settle ────────────────────────────────────────────────────────────────

func TestLogsStep_TimeRange(t *testing.T) {
	ns := func(tm time.Time) string { return strconv.FormatInt(tm.UnixNano(), 10) }
	cases := []struct {
		name         string
		window       time.Duration
		wantStart    time.Time
		settle       time.Duration
		wantEndAfter time.Duration // end = t1 + the settle that was slept
	}{
		{"default: one minute before the chain started", 0, t0.Add(-time.Minute), 0, 0},
		{"window 10m: back from the moment of the query", 10 * time.Minute, t1.Add(-10 * time.Minute), 0, 0},
		{"window 10m with a settle: back from after the settle", 10 * time.Minute, t1.Add(2*time.Second - 10*time.Minute), 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeLoki(t, nil, nLines(1))
			spec := logsSpec(newClock(), LogsSpec{Window: c.window, Settle: c.settle})
			if res := runLogs(f, spec, logsClaims(t, "body has count >= 1"), nil, nil); res.Status != "passed" {
				t.Fatal(res.Observed)
			}
			r := f.requests()[0]
			if r.start != ns(c.wantStart) || r.end != ns(t1.Add(c.wantEndAfter)) || r.limit != "2000" {
				t.Fatalf("start %s end %s limit %s; want %s..%s limit 2000", r.start, r.end, r.limit, ns(c.wantStart), ns(t1.Add(c.wantEndAfter)))
			}
		})
	}
}

func TestLogsStep_SettleIsWaitedBeforeTheFirstQueryOnly(t *testing.T) {
	var events []string
	clk := newClock()
	clk.events = &events
	f := newFakeLoki(t, &events, nLines(0), nLines(0), nLines(1))
	spec := logsSpec(clk, LogsSpec{Settle: 2 * time.Second})
	res := runLogs(f, spec, logsClaims(t, "body has count >= 1"), nil, &Poll{Timeout: time.Minute, Interval: time.Second})
	if res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if got := strings.Join(events, ","); got != "sleep 2s,query,sleep 1s,query,sleep 1s,query" {
		t.Fatalf("the settle comes once, before the first query: %s", got)
	}
}

func TestLogsStep_NoSettleNoSleep(t *testing.T) {
	clk := newClock()
	f := newFakeLoki(t, nil, nLines(1))
	runLogs(f, logsSpec(clk, LogsSpec{Settle: 0}), logsClaims(t, "body has count >= 1"), nil, nil)
	if len(clk.sleeps) != 0 {
		t.Fatalf("a zero settle sleeps nothing, got %v", clk.sleeps)
	}
}

// ── counts ──────────────────────────────────────────────────────────────────────────────────────────

func TestLogsStep_Counts(t *testing.T) {
	cases := []struct {
		n      int
		claims []string
		status string
		obs    string
	}{
		{0, []string{"body has count == 0"}, "passed", "log query matched 0 line(s)"},
		{0, []string{"body has count >= 1"}, "failed", "log query matched 0 line(s) but"},
		{1, []string{"body has count >= 1", "body has count == 1"}, "passed", "log query matched 1 line(s)"},
		{1, []string{"body has count == 0"}, "failed", "log query matched 1 line(s) but"},
		{1999, []string{"body has count == 1999"}, "passed", "log query matched 1999 line(s)"},
		{2000, []string{"body has count >= 1"}, report.StatusError, "which is as many as one read returns"},
		{2000, []string{"body has count == 2000"}, report.StatusError, "narrow `contains` / `matching` or `window`"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d lines %v", c.n, c.claims), func(t *testing.T) {
			f := newFakeLoki(t, nil, nLines(c.n))
			res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, c.claims...), nil, nil)
			if res.Status != c.status || !strings.Contains(res.Observed, c.obs) {
				t.Fatalf("want %s / %q, got %s / %q", c.status, c.obs, res.Status, res.Observed)
			}
		})
	}
}

// The lossy case is an `error`, never retried by a poll, and the claims are not judged at all.
func TestLogsStep_LossyReadIsAnErrorAndNotRetried(t *testing.T) {
	clk := newClock()
	f := newFakeLoki(t, nil, nLines(2000))
	res := runLogs(f, logsSpec(clk, LogsSpec{}), logsClaims(t, "body has count >= 1"), nil, &Poll{Timeout: time.Minute, Interval: time.Second})
	if res.Status != report.StatusError || len(f.requests()) != 1 || len(res.FailedClaims) != 0 {
		t.Fatalf("lossy: %s, %d requests, failed claims %v", res.Status, len(f.requests()), res.FailedClaims)
	}
}

func TestLogsStep_At25LinesTheDocumentKeeps20(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(25))
	spec := logsSpec(newClock(), LogsSpec{})
	ok := runLogs(f, spec, logsClaims(t, "body has count == 25", "body has lines.0 equals line-0", "body has lines.19 equals line-19"), nil, nil)
	if ok.Status != "passed" {
		t.Fatalf("count is the full 25 and 20 lines are kept: %s / %s", ok.Status, ok.Observed)
	}
	no := runLogs(f, spec, logsClaims(t, "body has lines.20 equals line-20"), nil, nil)
	if no.Status != "failed" {
		t.Fatalf("a 21st line is not in the document: %s / %s", no.Status, no.Observed)
	}
}

func TestLogsStep_AFiveThousandByteLineIsCutOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 2500) // 5000 bytes; 2048 is even, so the cut falls between two runes... shift it by one byte
	long = "x" + long
	f := newFakeLoki(t, nil, oneLine(long))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count == 1"), map[string]scenario.SaveSpec{"l0": {Path: "lines.0"}}, nil)
	if res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	got := res.Captured["l0"]
	if len(got) > scenario.LogsLineMaxBytes || len(got) < scenario.LogsLineMaxBytes-3 {
		t.Fatalf("the line is cut at %d bytes, got %d", scenario.LogsLineMaxBytes, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("the cut split a rune")
	}
	if cutOnRune("abc", 2) != "ab" || cutOnRune("é", 1) != "" || cutOnRune("aé", 2) != "a" {
		t.Fatal("cutOnRune")
	}
}

// ── errors: a log store that cannot answer never makes `count == 0` hold ────────────────────────────

func TestLogsStep_StoreErrorsAreErrorsAndZeroCountDoesNotPass(t *testing.T) {
	down := newFakeLoki(t, nil, nLines(0))
	downURL := down.srv.URL
	down.srv.Close()
	cases := []struct {
		name string
		loki *obsquery.Loki
	}{
		{"Loki down", &obsquery.Loki{BaseURL: downURL}},
		{"HTTP 500", newFakeLoki(t, nil, httpStatus(500)).loki()},
		{"HTTP 401", newFakeLoki(t, nil, httpStatus(401)).loki()},
		{"garbage JSON", newFakeLoki(t, nil, garbage).loki()},
		{"an empty object", newFakeLoki(t, nil, func(w http.ResponseWriter) { fmt.Fprint(w, `{}`) }).loki()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := LogsStep("l", logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count == 0"), nil, nil, false, c.loki).Run(testCID, map[string]string{})
			if res.Status != report.StatusError {
				t.Fatalf("an unanswered log store is `error`, never a pass of `count == 0`: %s / %s", res.Status, res.Observed)
			}
			if !strings.HasPrefix(res.Observed, "the log store did not answer: ") || strings.Contains(res.Observed, "SERVER-FREE-TEXT") {
				t.Fatalf("Observed: %q", res.Observed)
			}
			if len(res.FailedClaims) != 0 || len(res.AssertionsEnforced) != 0 {
				t.Fatalf("the claims were not judged: %+v", res)
			}
		})
	}
}

func TestLogsStep_AnErrorKeepsItsOwnSentenceThroughChainRun(t *testing.T) {
	f := newFakeLoki(t, nil, httpStatus(500))
	step := LogsStep("l", logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count == 0"), nil, nil, false, f.loki())
	res := Run(testCID, []Step{step})
	if res.Status != report.StatusError || len(res.Steps) != 1 || !strings.Contains(res.Steps[0].Observed, "Loki answered HTTP 500") {
		t.Fatalf("chain.Run must not rewrite the logs step's error to 'SUT unreachable': %s %+v", res.Status, res.Steps)
	}
}

// ── poll ────────────────────────────────────────────────────────────────────────────────────────────

func TestLogsStep_PollPassesOnTheThirdAttempt(t *testing.T) {
	clk := newClock()
	f := newFakeLoki(t, nil, nLines(0), nLines(0), nLines(1))
	res := runLogs(f, logsSpec(clk, LogsSpec{}), logsClaims(t, "body has count >= 1"), nil, &Poll{Timeout: time.Minute, Interval: 2 * time.Second})
	if res.Status != "passed" || len(f.requests()) != 3 {
		t.Fatalf("%s after %d requests: %s", res.Status, len(f.requests()), res.Observed)
	}
	if len(clk.sleeps) != 2 || clk.sleeps[0] != 2*time.Second {
		t.Fatalf("the interval is slept between attempts: %v", clk.sleeps)
	}
}

func TestLogsStep_PollTimesOutWithTheLastObservation(t *testing.T) {
	clk := newClock()
	f := newFakeLoki(t, nil, nLines(0))
	res := runLogs(f, logsSpec(clk, LogsSpec{}), logsClaims(t, "body has count >= 1"), nil, &Poll{Timeout: 5 * time.Second, Interval: time.Second})
	if res.Status != "failed" || !strings.Contains(res.Observed, "poll timed out after 5s") || !strings.Contains(res.Observed, "log query matched 0 line(s) but") {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if n := len(f.requests()); n < 5 || n > 7 {
		t.Fatalf("a 5s poll at 1s asks about 6 times, asked %d", n)
	}
}

func TestLogsStep_AnErrorIsNotRetriedByAPoll(t *testing.T) {
	f := newFakeLoki(t, nil, httpStatus(500), nLines(1))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count >= 1"), nil, &Poll{Timeout: time.Minute, Interval: time.Second})
	if res.Status != report.StatusError || len(f.requests()) != 1 {
		t.Fatalf("an error is final: %s after %d requests", res.Status, len(f.requests()))
	}
}

// ── headers, credentials ───────────────────────────────────────────────────────────────────────────

func TestLogsStep_TenantAndBasicAuthAreSentWhenConfigured_CredentialNeverInTheResult(t *testing.T) {
	const cred = "lokiuser:S3cretLokiPw"
	f := newFakeLoki(t, nil, httpStatus(401))
	l := f.loki()
	l.Tenant, l.Credential = "inst-a", cred
	scrub := func(s string) string { return strings.ReplaceAll(s, "S3cretLokiPw", "[credential]") }
	res := LogsStep("l", logsSpec(newClock(), LogsSpec{Scrub: scrub}), logsClaims(t, "body has count == 0"), nil, nil, false, l).Run(testCID, map[string]string{})
	r := f.requests()[0]
	if r.tenant != "inst-a" || !r.hasAuth || r.user != "lokiuser" || r.pass != "S3cretLokiPw" {
		t.Fatalf("tenant %q, basic auth %v %q", r.tenant, r.hasAuth, r.user)
	}
	if res.Status != report.StatusError {
		t.Fatalf("setup: %s", res.Status)
	}
	if b := fmt.Sprintf("%+v", res); strings.Contains(b, "S3cretLokiPw") || strings.Contains(b, "lokiuser") {
		t.Fatalf("the credential is in the result: %s", b)
	}
	// and a line that echoes the credential is scrubbed before it is kept
	f2 := newFakeLoki(t, nil, oneLine("auth failed for lokiuser with S3cretLokiPw"))
	ok := LogsStep("l", logsSpec(newClock(), LogsSpec{Scrub: scrub}), logsClaims(t, "body has count == 1"), map[string]scenario.SaveSpec{"l0": {Path: "lines.0"}}, nil, false, f2.loki()).Run(testCID, map[string]string{})
	if ok.Status != "passed" || strings.Contains(ok.Captured["l0"], "S3cretLokiPw") {
		t.Fatalf("%s: %q", ok.Status, ok.Captured["l0"])
	}
}

func TestLogsStep_NoTenantNoAuthNothingSent(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(1))
	runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count >= 1"), nil, nil)
	if r := f.requests()[0]; r.hasAuth || r.tenant != "" {
		t.Fatalf("unconfigured: no Authorization and no tenant header, got %+v", r)
	}
}

// ── the lines are the system's words ────────────────────────────────────────────────────────────────

func TestLogsStep_LinesAreTokenRedactedBeforeTheyAreKept_TheRunsOwnIdSurvives(t *testing.T) {
	const tok = "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	f := newFakeLoki(t, nil, oneLine("run "+testCID+" used token "+tok+" and Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc"))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count == 1", "body has lines.0 containing "+testCID),
		map[string]scenario.SaveSpec{"l0": {Path: "lines.0"}}, nil)
	if res.Status != "passed" {
		t.Fatalf("%s: %s (the run's own id must survive the redaction)", res.Status, res.Observed)
	}
	if got := res.Captured["l0"]; strings.Contains(got, tok) || strings.Contains(got, "eyJhbGci") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("the kept line must be redacted: %q", got)
	}
}

func TestLogsStep_ClaimMissCarriesNoLineAndNoFilterInObserved_ClaimsAreAuthorOnly(t *testing.T) {
	const word = "SYSTEM-WORDS-7731"
	f := newFakeLoki(t, nil, oneLine("customer said "+word))
	spec := logsSpec(newClock(), LogsSpec{Contains: "FILTER-SENTINEL", Matching: "MATCHING-SENTINEL.*"})
	res := runLogs(f, spec, logsClaims(t, "body has count == 5", "body has lines.0 containing CLAIMED-SENTINEL"), nil, nil)
	if res.Status != "failed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if !strings.Contains(res.Observed, "log query matched 1 line(s) but") || !strings.Contains(res.Observed, chainClaimsNote) {
		t.Fatalf("Observed carries the count and the held-out note, got %q", res.Observed)
	}
	for _, leak := range []string{word, "FILTER-SENTINEL", "MATCHING-SENTINEL", "CLAIMED-SENTINEL", "customer"} {
		if strings.Contains(res.Observed, leak) {
			t.Fatalf("Observed (builder-visible) must never carry %q: %q", leak, res.Observed)
		}
	}
	if len(res.FailedClaims) != 2 {
		t.Fatalf("both claims missed and are listed (author-only), got %+v", res.FailedClaims)
	}
}

// ── inputs ──────────────────────────────────────────────────────────────────────────────────────────

func TestLogsStep_AnUnsavedVariableInContainsIsNotFiredAndNothingIsQueried(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(1))
	st := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "order ${saved.oid}"}), logsClaims(t, "body has count >= 1"), nil, nil, false, f.loki())
	if err := st.Needs(map[string]string{}); err == nil || !strings.Contains(err.Error(), "oid") {
		t.Fatalf("Needs must name the unsaved variable, got %v", err)
	}
	if res := st.Run(testCID, map[string]string{}); res.Status != "failed" || len(f.requests()) != 0 {
		t.Fatalf("%s, %d requests", res.Status, len(f.requests()))
	}
}

// ⛔ a filter that bound to nothing would match every line.
func TestLogsStep_ContainsThatBindsToNothingIsNotQueried(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(5))
	st := LogsStep("l", logsSpec(newClock(), LogsSpec{Contains: "${saved.oid}"}), logsClaims(t, "body has count >= 1"), nil, nil, false, f.loki())
	res := st.Run(testCID, map[string]string{"oid": ""})
	if res.Status != "failed" || !strings.Contains(res.Observed, "`contains` is empty once") || len(f.requests()) != 0 {
		t.Fatalf("%s: %s (%d requests)", res.Status, res.Observed, len(f.requests()))
	}
}

func TestLogsStep_SourceWithAnEmptyLabelSetIsNotQueried(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(5))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{Scope: scenario.LogsScopeSource, Contains: "x"}), logsClaims(t, "body has count >= 1"), nil, nil)
	if res.Status != "failed" || len(f.requests()) != 0 {
		t.Fatalf("%s: %s (%d requests)", res.Status, res.Observed, len(f.requests()))
	}
}

func TestLogsStep_SavesFromTheDocument(t *testing.T) {
	f := newFakeLoki(t, nil, nLines(4))
	res := runLogs(f, logsSpec(newClock(), LogsSpec{}), logsClaims(t, "body has count >= 1"), map[string]scenario.SaveSpec{"n": {Path: "count"}}, nil)
	if res.Status != "passed" || res.Captured["n"] != "4" {
		t.Fatalf("%s %v", res.Status, res.Captured)
	}
}
