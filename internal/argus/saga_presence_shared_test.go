package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// Saga-presence against an environment's SHARED Loki. The verdict is made on the parsed line in
// Groovy (so no label is needed); what changes is WHERE the lines are looked for: the configured Loki, selected by
// cluster + namespace. An instance with its own Loki gets exactly the props and the query it always got.

const sharedSagaURL = "http://loki.observability.svc:3100"

func sharedSagaConfig(t *testing.T) (c *config.Config, props map[string]string) {
	t.Helper()
	cfg := sagaScenario(t)
	cfg.Observability.Loki.URL = sharedSagaURL
	cfg.Observability.Loki.SharedCluster = "cluster-a"
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-uni-arb")
	return cfg, sagaPropsFor(t, cfg)
}

func TestSagaPresenceProps_SharedLokiSelectsClusterAndNamespaceOnTheConfiguredLoki(t *testing.T) {
	_, p := sharedSagaConfig(t)
	if p["loki.url"] != sharedSagaURL {
		t.Errorf("loki.url = %q, want the configured shared Loki %q", p["loki.url"], sharedSagaURL)
	}
	want := `{cluster="cluster-a",namespace="shop-uni-arb",container!~"` + obsquery.ObsInfraExclusion + `"}`
	if p["loki.selector"] != want {
		t.Errorf("loki.selector = %q\n want %q", p["loki.selector"], want)
	}
	t.Logf("shared-mode saga-presence selector prop: %s", p["loki.selector"])
	// the verdict's names are the SUT's declared ones, as before
	if p["loki.corr_field"] != "correlation_id" || p["loki.saga_field"] != "event_type" || p["loki.saga_values"] != `["saga"]` {
		t.Errorf("the judging props changed: %v", p)
	}
}

func TestSagaPresenceProps_SharedLokiWithoutANamespaceIsRefusedNotFallenBack(t *testing.T) {
	cfg := sagaScenario(t)
	cfg.Observability.Loki.URL = sharedSagaURL
	cfg.Observability.Loki.SharedCluster = "cluster-a"
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	s := headerScenario(t, "HTTP Ingestion", "saga-presence")
	p, err := DeriveProps(cfg, s, sagaCID)
	if err == nil || !strings.Contains(err.Error(), "ARGUS_SUT_NAMESPACE") {
		t.Fatalf("want a refusal naming ARGUS_SUT_NAMESPACE, got props=%v err=%v", p, err)
	}
}

// 3: an instance with its own Loki - the props are what they were, and no selector prop exists.
func TestSagaPresenceProps_OwnLokiIsUnchanged(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-uni-arb") // set, and must make no difference
	p := sagaPropsFor(t, sagaScenario(t))
	if p["loki.url"] != "http://loki:3100" {
		t.Errorf("loki.url = %q, want the in-network bundled address", p["loki.url"])
	}
	if _, ok := p["loki.selector"]; ok {
		t.Errorf("an own-Loki instance must not carry loki.selector: %v", p)
	}
}

func TestSagaPresenceTemplate_FallsBackToTheServiceSelectorWhenNoneIsGiven(t *testing.T) {
	script := sagaVerifyScript(t)
	for _, need := range []string{
		`props.get("loki.selector")`,
		`'{service=~".+",service!~"' + excl + '"}'`, // the pre-existing selector, character for character
		`def logql = streamSel + ' |= ' + lq(corr) + ' |= ' + lq(step)`,
	} {
		if !strings.Contains(script, need) {
			t.Errorf("template lost %q", need)
		}
	}
}

// The REAL template script (Groovy) against a fake Loki that records the query it receives.
func TestSagaPresenceTemplate_QueryAsSentToLoki(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{
			map[string]any{"stream": map[string]string{"container": "x"}, "values": [][]string{{"170000000000000000", `{"event_type":"saga","correlation_id":"` + sagaCID + `","step_name":"control_action"}`}}}}}})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t.Run("shared: the query selects cluster and namespace and still passes on the parsed line", func(t *testing.T) {
		_, p := sharedSagaConfig(t)
		mu.Lock()
		queries = nil
		mu.Unlock()
		v := execVerifyScript(t, p, srv.URL)
		if !v.Successful {
			t.Fatalf("verdict: %+v", v)
		}
		mu.Lock()
		defer mu.Unlock()
		want := `{cluster="cluster-a",namespace="shop-uni-arb",container!~"` + obsquery.ObsInfraExclusion + `"} |= "` + sagaCID + `" |= "control_action"`
		if len(queries) != 1 || queries[0] != want {
			t.Fatalf("query sent by the template:\n got %q\nwant %q", queries, want)
		}
		t.Logf("shared-mode saga-presence LogQL sent by templates/saga-presence.jmx: %s", queries[0])
	})

	t.Run("own Loki: the query is exactly the pre-change one", func(t *testing.T) {
		p := sagaPropsFor(t, sagaScenario(t))
		mu.Lock()
		queries = nil
		mu.Unlock()
		v := execVerifyScript(t, p, srv.URL)
		if !v.Successful {
			t.Fatalf("verdict: %+v", v)
		}
		mu.Lock()
		defer mu.Unlock()
		want := `{service=~".+",service!~"` + obsquery.ObsInfraExclusion + `"} |= "` + sagaCID + `" |= "control_action"`
		if len(queries) != 1 || queries[0] != want {
			t.Fatalf("own-Loki query changed:\n got %q\nwant %q", queries, want)
		}
	})
}
