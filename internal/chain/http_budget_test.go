package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// GitHub #621: a chain `http` request was cut at a fixed 30 s whatever the chain's
// `## TIMEOUT` said. These tests scale the durations down (seconds, not 45 s) against a local server
// that sleeps.

func slowServer(t *testing.T, d time.Duration) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			w.WriteHeader(200)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func slowStep(url string) Step {
	return HTTPStep("slow", "GET", url, nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{})
}

func TestHTTPStep_SlowResponseInsideTheChainBudgetPasses(t *testing.T) {
	ts := slowServer(t, 1*time.Second)
	res := Run("b621-ok", []Step{slowStep(ts.URL)}, 3*time.Second)
	if len(res.Steps) != 1 || res.Steps[0].Status != "passed" {
		t.Fatalf("steps = %+v, want one passed step (1 s response inside a 3 s budget)", res.Steps)
	}
}

func TestHTTPStep_RequestCutOffByTheChainDeadlineNamesTheDeadlineAndElapsed(t *testing.T) {
	old := stepGrace
	stepGrace = 200 * time.Millisecond
	t.Cleanup(func() { stepGrace = old })
	ts := slowServer(t, 3*time.Second)

	start := time.Now()
	res := Run("b621-cut", []Step{slowStep(ts.URL)}, 600*time.Millisecond)
	took := time.Since(start)

	if len(res.Steps) != 1 {
		t.Fatalf("steps = %+v, want one", res.Steps)
	}
	s := res.Steps[0]
	if s.Status != "failed" {
		t.Fatalf("status = %q (%s), want failed", s.Status, s.Observed)
	}
	for _, want := range []string{"cut off by the chain's ## TIMEOUT deadline", "after 0.", "context deadline exceeded"} {
		if !strings.Contains(s.Observed, want) {
			t.Fatalf("observed %q lacks %q", s.Observed, want)
		}
	}
	if strings.Contains(s.Observed, "SUT unreachable") || strings.Contains(s.Observed, "did not return within") {
		t.Fatalf("observed %q must name the deadline cut, not unreachable / an abandoned call", s.Observed)
	}
	if took > 600*time.Millisecond+stepGrace {
		t.Fatalf("the request outlived the chain deadline plus stepGrace: %s", took)
	}
}

func TestHTTPStep_ConnectionRefusedUnderABudgetStaysSUTUnreachable(t *testing.T) {
	res := Run("b621-refused", []Step{slowStep(closedURL(t))}, 5*time.Second)
	if len(res.Steps) != 1 || !strings.Contains(res.Steps[0].Observed, "SUT unreachable") {
		t.Fatalf("steps = %+v, want the SUT unreachable wording for a refused connection", res.Steps)
	}
}

func TestRequestTimeout_DefaultIsThirtySecondsAndABudgetBoundsIt(t *testing.T) {
	if got := requestTimeout(time.Time{}); got != 30*time.Second {
		t.Fatalf("no budget: %s, want 30s", got)
	}
	if got := requestTimeout(time.Now().Add(90 * time.Second)); got < 89*time.Second || got > 90*time.Second {
		t.Fatalf("90s left: %s, want ~90s (not capped at 30s)", got)
	}
	if got := requestTimeout(time.Now().Add(-time.Second)); got != stepGrace {
		t.Fatalf("budget spent: %s, want stepGrace %s", got, stepGrace)
	}
}
