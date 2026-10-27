package toolcore

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// LokiFor reads the configured Loki as a SHARED Loki only when the config says so AND this
// executor's --loki is that Loki, with the SUT namespace render-k8s gave it.

const sharedBlock = `observability:
  loki:
    url: http://loki.observability.svc:3100
    shared_cluster: cluster-a
    level_field: lvl
    correlation_field: request_id
    saga_event_field: event
    saga_event_values: [tool_dispatch, ghost_dispatch]
    log_format: logfmt
`

func TestLokiFor_SharedSourceFromConfigAndNamespace(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-uni-arb")
	e := lokiCredEnv(t, sharedBlock)
	e.Loki = "http://loki.observability.svc:3100/" // --loki IS the configured url (adopt)
	e.ObsInstance = "shop-uni-arb"
	l := lokiFor(e)
	if l.Shared == nil {
		t.Fatal("shared_cluster + adopt's --loki must give a shared source")
	}
	want := obsquery.SharedLogSource{
		Cluster: "cluster-a", Namespace: "shop-uni-arb",
		Constants: map[string]string{"argus_instance": "shop-uni-arb", "project": "t"},
		Format:    "logfmt", LevelField: "lvl", CorrelationField: "request_id", SagaField: "event", SagaMarkers: []string{"tool_dispatch", "ghost_dispatch"},
	}
	got := *l.Shared
	if got.Cluster != want.Cluster || got.Namespace != want.Namespace || got.Format != want.Format || got.LevelField != want.LevelField ||
		got.CorrelationField != want.CorrelationField || got.SagaField != want.SagaField || strings.Join(got.SagaMarkers, ",") != strings.Join(want.SagaMarkers, ",") ||
		got.Constants["argus_instance"] != "shop-uni-arb" || got.Constants["project"] != "t" {
		t.Fatalf("shared source:\n got %+v\nwant %+v", got, want)
	}
	// get_sagas / tail_logs use it too, so they cannot silently query the service label of a Loki that has none
	q, err := l.CorrelationQuery("tr-1-S-aaaaaaaa", "", "")
	if err != nil || !strings.HasPrefix(q, `{cluster="cluster-a",namespace="shop-uni-arb",`) {
		t.Fatalf("got %q / %v", q, err)
	}
}

func TestLokiFor_SharedWithoutANamespaceStaysSharedAndBuildsNoQuery(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	e := lokiCredEnv(t, sharedBlock)
	e.Loki = "http://loki.observability.svc:3100"
	l := lokiFor(e)
	if l.Shared == nil {
		t.Fatal("a missing namespace must NOT turn the shared source off (that would query the service label of a Loki that has none)")
	}
	if q, err := l.CorrelationQuery("tr-1-S-aaaaaaaa", "", ""); err == nil || q != "" {
		t.Fatalf("want a refusal, got %q / %v", q, err)
	}
}

// the executor reading ANOTHER Loki than the configured one gets no shared source (same guard as the credential)
func TestLokiFor_NoSharedSourceForAnotherLoki(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-uni-arb")
	e := lokiCredEnv(t, sharedBlock) // e.Loki = http://loki:3100
	if lokiFor(e).Shared != nil {
		t.Fatal("--loki is not the configured shared Loki: no shared source")
	}
}

// 3: a config without shared_cluster - the common case - has no shared source, namespace or not.
func TestLokiFor_OwnLokiHasNoSharedSource(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-uni-arb")
	e := lokiCredEnv(t, "observability:\n  loki:\n    url: http://loki:3100\n")
	l := lokiFor(e)
	if l.Shared != nil {
		t.Fatal("an own-Loki instance must never get a shared source")
	}
	q, _ := l.CorrelationQuery("tr-1-S-aaaaaaaa", "", "")
	if q != obsquery.CorrelationLogQL("tr-1-S-aaaaaaaa", "", "") {
		t.Fatalf("own-Loki query changed: %s", q)
	}
}

func TestConfigWarnings_NamesTheSharedPromtailLimits(t *testing.T) {
	e := lokiCredEnv(t, sharedBlock)
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var hit int
	for _, w := range ConfigWarnings(c, e.ScenariosDir, 0) {
		if strings.Contains(w, "readline_rate") || strings.Contains(w, "batchwait") {
			hit++
		}
	}
	if hit != 2 {
		t.Fatalf("validate-config must surface both limits, saw %d", hit)
	}
}
