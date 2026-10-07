package mcpserver

// / — the in-env MCP get_report: with the runner (product) token the
// envelope carries neither report.Failure.FailedBodyCheck nor the failing bullet nor its expected
// value, with and without a run id; the author token reads the same file WITH it (positive control).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

const (
	fbcBullet   = "body has fbcFieldSentinelK2 equals FBC-EXPECTED-SENTINEL-9Wd"
	fbcFieldTok = "fbcFieldSentinelK2"
	fbcExpected = "FBC-EXPECTED-SENTINEL-9Wd"
)

func writeFailedBodyCheckFixture(t *testing.T) toolcore.Env {
	t.Helper()
	tmp := t.TempDir()
	exp := "status=200; " + fbcBullet
	rep := report.Report{
		RunID: "run_fbc", Project: "fixture", Mode: "ci", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "HTTP Ingestion", Scenarios: []report.ScenarioResult{{
			ID: "BODY-030", Status: "failed",
			Failure: &report.Failure{Expected: &exp,
				Observed:        "status matched but the response body did not satisfy the scenario's body assertion (the asserted value is held out; see failure.expected with the test hat)",
				FailedBodyCheck: &report.FailedBodyCheck{Index: 2, Bullet: fbcBullet}},
		}}}},
	}
	b, _ := json.Marshal(&rep)
	dir := filepath.Join(tmp, "local")
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "report.json"), filepath.Join(dir, "runs", "run_fbc.json")} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return toolcore.Env{Instance: "local", ResultsRoot: tmp, ScenariosDir: tmp,
		Grafana: "http://grafana", Loki: "http://127.0.0.1:9"}
}

func callGetReportArgs(t *testing.T, env toolcore.Env, token string, args map[string]any) string {
	t.Helper()
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", token)
	raw, _ := s.Dispatch("s", token, reqBytes(1, "tools/call", map[string]any{"name": "runner__get_report", "arguments": args}))
	return string(raw)
}

func TestFailedBodyCheck_InEnvMCP_GetReport(t *testing.T) {
	env := writeFailedBodyCheckFixture(t)
	for name, args := range map[string]map[string]any{
		"no run id":   {"instance_id": "local"},
		"with run id": {"instance_id": "local", "run_id": "run_fbc"},
	} {
		builder := callGetReportArgs(t, env, rtok, args)
		if !strings.Contains(builder, "BODY-030") {
			t.Fatalf("%s: the builder envelope must still be the report (sanity): %s", name, builder)
		}
		// the key may sit in the envelope as an escaped string: match the bare word
		for _, needle := range []string{"failed_body_check", fbcBullet, fbcFieldTok, fbcExpected} {
			if strings.Contains(builder, needle) {
				t.Errorf("SECURITY (%s): the builder's runner__get_report envelope carries %q:\n%s", name, needle, builder)
			}
		}
		author := callGetReportArgs(t, env, atok, args)
		if !strings.Contains(author, "failed_body_check") || !strings.Contains(author, fbcFieldTok) {
			t.Errorf("positive control (%s): the author's get_report must carry failed_body_check:\n%s", name, author)
		}
	}
}
