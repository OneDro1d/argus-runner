package argus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE WIRING. chain.bodyExcerpt scrubs whatever function it is handed, and its own tests
// hand it one. With the scenario runner handing it NONE (the `chain.WithBodyScrub(credentialScrub(c))`
// option dropped from the step), every test stayed green and a credential the system echoed in its error
// body would have been written into the author's report. This runs the scenario the way an executor does:
// a system that answers 422 and reflects the request back.
func TestChainRun_BodyExcerptNeverCarriesADeclaredCredential(t *testing.T) {
	const canary = "Cnry-excerpt-check-env-5521"
	t.Setenv("EXCERPT_CHECK_PASSWORD", canary)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"EXCERPT-VISIBLE-MESSAGE","you_sent":` + string(b) + `}`))
	}))
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"call","method":"POST","url":"${BASE}/x","body":{"password":"${EXCERPT_CHECK_PASSWORD}"}}]}`
	s := scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step call: status=200\n"))
	c := &config.Config{CheckEnv: []string{"EXCERPT_CHECK_PASSWORD"}}

	r := runChainScenario(c, s, "tr-excerpt-scrub", "testkit/ui")
	if r.Status != "failed" || len(r.Steps) != 1 {
		t.Fatalf("want one failed step, got %s / %+v", r.Status, r.Steps)
	}
	got := r.Steps[0].BodyExcerpt
	if !strings.Contains(got, "EXCERPT-VISIBLE-MESSAGE") {
		t.Fatalf("the excerpt does not carry the system's error message, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, canary) {
		t.Errorf("the excerpt carries a declared check_env value: %q", got)
	}
	// through the same real path: the wanted status is in neither builder-visible text.
	if strings.Contains(r.Steps[0].Observed, "200") || (r.Failure != nil && strings.Contains(r.Failure.Observed, "200")) {
		t.Errorf("the wanted status reached builder-visible text: step %q / failure %+v", r.Steps[0].Observed, r.Failure)
	}
}
