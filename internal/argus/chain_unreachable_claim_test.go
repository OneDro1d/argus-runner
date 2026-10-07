package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// — the `unreachable` claim, end to end: scenario.Parse -> runChainScenario ->
// chain.UnreachableHTTPStep -> chain.Run, against local servers only.

func unreachableChain(alive, blocked string) string {
	return `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"` + alive + `/ok"},
  {"type":"http","name":"blocked","method":"GET","url":"` + blocked + `/x"}
]}`
}

func closedBase(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func stepStatus(res report.ScenarioResult, name string) string {
	for _, s := range res.Steps {
		if s.Name == name {
			return s.Status
		}
	}
	return "<absent>"
}

func TestChainRun_UnreachableClaimPassesOnARefusedConnectionAfterAPassingStep(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer alive.Close()
	expect := "### Runnable\n- step alive: status=200\n- step blocked: unreachable\n"
	s := scenario.Parse(chainRunMD("", unreachableChain(alive.URL, closedBase(t)), expect))
	res := runChainScenario(&config.Config{}, s, "tr-unreach-ok", "testkit/ui")
	if res.Status != "passed" || stepStatus(res, "blocked") != "passed" {
		t.Fatalf("want passed; got %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
}

func TestChainRun_UnreachableClaimFailsWhenTheTargetAnswers(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer alive.Close()
	forbid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
	defer forbid.Close()
	expect := "### Runnable\n- step alive: status=200\n- step blocked: unreachable\n"
	s := scenario.Parse(chainRunMD("", unreachableChain(alive.URL, forbid.URL), expect))
	res := runChainScenario(&config.Config{}, s, "tr-unreach-403", "testkit/ui")
	if res.Status != "failed" || stepStatus(res, "blocked") != "failed" {
		t.Fatalf("a 403 means the network let it through: want failed; got %q %+v", res.Status, res.Steps)
	}
}

func TestChainRun_UnreachableClaimIsNotMeasuredWhenThePositiveStepFailed(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer down.Close()
	expect := "### Runnable\n- step alive: status=200\n- step blocked: unreachable\n"
	s := scenario.Parse(chainRunMD("", unreachableChain(down.URL, closedBase(t)), expect))
	res := runChainScenario(&config.Config{}, s, "tr-unreach-gate", "testkit/ui")
	if got := stepStatus(res, "blocked"); got != report.StepNotMeasured || res.Status == "passed" {
		t.Fatalf("want not-measured and a non-passing scenario; got %q %q %+v", got, res.Status, res.Steps)
	}
}

// The executor refuses what the validator refuses, with the same reason text.
func TestUnreachableClaim_ValidatorAndExecutorAgreeOnACombinedClaim(t *testing.T) {
	_, _, execErr := httpStepExpect([]string{"unreachable", "status=200"})
	if execErr == nil {
		t.Fatal("the executor must refuse a combined unreachable claim")
	}
	md := chainRunMD("", unreachableChain("http://x", "http://y"),
		"### Runnable\n- step alive: status=200\n- step blocked: unreachable\n- step blocked: status=200\n")
	_, errs := scenario.Validate(md)
	want := `step "blocked": ` + execErr.Error()
	for _, e := range errs {
		if e.Message == want {
			return
		}
	}
	t.Fatalf("validator must say %q; got %v", want, errs)
}

// A file that reaches a run without the validator is refused at preflight too (the second door).
func TestChainRun_UnreachableWithPollOrAlwaysIsRefusedAtPreflight(t *testing.T) {
	for _, extra := range []string{`,"poll":{"timeout":"2s","interval":"1s"}`, `,"always":true`} {
		trig := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://127.0.0.1:1/ok"},
  {"type":"http","name":"blocked","method":"GET","url":"http://127.0.0.1:1/x"` + extra + `}
]}`
		s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step alive: status=200\n- step blocked: unreachable\n"))
		res := runChainScenario(&config.Config{}, s, "tr-unreach-pre", "testkit/ui")
		if res.Status == "passed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "unreachable") {
			t.Errorf("%s: want a preflight refusal naming the claim; got %q %+v", extra, res.Status, res.Failure)
		}
	}
}
