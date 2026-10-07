package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D20 gate — the status claim is enforced on its own: a response whose BODY satisfies every
// body assertion still fails when its status is not the declared one (a 500 that happens to echo
// the expected field must never read as a created workspace). Kills the "status ignored" mutant,
// which every other http test survived.
func TestHTTPStep_WrongStatusFailsEvenWhenTheBodyMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"name":"argus-x","status":"running"}`))
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"create","method":"POST","url":"${BASE}/workspaces"}]}`
	expect := "### Runnable\n- step create: status=201\n- step create: body has status containing running\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-status-only", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("status 500 against a declared 201 must fail even with a matching body, got %q %+v", res.Status, res.Steps)
	}
	if len(res.Steps) != 1 || res.Steps[0].Observed == "" {
		t.Fatalf("want one failed step naming the status, got %+v", res.Steps)
	}
	if got := res.Steps[0].Observed; !strings.Contains(got, "500") || !strings.Contains(got, "201") {
		t.Errorf("the failure must name the observed 500 and the declared 201: %q", got)
	}
}

// AC-D20 gate — the leak case. The workspace IS created, then the SUT stops answering (the
// connection drops mid-poll). chain.Run's VR12-CH2 stop marks every later step not-measured and
// fires nothing — which, for the `always` cleanup step, would leave the created resource behind
// on a shared SUT. An `always` step must still fire after the stop, with the id it was handed.
func TestHTTPStep_AlwaysStepStillFiresAfterTheSUTStopsAnswering(t *testing.T) {
	var deletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/workspaces":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"ws-9"}`))
		case r.Method == "GET":
			// no answer at all: drop the connection, so the client sees a transport error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("httptest server must support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		case r.Method == "DELETE" && r.URL.Path == "/workspaces/ws-9":
			atomic.AddInt32(&deletes, 1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	trig := `{"steps":[
		{"type":"http","name":"create","method":"POST","url":"${BASE}/workspaces","save":{"id":"id"}},
		{"type":"http","name":"wait","method":"GET","url":"${BASE}/workspaces/${saved.id}"},
		{"type":"http","name":"read-again","method":"GET","url":"${BASE}/workspaces/${saved.id}"},
		{"type":"http","name":"delete","method":"DELETE","url":"${BASE}/workspaces/${saved.id}","always":true}
	]}`
	expect := "### Runnable\n- step create: status=201\n- step wait: status=200\n- step read-again: status=200\n- step delete: status=200\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-always-stop", "testkit/ui")

	if got := atomic.LoadInt32(&deletes); got != 1 {
		t.Fatalf("the always step must reach the SUT exactly once after the stop, DELETE count = %d; steps: %+v", got, res.Steps)
	}
	if len(res.Steps) != 4 {
		t.Fatalf("want 4 steps, got %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[2].Status != report.StepNotMeasured {
		t.Errorf("a NON-always step after the stop must still be not-measured and unfired, got %q", res.Steps[2].Status)
	}
	if res.Status == "passed" {
		t.Errorf("a chain whose SUT stopped answering must not pass, got %q", res.Status)
	}
}
