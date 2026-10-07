package obsquery

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// ── PROB-2 (2026-07-22): a SUT may mark its control actions with SEVERAL marker values ──────
//
// Found by evaluating the Social triage of run 20260722T12574061bf58. Its cross-cutting finding
// was that every saga contained exactly ONE node (`tool_dispatch_received`, social-gateway) and no
// step from social-ayrshare or social-ghost at all — even though those services demonstrably wrote
// control actions to Loki (NACK-with-requeue, DLQ routing, circuit_breaker_open). The triage
// attributed it to SUT saga emission. Half right: the SUT DOES emit them. Live marker census:
//     social-gateway   event=tool_dispatch (133), audit_log_insert_failed (5)
//     social-ayrshare  event=ayrshare_dispatch (172)
//     social-ghost     event=ghost_dispatch (34)
// saga_event_value was a single scalar, so only ONE of those could ever be declared and the saga
// was partial BY CONSTRUCTION. the operator's own convention hides this: OrderService puts every step under
// event_type=saga and varies step_name, so one value suffices. Social encodes step identity in the
// marker field itself. Both are legitimate; the contract must express both.

func TestSagaMarker_MultipleDeclaredValuesAllMatch(t *testing.T) {
	l := &Loki{SagaEventField: "event", SagaEventValues: []string{"tool_dispatch", "ayrshare_dispatch", "ghost_dispatch"}}
	for _, tc := range []struct{ line, wantStep string }{
		{`{"time":"t1","level":"INFO","msg":"tool_dispatch_received","service":"social-gateway","event":"tool_dispatch"}`, "tool_dispatch_received"},
		{`{"time":"t2","level":"WARN","msg":"ayrshare transient failure — NACK with requeue","service":"social-ayrshare","event":"ayrshare_dispatch"}`, "ayrshare transient failure — NACK with requeue"},
		{`{"time":"t3","level":"WARN","msg":"circuit_breaker_open; short-circuit","service":"social-ghost","event":"ghost_dispatch"}`, "circuit_breaker_open; short-circuit"},
	} {
		step, ok := l.sagaStep(tc.line)
		if !ok {
			t.Errorf("declared marker value not matched: %s", tc.line)
			continue
		}
		if step.StepName != tc.wantStep {
			t.Errorf("StepName = %q, want %q", step.StepName, tc.wantStep)
		}
	}
	// a value that was NOT declared must still be rejected — the marker set is a whitelist, not
	// "any non-empty value", or ordinary request logs would masquerade as saga steps.
	if _, ok := l.sagaStep(`{"msg":"x","event":"audit_log_insert_failed"}`); ok {
		t.Error("an UNDECLARED marker value must not match")
	}
}

func TestSagaMarker_SingleValueContractUnchanged(t *testing.T) {
	// the the operator default (event_type=saga) and an explicit single scalar must behave exactly as before
	if _, ok := (&Loki{}).sagaStep(`{"ts":"t","event_type":"saga","step_name":"pg_insert","step_status":"ok"}`); !ok {
		t.Error("the the operator default marker regressed")
	}
	l := &Loki{SagaEventField: "event", SagaEventValue: "tool_dispatch"}
	if _, ok := l.sagaStep(`{"msg":"m","event":"tool_dispatch"}`); !ok {
		t.Error("a single declared scalar must still match")
	}
	if _, ok := l.sagaStep(`{"msg":"m","event":"other"}`); ok {
		t.Error("a non-matching value must not match")
	}
}

// ── PROB-2b: failed_step / last_known_good must work for a SUT that is not the operator-vocabulary ───
//
// The saga's headline diagnostics compared step_status against the literals "failed" and "ok".
// Social's step_status maps to its `level` field, so its steps read INFO / WARN / ERROR and NEITHER
// comparison could ever fire — which is why the triage reported "not one saga contained a
// step_status=failed node". The RAW value stays in the output (honesty: show what the SUT said);
// only the DERIVED failed_step/last_known_good use the normalized reading.
func TestSagaStatus_NormalizedForDerivedFieldsRawValuePreserved(t *testing.T) {
	for raw, want := range map[string]string{
		"ok": "ok", "OK": "ok", "success": "ok", "succeeded": "ok", "passed": "ok",
		"INFO": "ok", "info": "ok", "DEBUG": "ok",
		"failed": "failed", "FAILED": "failed", "failure": "failed",
		"error": "failed", "ERROR": "failed", "fatal": "failed",
		"": "", "weird-custom-value": "",
	} {
		if got := normalizeStatus(raw); got != want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSaga_FailedStepDerivedFromNonPWWVocabulary(t *testing.T) {
	saga := failcontext.Saga{Available: true, Timeline: []failcontext.SagaStep{
		{StepName: "tool_dispatch_received", StepStatus: "INFO", Timestamp: "t1"},
		{StepName: "dispatch_received", StepStatus: "INFO", Timestamp: "t2"},
		{StepName: "circuit_breaker_open; short-circuit", StepStatus: "ERROR", Timestamp: "t3"},
	}}
	deriveFailedStep(&saga)

	if saga.FailedStep != "circuit_breaker_open; short-circuit" {
		t.Errorf("failed_step must be derived from an ERROR-level step, got %q", saga.FailedStep)
	}
	if saga.LastKnownGood != "dispatch_received" {
		t.Errorf("last_known_good must be the preceding ok step, got %q", saga.LastKnownGood)
	}
	// the RAW status the SUT emitted must survive untouched in the timeline
	if saga.Timeline[2].StepStatus != "ERROR" {
		t.Errorf("the raw step_status must be preserved, got %q", saga.Timeline[2].StepStatus)
	}
}

// PROB-2c — a TIMELINE must be in time order. Loki returns entries grouped per stream, so with one
// node the order never showed; with a real multi-service saga the steps came back interleaved
// (ayrshare 42.245, ayrshare 42.832, gateway 42.238, ayrshare 42.830, ayrshare 44.326). That is
// misleading on its face AND it breaks last_known_good, which is derived by walking the timeline.
func TestSaga_TimelineIsSortedChronologically(t *testing.T) {
	saga := failcontext.Saga{Timeline: []failcontext.SagaStep{
		{StepName: "b", Timestamp: "2026-07-22T12:57:42.245696218Z"},
		{StepName: "d", Timestamp: "2026-07-22T12:57:42.832005250Z"},
		{StepName: "a", Timestamp: "2026-07-22T12:57:42.238531237Z"},
		{StepName: "c", Timestamp: "2026-07-22T12:57:42.830706425Z"},
		{StepName: "e", Timestamp: "2026-07-22T12:57:44.326360942Z"},
	}}
	sortTimeline(&saga)
	var got string
	for _, s := range saga.Timeline {
		got += s.StepName
	}
	if got != "abcde" {
		t.Errorf("timeline order = %q, want chronological \"abcde\"", got)
	}
}

// PROB-2d — a step carrying an ERROR string is a failure even when its status vocabulary does not
// say so. Social's DLQ/NACK control actions are level WARN, which is deliberately neither ok nor
// failed; without this, failed_step stayed empty on a saga that plainly shows the failure.
func TestSaga_ErrorFieldImpliesFailedStep(t *testing.T) {
	saga := failcontext.Saga{Timeline: []failcontext.SagaStep{
		{StepName: "tool_dispatch_received", StepStatus: "INFO", Timestamp: "t1"},
		{StepName: "dispatch_received", StepStatus: "INFO", Timestamp: "t2"},
		{StepName: "NACK with requeue", StepStatus: "WARN", Timestamp: "t3",
			Error: "ayrshare: server error (status=400)"},
	}}
	deriveFailedStep(&saga)
	if saga.FailedStep != "NACK with requeue" {
		t.Errorf("a step with an error string must be the failed step, got %q", saga.FailedStep)
	}
	if saga.LastKnownGood != "dispatch_received" {
		t.Errorf("last_known_good = %q", saga.LastKnownGood)
	}
	// a WARN with NO error must still not be called a failure — that would invent failures
	clean := failcontext.Saga{Timeline: []failcontext.SagaStep{
		{StepName: "retrying", StepStatus: "WARN", Timestamp: "t1"},
	}}
	deriveFailedStep(&clean)
	if clean.FailedStep != "" {
		t.Errorf("a bare WARN must not be reported as failed, got %q", clean.FailedStep)
	}
}
