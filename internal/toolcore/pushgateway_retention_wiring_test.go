package toolcore

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// the executor's production call site must hand the CONFIGURED retention to
// PushMetrics. This drives toolcore.Run itself against a fake Pushgateway that lists one old group
// of this instance.
func TestRun_PrunesOldPushgatewayGroupsPerConfiguredRetention(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	run := func(t *testing.T, retentionYAML string) []string {
		var mu sync.Mutex
		var deletes []string
		old := float64(time.Now().Add(-3 * time.Hour).Unix())
		pgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/metrics":
				_, _ = fmt.Fprintf(w, `{"status":"success","data":[{"labels":{"instance":"local","job":"argus","run_id":"run-ancient"},"push_time_seconds":{"metrics":[{"labels":{},"value":"%.0f"}]}}]}`, old)
			case r.Method == http.MethodDelete:
				deletes = append(deletes, r.URL.Path)
				w.WriteHeader(http.StatusAccepted)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer pgw.Close()
		cfg := "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n"
		if retentionYAML != "" {
			cfg += "observability:\n  pushgateway:\n    group_retention: " + retentionYAML + "\n"
		}
		e := rlEnv(t, cfg, map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
		e.ResultsRoot = t.TempDir()
		e.Instance = "local"
		e.Pushgateway = pgw.URL
		if _, _, err := Run(e, "", "", "", ""); err != nil {
			t.Fatalf("run: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deletes...)
	}

	for name, yaml := range map[string]string{"default (15m)": "", "1h": "1h"} {
		got := run(t, yaml)
		if len(got) != 1 || got[0] != "/metrics/job/argus/instance/local/run_id/run-ancient" {
			t.Errorf("%s: want the 3h-old group deleted, got %v", name, got)
		}
	}
	if got := run(t, "0"); len(got) != 0 {
		t.Errorf("group_retention 0 keeps groups for ever, but the run deleted %s", strings.Join(got, ", "))
	}
}
