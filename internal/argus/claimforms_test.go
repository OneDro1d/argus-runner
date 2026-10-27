package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The executor's second door for the new forms: an mcp engine reads an `elapsed` bullet as
// prose (no operator word, no field-ish token), so classifyExpectBullet refuses it by name; the new body
// forms go through the one shared parser.
func TestClassifyExpectBullet_ElapsedIsRefusedOnTheMCPEngine(t *testing.T) {
	_, err := classifyExpectBullet("- elapsed < 500ms")
	if err == nil || !strings.Contains(err.Error(), "`elapsed` is a claim of a chain `http` step only") {
		t.Fatalf("want a by-name refusal, got %v", err)
	}
}

// THE WIRING, END TO END. The parser, the judge and the step each have their own tests, and all of them
// stayed green with the claim never handed from the scenario runner to the step (chain.WithElapsed(nil)):
// a scenario declaring `elapsed < 20ms` against a server that takes 150 ms then PASSED. This runs the
// scenario the way an executor does, so the claim has to travel the whole way to count.
func TestChainRun_ElapsedClaimReachesTheStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"call","method":"GET","url":"${BASE}/x"}]}`

	slow := scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step call: status=200\n- step call: elapsed < 20ms\n"))
	r := runChainScenario(&config.Config{}, slow, "tr-elapsed-wire", "testkit/ui")
	if r.Status != "failed" {
		t.Fatalf("a 150 ms answer under `elapsed < 20ms` must FAIL (not pass, not error): %s / %+v", r.Status, r.Failure)
	}
	if len(r.Steps) != 1 || r.Steps[0].ElapsedMs == nil || *r.Steps[0].ElapsedMs < 150 {
		t.Fatalf("the step must record the measured duration: %+v", r.Steps)
	}

	fast := scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step call: status=200\n- step call: elapsed < 5s\n"))
	if r := runChainScenario(&config.Config{}, fast, "tr-elapsed-wire-ok", "testkit/ui"); r.Status != "passed" {
		t.Fatalf("a 150 ms answer under `elapsed < 5s` must pass: %s / %+v", r.Status, r.Failure)
	}
}

func TestClassifyExpectBullet_NewBodyFormsAreBodyClaims(t *testing.T) {
	for _, b := range []string{
		"- body has n == 5", "- body has n within 2% of 100", "- body not contains error",
		"- body has s not equals x", "- body has no secret", "- body every rows has id",
	} {
		got, err := classifyExpectBullet(b)
		if err != nil || got.kind != expectBody {
			t.Errorf("%q: got %+v %v", b, got, err)
		}
	}
	if _, err := classifyExpectBullet("- body has n within 2"); err == nil {
		t.Error("a malformed tolerance claim is refused here too, by the shared parser")
	}
}
