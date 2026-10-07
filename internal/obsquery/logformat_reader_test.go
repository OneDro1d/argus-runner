package obsquery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// #422: the saga reader must parse a line the way the SUT DECLARED it encodes its logs
// (observability.loki.log_format), the same rule saga-presence.jmx applies to the verdict (#421):
// json (default) -> JSON lines only; logfmt -> JSON first, then logfmt.

const (
	echoedPlainLine = `2026/10/02 10:00:00 handler: query correlation_id=tr-20260902T134305716-FX-ECHO-00000005 event_type=saga step_name=control_action`
	logfmtSagaLine  = `2026/10/02 10:00:00 INFO telemetry.event correlation_id=tr-20260902T134305716-FX-ECHO-00000005 event_type=saga step_name=control_action step_status=ok service=memstore-gateway`
	jsonSagaLine    = `{"correlation_id":"tr-20260902T134305716-FX-ECHO-00000005","event_type":"saga","step_name":"control_action","step_status":"ok","service":"order-api"}`
)

// RED before the fix: a JSON-declared (default) SUT's plain-text echo was read as a saga step.
func TestSagaStep_JSONDeclared_PlainTextEchoIsNotAStep(t *testing.T) {
	for _, format := range []string{"", "json", "JSON", "garbage"} {
		l := &Loki{LogFormat: format}
		if step, ok := l.sagaStep(echoedPlainLine); ok {
			t.Errorf("log_format=%q: a plain-text line echoing saga pairs must not be a saga step, got %+v", format, step)
		}
	}
}

func TestSagaStep_JSONDeclared_JSONLineStillRead(t *testing.T) {
	step, ok := (&Loki{}).sagaStep(jsonSagaLine)
	if !ok || step.StepName != "control_action" {
		t.Fatalf("a JSON saga line must still be read on a JSON-declared SUT: ok=%v step=%+v", ok, step)
	}
}

// A logfmt-declared SUT's real logfmt saga is still read (the config the verdict also honours).
func TestSagaStep_LogfmtDeclared_LogfmtSagaRead(t *testing.T) {
	for _, format := range []string{"logfmt", "LOGFMT", " logfmt "} {
		l := &Loki{LogFormat: format}
		step, ok := l.sagaStep(logfmtSagaLine)
		if !ok {
			t.Fatalf("log_format=%q: a logfmt saga line must be read", format)
		}
		if step.StepName != "control_action" || step.StepStatus != "ok" || step.Service != "memstore-gateway" {
			t.Errorf("log_format=%q: fields = %+v", format, step)
		}
	}
}

// logfmt-declared still tries JSON first: JSON lines are read exactly as before.
func TestSagaStep_LogfmtDeclared_JSONLineStillRead(t *testing.T) {
	step, ok := (&Loki{LogFormat: "logfmt"}).sagaStep(jsonSagaLine)
	if !ok || step.StepName != "control_action" || step.Service != "order-api" {
		t.Fatalf("JSON first on a logfmt-declared SUT: ok=%v step=%+v", ok, step)
	}
}

// GAP-2 stays fixed: tail-logs DISPLAYS a logfmt line structured even when the SUT declared nothing
// (json is the default). Only saga steps follow the declaration; a displayed line decides nothing.
func TestParseLogLine_UndeclaredLogfmtLineStillStructured(t *testing.T) {
	got := (&Loki{}).parseLogLine(lokiEntry{TS: "1700000000000000000", Line: logfmtSagaLine})
	if got.Service != "memstore-gateway" || got.Fields["correlation_id"] != "tr-20260902T134305716-FX-ECHO-00000005" {
		t.Fatalf("an undeclared SUT's logfmt line must still be read for display (GAP-2): %+v", got)
	}
}

// End to end through Sagas(): the echoed line must not appear in get-sagas on a JSON-declared SUT.
func TestSagas_JSONDeclared_EchoNotInTimeline_LogfmtDeclared_Read(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		now := strconv.FormatInt(time.Now().UnixNano(), 10)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []map[string]any{
			{"values": [][2]string{{now, echoedPlainLine}}},
		}}})
	}))
	defer ts.Close()

	if sg := (&Loki{BaseURL: ts.URL}).Sagas("tr-20260902T134305716-FX-ECHO-00000005", "30m", time.Time{}); len(sg.Timeline) != 0 {
		t.Errorf("JSON-declared: the echoed plain-text line must not be a timeline step, got %+v", sg.Timeline)
	}
	// the same wire line under a logfmt declaration IS a step: the declaration, not the line, decides
	if sg := (&Loki{BaseURL: ts.URL, LogFormat: "logfmt"}).Sagas("tr-20260902T134305716-FX-ECHO-00000005", "30m", time.Time{}); len(sg.Timeline) != 1 {
		t.Errorf("logfmt-declared: the line must be read as a step, got %+v", sg.Timeline)
	}
}
