package obsquery

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// ── GAP-2: logfmt-tolerant line parsing ───────────────────────────────────────────────────
//
// Found 2026-07-22 evaluating the Memstore triage. The reader json.Unmarshal'd every Loki line and
// skipped anything that failed — so a SUT that logs logfmt (Memstore: Go's stdlib logger, e.g.
// `2026/07/16 19:28:43 INFO telemetry.event event_type=auth.succeeded service=memstore-gateway
// correlation_id="" ...`) had EVERY line dropped before the saga filter ever ran. The triage
// concluded the SUT emitted nothing on failure paths; in fact we could not read what it emitted.

func TestParseKV_Logfmt_WithLeadingPrefixTokens(t *testing.T) {
	line := `2026/07/16 19:28:43 INFO telemetry.event event_type=auth.succeeded service=memstore-gateway ` +
		`version=v0.1.0 correlation_id="" workspace_id="" user_id=dev-user resource_type=auth`
	m, _, ok := parseKV(line)
	if !ok {
		t.Fatalf("logfmt line must parse")
	}
	for k, want := range map[string]string{
		"event_type": "auth.succeeded", "service": "memstore-gateway",
		"version": "v0.1.0", "user_id": "dev-user", "resource_type": "auth",
	} {
		if got := str(m[k]); got != want {
			t.Errorf("key %q = %q, want %q", k, got, want)
		}
	}
	// an explicitly-empty quoted value must be PRESENT-and-empty, not absent
	if v, present := m["correlation_id"]; !present || str(v) != "" {
		t.Errorf(`correlation_id must parse as present-and-empty, got present=%v value=%q`, present, str(v))
	}
	// the leading date/level/message tokens are not k=v and must not invent keys
	if _, bad := m["2026/07/16"]; bad {
		t.Error("the leading timestamp token must not become a key")
	}
}

func TestParseKV_QuotedValuesWithSpacesAndEscapes(t *testing.T) {
	m, _, ok := parseKV(`level=INFO msg="tool dispatch received" note="he said \"hi\"" n=3`)
	if !ok {
		t.Fatal("must parse")
	}
	if got := str(m["msg"]); got != "tool dispatch received" {
		t.Errorf("quoted value with spaces = %q", got)
	}
	if got := str(m["note"]); got != `he said "hi"` {
		t.Errorf("escaped quotes = %q", got)
	}
}

func TestParseKV_JSONStillWinsAndPlainProseIsNotForced(t *testing.T) {
	// JSON must still parse as JSON (the common case must not regress)
	m, _, ok := parseKV(`{"level":"INFO","service":"social-gateway","event":"tool_dispatch"}`)
	if !ok || str(m["service"]) != "social-gateway" {
		t.Fatalf("JSON must still parse: ok=%v m=%v", ok, m)
	}
	// a line with NO k=v pairs at all must report not-parsed rather than yield a junk map
	if _, _, ok := parseKV("plain prose log line with no pairs"); ok {
		t.Error("a line with no k=v pairs must not be reported as parsed")
	}
}

// ── GAP-1: declarable saga step fields + the DEGRADED honesty gate ────────────────────────
//
// Found 2026-07-22 evaluating the Social triage. The saga node builder read HARDCODED the operator keys
// (saga_id/step/step_name/step_status/ts/...). Social's real saga line is
//   {"time":…,"level":"INFO","msg":"tool_dispatch_received","service":"social-gateway",
//    "event":"tool_dispatch","tool_name":…,"request_id":"tr-…"}
// — of those keys ONLY `service` intersects. Result: available:true with every semantic field
// empty, which reads as present-and-healthy to any automated check. The per-SUT translation table
// declared 4 fields and did not cover the step fields at all.

func TestSagaStepFields_DeclaredNamesAreHonoured(t *testing.T) {
	l := &Loki{
		SagaEventField: "event", SagaEventValue: "tool_dispatch",
		SagaStepFields: map[string]string{
			"step_name": "msg", "timestamp": "time", "step_status": "level",
		},
	}
	line := `{"time":"2026-07-22T12:01:53Z","level":"INFO","msg":"tool_dispatch_received",` +
		`"service":"social-gateway","event":"tool_dispatch","request_id":"tr-20260902T134305716-FX-X-00000001"}`
	step, ok := l.sagaStep(line)
	if !ok {
		t.Fatal("a line carrying the declared saga marker must yield a step")
	}
	if step.StepName != "tool_dispatch_received" {
		t.Errorf("StepName from the declared field = %q", step.StepName)
	}
	if step.Timestamp != "2026-07-22T12:01:53Z" {
		t.Errorf("Timestamp from the declared field = %q", step.Timestamp)
	}
	if step.Service != "social-gateway" {
		t.Errorf("Service = %q", step.Service)
	}
}

func TestSagaStepFields_DefaultsUnchangedForPWWConvention(t *testing.T) {
	l := &Loki{} // no declaration at all — the the operator default contract must be untouched
	line := `{"ts":"2026-07-22T11:06:47Z","event_type":"saga","saga_id":"s1","step":2,` +
		`"step_name":"control_action","step_status":"ok","service":"order-api","what":"rate_limit changed"}`
	step, ok := l.sagaStep(line)
	if !ok {
		t.Fatal("the canonical the operator saga line must still yield a step")
	}
	if step.SagaID != "s1" || step.Step != 2 || step.StepName != "control_action" ||
		step.StepStatus != "ok" || step.Timestamp != "2026-07-22T11:06:47Z" {
		t.Errorf("the operator defaults regressed: %+v", step)
	}
	if step.Fields.What == nil || *step.Fields.What != "rate_limit changed" {
		t.Error("the what/why/by_whom fields regressed")
	}
}

func TestSagaStepFields_FallbackChainFindsMsgAndTime(t *testing.T) {
	// NOTHING declared, and the SUT uses msg/time — the generic fallback must still populate,
	// so a SUT gets useful output before anyone edits argus-config.
	l := &Loki{SagaEventField: "event", SagaEventValue: "tool_dispatch"}
	step, ok := l.sagaStep(`{"time":"2026-07-22T12:01:53Z","msg":"tool_dispatch_received","event":"tool_dispatch"}`)
	if !ok {
		t.Fatal("must yield a step")
	}
	if step.StepName != "tool_dispatch_received" {
		t.Errorf("fallback StepName (msg) = %q", step.StepName)
	}
	if step.Timestamp != "2026-07-22T12:01:53Z" {
		t.Errorf("fallback Timestamp (time) = %q", step.Timestamp)
	}
}

// THE failure mode this whole fix exists for: a timeline of content-free nodes must NEVER be
// reported as a clean available:true. That is the "green No-data panel is blind, not all-clear"
// trap — it cost the Social triage every one of its eight verdicts.
func TestSaga_ContentFreeNodesAreReportedDegraded(t *testing.T) {
	l := &Loki{SagaEventField: "event", SagaEventValue: "tool_dispatch"}
	var saga failcontext.Saga
	saga.Available = true
	saga.Timeline = []failcontext.SagaStep{{Service: "social-gateway"}, {Service: "social-gateway"}}
	markDegradedIfContentFree(&saga, l)

	if saga.Note == "" {
		t.Fatal("a content-free timeline must carry an explanatory note")
	}
	for _, want := range []string{"saga_step_fields", "step_name"} {
		if !strings.Contains(saga.Note, want) {
			t.Errorf("the note must name the remedy %q: %s", want, saga.Note)
		}
	}
	if saga.Available {
		t.Error("a timeline with no readable step content must NOT report available:true")
	}
}

func TestSaga_PopulatedTimelineIsNotMarkedDegraded(t *testing.T) {
	l := &Loki{}
	saga := failcontext.Saga{Available: true, Timeline: []failcontext.SagaStep{
		{StepName: "control_action", StepStatus: "ok", Timestamp: "2026-07-22T11:06:47Z"},
	}}
	markDegradedIfContentFree(&saga, l)
	if !saga.Available || saga.Note != "" {
		t.Errorf("a healthy timeline must be untouched: available=%v note=%q", saga.Available, saga.Note)
	}
}
