package toolcore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// THE WIRING of a REAL executor's Loki into a chain's `logs` step, through the production entry
// point (toolcore.Run -> argus.RunAllWithLogs(..., logsReaderFor(e))). The chain/argus tests hand the step a reader
// directly; nothing proved Run hands it one. Found by mutation: with `nil` in place of logsReaderFor(e) every
// package stayed green, so the step could have been dead in production.

const logsWiringScenario = "# Scenario: c\n\n## Metadata\n- **ID**: CHN-LOGS\n- **Layer**: Permissions\n- **Tags**: chain\n\n" +
	"## TRIGGER\nPOST `chain`\n\n```json\n" +
	`{"steps":[{"type":"logs","name":"l","scope":"source","labels":{"app":"orders"},"contains":"hello","settle":"0s"}]}` +
	"\n```\n\n## EXPECT\n### Runnable\n- step l: body has count >= 1\n\n## TIMEOUT\n60s\n\n" +
	"## CLEANUP\nN/A - a unit-test fixture; it creates nothing.\n"

type wiringLoki struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queries []string
	tenants []string
}

func newWiringLoki(t *testing.T) *wiringLoki {
	t.Helper()
	w := &wiringLoki{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			rw.WriteHeader(http.StatusNoContent) // the run's own request-event push
			return
		}
		w.mu.Lock()
		w.queries = append(w.queries, r.URL.Query().Get("query"))
		w.tenants = append(w.tenants, r.Header.Get("X-Scope-OrgID"))
		w.mu.Unlock()
		_, _ = rw.Write([]byte(`{"status":"success","data":{"result":[{"stream":{"app":"orders"},"values":[["1000","hello world"]]}]}}`))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// runLogsWiring runs the one chain scenario through toolcore.Run and returns its result row.
func runLogsWiring(t *testing.T, e Env) report.ScenarioResult {
	t.Helper()
	t.Setenv(probeDisableEnv, "1")
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	if _, _, err := Run(e, "run-logs-wiring", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rep, err := readReport(e.reportPath())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rep.Find("CHN-LOGS")
	if got == nil {
		t.Fatalf("the scenario is missing from the report: %+v", rep.Layers)
	}
	return *got
}

func logsWiringEnv(t *testing.T) Env {
	t.Helper()
	// NOT money-handling: a money-handling config refuses a chain with a logs step before any query.
	return rlEnv(t, "project:\n  name: t\n", map[string]string{"CHN-LOGS": logsWiringScenario})
}

func TestRun_HandsTheExecutorsLokiAndTenantToAChainsLogsStep(t *testing.T) {
	loki := newWiringLoki(t)
	e := logsWiringEnv(t)
	e.Loki = loki.srv.URL
	e.LokiTenant = "inst-a"
	res := runLogsWiring(t, e)
	if res.Status != "passed" {
		t.Fatalf("want passed through the real executor path: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	loki.mu.Lock()
	defer loki.mu.Unlock()
	if len(loki.queries) != 1 {
		t.Fatalf("want exactly one query_range request on --loki, got %d: %q", len(loki.queries), loki.queries)
	}
	if want := `{app="orders"} |= "hello"`; loki.queries[0] != want {
		t.Fatalf("the query:\n got %s\nwant %s", loki.queries[0], want)
	}
	if loki.tenants[0] != "inst-a" {
		t.Fatalf("Env.LokiTenant must reach the request as X-Scope-OrgID, got %q", loki.tenants[0])
	}
}

func TestRun_NoLokiOnTheExecutor_ALogsStepIsRefusedAndNothingIsSent(t *testing.T) {
	loki := newWiringLoki(t) // exists, but is NOT given to the executor
	e := logsWiringEnv(t)
	e.Loki = ""
	res := runLogsWiring(t, e)
	if res.Status != "failed" || res.Failure == nil ||
		!strings.Contains(res.Failure.Observed, "this executor has none") || !strings.HasSuffix(res.Failure.Observed, "— preflight") {
		t.Fatalf("want the no-Loki preflight refusal, got %q %+v", res.Status, res.Failure)
	}
	loki.mu.Lock()
	defer loki.mu.Unlock()
	if len(loki.queries) != 0 {
		t.Fatalf("no request may be sent: %q", loki.queries)
	}
}
