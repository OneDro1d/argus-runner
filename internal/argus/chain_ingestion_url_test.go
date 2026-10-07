package argus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// / a chain `http` step whose url starts with `${INGESTION_URL}` is
// resolved against the same http target a plain HTTP check of that scenario uses (targets.http —
// host and port only; the path of base_url is NOT prepended). A chain cannot name
// another target (**Target** is refused on a chain), so targets.http is the only base there is.

// seen is what a local server recorded of the requests it received.
type seen struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (s *seen) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.RequestURI())
		s.bodies = append(s.bodies, string(b))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"item-9","ok":true}`))
	})
}

func httpTargetCfg(base string) *config.Config {
	return &config.Config{Targets: config.Targets{HTTP: &config.HTTPTarget{BaseURL: base}}}
}

func TestChainHTTPStep_IngestionURLResolvesToTheConfiguredHTTPTarget(t *testing.T) {
	const fake = "fake-chain-secret-3318"
	t.Setenv("SOME_PASSWORD", fake)
	rec := &seen{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// base_url carries a path prefix on purpose: only host and port are used.
	cfg := httpTargetCfg(srv.URL + "/ignored/prefix")
	trig := `{"steps":[
		{"type":"http","name":"login","method":"POST","url":"${INGESTION_URL}/api/v1/login","body":{"password":"${SOME_PASSWORD}"},"save":{"id":"id"}},
		{"type":"http","name":"read","method":"GET","url":"${INGESTION_URL}/api/v1/items/${saved.id}"}
	]}`
	expect := "### Runnable\n- step login: status=200\n- step read: status=200\n"
	res := runChainScenario(cfg, scenario.Parse(chainRunMD("", trig, expect)), "tr-ingest-ok", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %q %+v", res.Status, res.Failure)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 2 || rec.paths[0] != "POST /api/v1/login" || rec.paths[1] != "GET /api/v1/items/item-9" {
		t.Errorf("the server must see the path alone, with the base_url path unused and the saved id bound: %v", rec.paths)
	}
	if !strings.Contains(rec.bodies[0], fake) {
		t.Errorf("the body must carry the substituted environment value, got %q", rec.bodies[0])
	}
}

func TestChainHTTPStep_IngestionURLWithNoHTTPTargetSaysWhatIsMissing(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"ping","method":"GET","url":"${INGESTION_URL}/health"}]}`
	res := runChainScenario(&config.Config{}, scenario.Parse(chainRunMD("", trig, "### Runnable\n- step ping: status=200\n")), "tr-ingest-none", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("a chain with no http target cannot resolve ${INGESTION_URL}: %q", res.Status)
	}
	if !strings.Contains(res.Failure.Observed, "targets.http.base_url") || !strings.Contains(res.Failure.Observed, "INGESTION_URL") {
		t.Errorf("the failure must name what to declare: %q", res.Failure.Observed)
	}
}

func TestChainHTTPStep_UnresolvedVariableIsNamed(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"ping","method":"GET","url":"${NEVER_SET_BASE}/health/${ALSO_NEVER_SET}"}]}`
	res := runChainScenario(&config.Config{}, scenario.Parse(chainRunMD("", trig, "### Runnable\n- step ping: status=200\n")), "tr-ingest-unres", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("an unresolved variable must fail the step: %q", res.Status)
	}
	obs := res.Failure.Observed
	if !strings.Contains(obs, "${NEVER_SET_BASE}") || !strings.Contains(obs, "${ALSO_NEVER_SET}") {
		t.Errorf("the failure must name EVERY unresolved variable, got %q", obs)
	}
	if strings.Contains(obs, "missing/unresolved http method or url") {
		t.Errorf("the generic message must be gone: %q", obs)
	}
}

func TestChainHTTPStep_IngestionURLRefusesANamedTarget(t *testing.T) {
	rec := &seen{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	trig := `{"steps":[{"type":"http","name":"ping","method":"GET","target":"other","url":"${INGESTION_URL}/health"}]}`
	res := runChainScenario(httpTargetCfg(srv.URL), scenario.Parse(chainRunMD("", trig, "### Runnable\n- step ping: status=200\n")), "tr-ingest-target", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "target") {
		t.Fatalf("an http step naming a target must be refused, never sent to the plain slot: %q %+v", res.Status, res.Failure)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 0 {
		t.Errorf("nothing may be sent on a refusal: %v", rec.paths)
	}
}
