package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
)

// through the in-env MCP surface (the same handlers the control-plane relay and a
// builder's get_tail_logs / get_sagas reach): a fragment or a quote in correlation_id must be refused
// BEFORE Loki is contacted. `tr-` would otherwise match every line in the window, certification runs'
// included.
func TestGetSagasAndTailLogs_RefuseNonMintedCorrelationID_BeforeLoki(t *testing.T) {
	var hits atomic.Int32
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer loki.Close()
	env := writeFixtureReport(t)
	env.Loki = loki.URL

	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "p", rtok)
	for _, tool := range []string{"runner__get_sagas", "runner__get_tail_logs"} {
		for _, corr := range []string{"tr-", "tr-20260902T134305716", `tr-x" or {x=~".+"} |= "`} {
			raw, _ := s.Dispatch("p", rtok, reqBytes(1, "tools/call", map[string]any{
				"name":      tool,
				"arguments": map[string]any{"instance_id": "local", "correlation_id": corr},
			}))
			if !strings.Contains(string(raw), "correlation_id") || !strings.Contains(string(raw), "not a correlation id") {
				t.Errorf("%s(%q): want a correlation_id refusal, got %s", tool, corr, raw)
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a non-minted correlation id reached Loki %d time(s)", n)
	}
}
