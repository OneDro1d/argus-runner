package toolcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// #422: lokiFor must thread observability.loki.log_format into obsquery.Loki.LogFormat, so the
// reader honours the declaration the saga-presence verdict already honours (#421). Dropping the
// wiring makes the logfmt case read as JSON-only and the first test goes RED.

const wiringSagaLine = `2026/10/02 10:00:00 INFO telemetry.event correlation_id=tr-20260902T134305716-FX-X-00000001 event_type=saga step_name=control_action step_status=ok`

// sagaSteps runs the reader lokiFor builds against a fake Loki that serves wiringSagaLine.
func sagaSteps(t *testing.T, cfgBlock string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		now := strconv.FormatInt(time.Now().UnixNano(), 10)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []map[string]any{
			{"values": [][2]string{{now, wiringSagaLine}}},
		}}})
	}))
	defer srv.Close()
	e := lokiCredEnv(t, cfgBlock)
	e.Loki = srv.URL
	return len(lokiFor(e).Sagas("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{}).Timeline)
}

func TestLokiFor_ThreadsDeclaredLogFormat(t *testing.T) {
	if n := lokiFor(lokiCredEnv(t, "observability:\n  loki:\n    log_format: logfmt\n")).LogFormat; n != "logfmt" {
		t.Errorf("lokiFor must thread log_format: logfmt, got %q", n)
	}
	// behavioural, not just a field copy: the reader built from this config reads the logfmt saga
	if n := sagaSteps(t, "observability:\n  loki:\n    log_format: logfmt\n"); n != 1 {
		t.Errorf("a reader built from a logfmt-declared config must read a logfmt saga line, got %d steps", n)
	}
}

func TestLokiFor_UndeclaredLogFormat_IsJSONOnly(t *testing.T) {
	if n := sagaSteps(t, "observability:\n  loki:\n    url: http://loki:3100\n"); n != 0 {
		t.Errorf("a config without log_format must not read a logfmt line as a saga step, got %d steps", n)
	}
	if n := sagaSteps(t, "observability:\n  loki:\n    log_format: json\n"); n != 0 {
		t.Errorf("log_format: json must not read a logfmt line as a saga step, got %d steps", n)
	}
}
