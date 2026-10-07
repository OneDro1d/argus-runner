package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D20 — the "http" chain step: a multi-request REST lifecycle a chain sequences alongside its
// `mcp`/`ui` steps (the concrete case: a Coder workspace's create/poll/stop/delete). These tests
// exercise the whole path — scenario.Parse -> runChainScenario -> chain.HTTPStep -> chain.Run —
// against local httptest servers only. No mcp target is needed for a pure-http chain, so
// &config.Config{} is used throughout, matching the existing chain_scenario_test.go convention.

// save -> `${saved.id}` is used in a later step's url.
func TestHTTPStep_SaveIsUsedInLaterStepURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/workspaces":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"ws-77"}`))
		case r.Method == "GET" && r.URL.Path == "/workspaces/ws-77":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"running"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	trig := `{"steps":[
		{"type":"http","name":"create","method":"POST","url":"${BASE}/workspaces","save":{"id":"id"}},
		{"type":"http","name":"read","method":"GET","url":"${BASE}/workspaces/${saved.id}"}
	]}`
	expect := "### Runnable\n- step create: status=201\n- step read: status=200\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-http-save", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %q %+v", res.Status, res.Failure)
	}
	if len(res.Steps) != 2 || res.Steps[1].Status != "passed" {
		t.Fatalf("the read step must see the saved id resolved into its url: %+v", res.Steps)
	}
}

// poll succeeds on the 3rd attempt.
func TestHTTPStep_PollSucceedsOnThirdAttempt(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		status := "pending"
		if c >= 3 {
			status = "running"
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"latest_build":{"status":"` + status + `"}}`))
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"poll","method":"GET","url":"${BASE}/w",` +
		`"poll":{"timeout":"2s","interval":"10ms"}}]}`
	expect := "### Runnable\n- step poll: status=200\n- step poll: body has latest_build.status containing running\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-poll3", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("poll must succeed once the 3rd attempt reports running: %q %+v", res.Status, res.Failure)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", got)
	}
}

// poll times out and is reported as a failure naming the last observed status.
func TestHTTPStep_PollTimesOutNamesLastObservedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"latest_build":{"status":"pending"}}`))
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"poll","method":"GET","url":"${BASE}/w",` +
		`"poll":{"timeout":"60ms","interval":"15ms"}}]}`
	expect := "### Runnable\n- step poll: status=200\n- step poll: body has latest_build.status containing running\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-polltimeout", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("a poll that never satisfies its claim must fail, got %q", res.Status)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(res.Steps))
	}
	obs := res.Steps[0].Observed
	if !strings.Contains(obs, "poll timed out") {
		t.Errorf("must say it timed out: %q", obs)
	}
	if !strings.Contains(obs, "200") {
		t.Errorf("must name the last observed status (200): %q", obs)
	}
}

// an always-step runs after a failed step, and the chain verdict stays failed.
func TestHTTPStep_AlwaysStepRunsAfterAFailedStepAndVerdictStaysFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/workspaces":
			// create genuinely fails — no id is ever captured for the cleanup step to use.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"deleted":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	trig := `{"steps":[
		{"type":"http","name":"create","method":"POST","url":"${BASE}/workspaces","save":{"id":"id"}},
		{"type":"http","name":"delete","method":"DELETE","url":"${BASE}/workspaces/${saved.id}","always":true}
	]}`
	expect := "### Runnable\n- step create: status=201\n- step delete: status=200\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-always", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("the chain verdict must stay failed, got %q", res.Status)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(res.Steps))
	}
	if res.Steps[0].Status != "failed" {
		t.Errorf("create must be the named failure, got %q", res.Steps[0].Status)
	}
	// The proof this test exists for: the always step must have GENUINELY FIRED — not silently
	// skipped as not-measured — even though `create` never captured an id for it to bind.
	if res.Steps[1].Status == report.StepNotMeasured {
		t.Fatalf("an `always` step must attempt to run even when an earlier step's capture never "+
			"happened, got %q", res.Steps[1].Status)
	}
	if res.Steps[1].Status != report.StepRanAfterFailureFailed {
		t.Errorf("want %q (it fires but cannot bind ${saved.id}, so it fails too), got %q",
			report.StepRanAfterFailureFailed, res.Steps[1].Status)
	}
}

// a header carrying a secret from env never appears in the ScenarioResult/report JSON.
func TestHTTPStep_SecretHeaderNeverAppearsInTheReport(t *testing.T) {
	const secret = "sekret-tok-9f3e7c21"
	os.Setenv("AC_D20_TEST_SECRET", secret)
	t.Cleanup(func() { os.Unsetenv("AC_D20_TEST_SECRET") })

	// Authorization is deliberately NOT used here: scenario.AuthorHeaders (which feeds this
	// engine's scenario-level default headers, headers.go:resolvedHeaders) excludes it on
	// purpose — it rides a separate path (props["auth.header"], the 4.8 JMeter override) that is
	// out of scope for AC-D20. X-Secret-Token exercises the SAME "never leak a header value"
	// promise through the ordinary custom-header path every other TRIGGER header takes.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Secret-Token")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	md := strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CHN-SECRET",
		"- **Layer**: Permissions",
		"- **Tags**: chain", "",
		"## TRIGGER",
		"POST `chain`",
		"X-Secret-Token: Bearer ${AC_D20_TEST_SECRET}", "",
		"```json",
		`{"steps":[{"type":"http","name":"call","method":"GET","url":"` + srv.URL + `/x"}]}`,
		"```", "",
		"## EXPECT",
		"### Runnable\n- step call: status=200\n", "",
		"## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	s := scenario.Parse(md)
	res := runChainScenario(&config.Config{}, s, "tr-secret", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %q %+v", res.Status, res.Failure)
	}
	if gotAuth != "Bearer "+secret {
		t.Fatalf("the header must actually reach the SUT (proves it was really sent): got %q", gotAuth)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("the secret header value must never appear in the marshaled report: %s", b)
	}
}

// #606 / — every request a chain `http` step sends carries X-Correlation-Id equal to
// the correlation id the report prints for the run, including each re-attempt of a polled step.
func TestHTTPStep_SendsXCorrelationIdEqualToTheReportedOne(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"="+r.Header.Get("X-Correlation-Id"))
		n := len(seen)
		mu.Unlock()
		status := "pending"
		if n >= 4 {
			status = "running"
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"latest_build":{"status":"` + status + `"}}`))
	}))
	defer srv.Close()

	trig := `{"steps":[
		{"type":"http","name":"one","method":"GET","url":"${BASE}/one"},
		{"type":"http","name":"poll","method":"GET","url":"${BASE}/poll","poll":{"timeout":"2s","interval":"10ms"}}
	]}`
	expect := "### Runnable\n- step one: status=200\n- step poll: status=200\n- step poll: body has latest_build.status containing running\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-corr-606", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %q %+v", res.Status, res.Failure)
	}
	if res.CorrelationID == "" {
		t.Fatalf("the result must print a correlation id")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 3 {
		t.Fatalf("want at least 3 requests (one + a polled re-attempt), got %v", seen)
	}
	for _, got := range seen {
		_, hdr, _ := strings.Cut(got, "=")
		if hdr != res.CorrelationID {
			t.Errorf("request %q: X-Correlation-Id must equal the reported %q", got, res.CorrelationID)
		}
	}
}

// #606, the runtime half: a step header that names X-Correlation-Id in another letter case (refused at
// validate time, but a run does not re-validate) must never reach the SUT. The headers are applied in map
// order, so without the runner dropping the variant this test would fail only some of the time: it runs
// the step many times.
func TestHTTPStep_AnAuthorCorrelationHeaderInAnyCaseNeverWins(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.Join(r.Header.Values("X-Correlation-Id"), ","))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"one","method":"GET","url":"${BASE}/one",` +
		`"headers":{"x-correlation-id":"author-value","X-CORRELATION-ID":"author-value-2"}}]}`
	expect := "### Runnable\n- step one: status=200\n"
	for i := 0; i < 40; i++ {
		s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
		res := runChainScenario(&config.Config{}, s, "tr-corr-606b", "testkit/ui")
		if res.Status != "passed" {
			t.Fatalf("expected passed, got %q %+v", res.Status, res.Failure)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, got := range seen {
		if got != "tr-corr-606b" {
			t.Fatalf("request %d carried X-Correlation-Id %q, want only the runner's tr-corr-606b", i, got)
		}
	}
}
