package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// A polled http step declares its own `poll.timeout` (up to 10 minutes), but a
// chain's `## TIMEOUT` is capped at 120s and the whole-chain backstop was TIMEOUT x step count. A
// poll longer than backstop + chain.stepGrace (5s, fixed in this package) was abandoned and reported
// as a chain TIMEOUT instead of its own verdict. The backstop must leave room for each polled step's
// declared poll timeout.
//
// Shape: one step, TIMEOUT 200ms (old backstop 200ms, abandoned at 5.2s), poll timeout 9s, and the
// SUT turns healthy at ~5.6s — past the old abandon point, well inside the declared poll.
func TestRunChainScenario_PollLongerThanTimeoutTimesStepsIsNotAbandoned(t *testing.T) {
	const healthyAfter = 5600 * time.Millisecond
	born := time.Now()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if time.Since(born) < healthyAfter {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	trig := `{"steps":[{"type":"http","name":"slow","method":"GET","url":"${BASE}/ready",` +
		`"poll":{"timeout":"9s","interval":"250ms"}}]}`
	expect := "### Runnable\n- step slow: status=200\n"
	s := scenario.Parse(chainRunMD(srv.URL, trig, expect))
	s.Timeout = "200ms"

	res := runChainScenario(&config.Config{}, s, "tr-poll-backstop", "testkit/ui")

	if res.Status != "passed" {
		obs := ""
		if len(res.Steps) > 0 {
			obs = res.Steps[0].Observed
		}
		t.Fatalf("status=%q (want passed): the SUT was healthy at %s, inside the step's declared 9s poll, "+
			"but the chain backstop (TIMEOUT x steps = 200ms + 5s grace) abandoned it; hits=%d step observed: %q failure=%+v",
			res.Status, healthyAfter, atomic.LoadInt32(&hits), obs, res.Failure)
	}
	if strings.Contains(strings.ToLower(res.Steps[0].Observed), "timeout") {
		t.Errorf("a passed poll should not report a timeout: %q", res.Steps[0].Observed)
	}
}

// The contribution itself, so the arithmetic is pinned without waiting on a clock.
func TestChainBackstop_PollTimeoutAddsToThePolledStepOnly(t *testing.T) {
	const to = 100 * time.Millisecond
	cases := []struct {
		name  string
		polls []time.Duration // one entry per step; 0 = not polled
		want  time.Duration
	}{
		{"no polls: TIMEOUT x steps, unchanged", []time.Duration{0, 0, 0}, 3 * to},
		{"zero steps still gets one TIMEOUT", nil, to},
		{"even a short poll adds its timeout to its own step", []time.Duration{50 * time.Millisecond, 0}, 2*to + 50*time.Millisecond},
		{"a long poll replaces its step's TIMEOUT with poll+TIMEOUT", []time.Duration{0, 10 * time.Second, 0}, 2*to + 10*time.Second + to},
		{"two polls both counted", []time.Duration{time.Second, time.Second}, 2 * (time.Second + to)},
	}
	for _, c := range cases {
		if got := chainBackstop(to, c.polls); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// Every step type that takes `poll` counts, not only http: db, logs and mcp poll through their own
// runners but run under the same whole-chain backstop.
func TestStepPollTimeouts_EveryPollingStepTypeCounts(t *testing.T) {
	specs, err := parseChainSpec(`{"steps":[
	  {"type":"http","name":"h","method":"GET","url":"http://x/a","poll":{"timeout":"1m","interval":"1s"}},
	  {"type":"db","name":"d","poll":{"timeout":"2m","interval":"1s"}},
	  {"type":"logs","name":"l","poll":{"timeout":"3m","interval":"1s"}},
	  {"type":"mcp","name":"m","tool":"t","poll":{"timeout":"4m","interval":"1s"}},
	  {"type":"http","name":"once","method":"GET","url":"http://x/b"},
	  {"type":"http","name":"bad","method":"GET","url":"http://x/c","poll":{"timeout":"soon","interval":"1s"}}
	]}`, "tr-test")
	if err != nil {
		t.Fatal(err)
	}
	got := stepPollTimeouts(specs)
	want := []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d (%s): got %s, want %s", i, specs[i].Type, got[i], want[i])
		}
	}
}
