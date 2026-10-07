package toolcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// AC-D13: an argus-config declaring observability.betterstack must make GetSagas/TailLogs (the
// SAME query surface the rest of Argus uses today against Loki) run against BetterStack's HTTP
// SQL API instead — proven end-to-end here against a fake BetterStack server, not just at the
// config-parsing layer.

func bsWireEnv(t *testing.T, betterstackBlock string) Env {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://sut:8080\n" + betterstackBlock
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Env{ConfigPath: p, ScenariosDir: dir}
}

// backendFor must select *obsquery.BetterStack when observability.betterstack is declared, and
// keep selecting *obsquery.Loki (unchanged) when it is not.
func TestBackendFor_SelectsBetterStackWhenConfigured(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	e := bsWireEnv(t, `observability:
  betterstack:
    query_url: http://bs-fake.invalid
    credential: ${BS_CREDENTIAL}
    team_id: "123456"
    sources:
      accounting: accounting-service
`)
	b := backendFor(e)
	bs, ok := b.(*obsquery.BetterStack)
	if !ok {
		t.Fatalf("backendFor must return *obsquery.BetterStack when observability.betterstack is declared, got %T", b)
	}
	if bs.QueryURL != "http://bs-fake.invalid" || bs.TeamID != "123456" || bs.Sources["accounting"] != "accounting-service" {
		t.Errorf("backendFor did not thread the declared betterstack config through: %+v", bs)
	}
	if bs.Credential != "user:pass" {
		t.Errorf("backendFor must thread the RESOLVED credential, got %q", bs.Credential)
	}
}

func TestBackendFor_DefaultsToLokiUnchanged(t *testing.T) {
	e := bsWireEnv(t, "")
	b := backendFor(e)
	if _, ok := b.(*obsquery.Loki); !ok {
		t.Fatalf("backendFor must default to *obsquery.Loki when observability.betterstack is not declared, got %T", b)
	}
}

// TestGetSagas_TailLogs_RunAgainstBetterStack_EndToEnd is the PROMISE's evidence: an
// argus-config selecting BetterStack makes GetSagas/TailLogs — the actual saga/log evidence
// paths — answer from a fake BetterStack server, correlation-id- and window-scoped, exactly as
// they would from Loki.
func TestGetSagas_TailLogs_RunAgainstBetterStack_EndToEnd(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	sagaLine := `{"correlation_id":"tr-20260902T134305716-FX-E2E-00000009","event_type":"saga","step_name":"persist","step_status":"ok","ts":"2026-06-22T10:00:00Z","service":"accounting-service"}`
	logLine := `{"correlation_id":"tr-20260902T134305716-FX-E2E-00000009","level":"info","msg":"order accepted"}`
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		sql := string(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(sql, "event_type") { // sagaOnly predicate present → saga query
			_ = json.NewEncoder(w).Encode([]map[string]string{{"dt": "2026-06-22 10:00:00", "raw": sagaLine}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"dt": "2026-06-22 10:00:00", "raw": logLine}})
	}))
	defer fake.Close()

	e := bsWireEnv(t, `observability:
  betterstack:
    query_url: `+fake.URL+`
    credential: ${BS_CREDENTIAL}
    team_id: "123456"
    sources:
      accounting: accounting-service
`)

	sagasOut, err := GetSagas(e, "tr-20260902T134305716-FX-E2E-00000009", "30m")
	if err != nil {
		t.Fatalf("GetSagas: %v", err)
	}
	sagas, ok := sagasOut.(failcontext.Saga)
	if !ok {
		t.Fatalf("GetSagas must return failcontext.Saga, got %T", sagasOut)
	}
	if !sagas.Available || len(sagas.Timeline) != 1 || sagas.Timeline[0].StepName != "persist" {
		t.Fatalf("GetSagas must resolve against the fake BetterStack server: available=%v timeline=%+v note=%q",
			sagas.Available, sagas.Timeline, sagas.Note)
	}

	logsOut, err := TailLogs(e, "tr-20260902T134305716-FX-E2E-00000009", "30m")
	if err != nil {
		t.Fatalf("TailLogs: %v", err)
	}
	logs, ok := logsOut.(failcontext.Logs)
	if !ok {
		t.Fatalf("TailLogs must return failcontext.Logs, got %T", logsOut)
	}
	if !logs.Available || len(logs.Lines) != 1 || logs.Lines[0].Msg != "order accepted" {
		t.Fatalf("TailLogs must resolve against the fake BetterStack server: available=%v lines=%+v note=%q",
			logs.Available, logs.Lines, logs.Note)
	}
}
