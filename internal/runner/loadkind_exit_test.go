package runner

// loadkind_exit_test.go -- AC-F2.2: a load test's errors leave the environment only as a word and a count, a broker
// pause only as a word and its times — by get_limited_report, by the results push's load_ramp, and by a push the
// outbox re-sends. get_full_report (and the report on disk) keeps the load tool's message.

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

const lkMarker = "AC-F22-APP-MARKER-31c7"

// lkRows are the issue's two rows (twice the header, once the chunk) and one refused connection, as JTL rows.
func lkRows() []report.LoadSample {
	row := func(class, msg string) report.LoadSample {
		return report.LoadSample{StartMs: 1, ElapsedMs: 1, Code: "Non HTTP response code: " + class, Message: "Non HTTP response message: " + msg}
	}
	return []report.LoadSample{
		row("org.apache.http.ProtocolException", "Invalid header: session=hunter2"),
		row("org.apache.http.ProtocolException", "Invalid header: session=hunter2"),
		row("org.apache.http.MalformedChunkCodingException", "Bad chunk header: "+lkMarker),
		row("org.apache.http.conn.HttpHostConnectException", "Connect to sut:8080 [sut/10.0.0.1] failed: Connection refused"),
		{StartMs: 1, ElapsedMs: 1, Code: "200", Message: "OK"},
	}
}

// lkReport: a plain ## LOAD check, an HTTP Load ramp and an AMQP Load ramp the broker paused, every error and reason
// built from rows by the code that builds them in a run (report.LoadErrorsFrom; amqpload sets the pause's word with
// report.BlockedKind).
func lkReport() *report.Report {
	errs := report.LoadErrorsFrom(lkRows(), nil)
	pause := "low on memory " + lkMarker
	return &report.Report{
		RunID: "run_lk1", Mode: "build", ClaimsAsWritten: true,
		Summary: report.Summary{Total: 3, Passed: 1, Failed: 2},
		Layers: []report.Layer{{Layer: "Load", Scenarios: []report.ScenarioResult{
			{ID: "LOAD-001", Status: "failed", Load: &report.LoadStats{Samples: 5, ErrorRate: 0.8, Errors: errs}},
			{ID: "HL-001", Status: "failed", LoadDriver: "http", LoadTarget: "api-lab", LoadSteps: []report.LoadStep{
				{Step: 1, Sessions: 10, Status: report.LoadStepMeasured, Errors: map[string]int{"transport": 4}, ErrorReasons: errs}}},
			{ID: "AMQL-001", Status: "passed", LoadDriver: "amqp", LoadTarget: "lab-broker", LoadSteps: []report.LoadStep{
				{Step: 1, Sessions: 4, Status: report.LoadStepBlocked, Errors: map[string]int{"blocked": 3},
					Blocked:        []report.BlockedPeriod{{SinceMs: 1000, UntilMs: 4000, Reason: pause, Kind: report.BlockedKind(pause)}},
					BlockedSeconds: 3}}},
		}}},
	}
}

var lkTexts = []string{"hunter2", "session=", "Invalid header", "Bad chunk", lkMarker, "low on", "Connection refused", "HttpHostConnectException"}

func lkNoTexts(t *testing.T, what string, b []byte) {
	t.Helper()
	for _, bad := range lkTexts {
		if bytes.Contains(b, []byte(bad)) {
			t.Errorf("%s carries %q: %s", what, bad, b)
		}
	}
}

func lkSum(es []report.LoadError) int {
	n := 0
	for _, e := range es {
		n += e.Count
	}
	return n
}

func TestLimitedReport_LoadErrorsLeaveAsWordsAndCounts_TheFullReportKeepsTheMessage(t *testing.T) {
	rep := lkReport()
	root := writeRunFixture(t, rep)
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})
	args := json.RawMessage(`{"run_id":"run_lk1"}`)

	full, err := cf(context.Background(), "get_full_report", args)
	if err != nil {
		t.Fatalf("get_full_report: %v", err)
	}
	for _, want := range []string{"Invalid header: session=hunter2", "Bad chunk header: " + lkMarker, "low on memory " + lkMarker} {
		if !bytes.Contains(full, []byte(want)) {
			t.Fatalf("positive control: the full report lost %q, so the checks below could not fail", want)
		}
	}

	lim, err := cf(context.Background(), "get_limited_report", args)
	if err != nil {
		t.Fatalf("get_limited_report: %v", err)
	}
	lkNoTexts(t, "get_limited_report", lim)

	var got report.Report
	if err := json.Unmarshal(lim, &got); err != nil {
		t.Fatalf("limited report: %v", err)
	}
	sc := got.Layers[0].Scenarios
	words := []report.LoadError{{Kind: "protocol", Count: 3}, {Kind: "refused", Count: 1}}
	if !reflect.DeepEqual(sc[0].Load.Errors, words) {
		t.Errorf("load.errors = %+v, want %+v", sc[0].Load.Errors, words)
	}
	if !reflect.DeepEqual(sc[1].LoadSteps[0].ErrorReasons, words) {
		t.Errorf("load_steps[].error_reasons = %+v, want %+v", sc[1].LoadSteps[0].ErrorReasons, words)
	}
	if want := []report.BlockedPeriod{{SinceMs: 1000, UntilMs: 4000, Kind: "memory"}}; !reflect.DeepEqual(sc[2].LoadSteps[0].Blocked, want) {
		t.Errorf("load_steps[].blocked = %+v, want %+v", sc[2].LoadSteps[0].Blocked, want)
	}
	// the counts add up to the report's own totals
	if a, b := lkSum(sc[0].Load.Errors), lkSum(rep.Layers[0].Scenarios[0].Load.Errors); a != b || a != 4 {
		t.Errorf("load.errors sum %d, report %d, want 4", a, b)
	}
	if a, b := lkSum(sc[1].LoadSteps[0].ErrorReasons), lkSum(rep.Layers[0].Scenarios[1].LoadSteps[0].ErrorReasons); a != b {
		t.Errorf("error_reasons sum %d, report %d", a, b)
	}
}

// get_limited_report on a report.json written before this change (no kind anywhere): the words are derived from the
// reasons, and no text leaves.
func TestLimitedReport_AReportWrittenBeforeTheWordsSendsWordsToo(t *testing.T) {
	rep := lkReport()
	sc := rep.Layers[0].Scenarios
	for i := range sc[0].Load.Errors {
		sc[0].Load.Errors[i].Kind = ""
	}
	sc[1].LoadSteps[0].ErrorReasons = append([]report.LoadError(nil), sc[0].Load.Errors...)
	sc[2].LoadSteps[0].Blocked[0].Kind = ""
	root := writeRunFixture(t, rep)
	lim, err := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})(context.Background(), "get_limited_report", json.RawMessage(`{"run_id":"run_lk1"}`))
	if err != nil {
		t.Fatalf("get_limited_report: %v", err)
	}
	lkNoTexts(t, "get_limited_report of an older report", lim)
	for _, want := range []string{`{"count":3,"kind":"protocol"}`, `{"count":1,"kind":"refused"}`, `"kind":"memory"`} {
		if !bytes.Contains(lim, []byte(want)) {
			t.Errorf("get_limited_report lacks %s: %s", want, lim)
		}
	}
}

// The results push: the ramp holds the word and the count, the word and the times, never a reason; the report itself
// is not changed by building the push (it is hashed and kept).
func TestMapReport_ThePushedRampHoldsWordsCountsAndTimes(t *testing.T) {
	rep := lkReport()
	before, _ := json.Marshal(rep)
	push := mapReport(rep, "run_lk1", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	after, _ := json.Marshal(rep)
	if !bytes.Equal(before, after) {
		t.Fatal("building the push changed the report")
	}
	lkNoTexts(t, "the pushed load_ramp", push.LoadRamp)
	if bytes.Contains(push.LoadRamp, []byte(`"reason":`)) {
		t.Errorf("the pushed load_ramp has a reason key: %s", push.LoadRamp)
	}
	var got []report.LoadRampEntry
	if err := json.Unmarshal(push.LoadRamp, &got); err != nil || len(got) != 2 {
		t.Fatalf("load_ramp = %s (%v)", push.LoadRamp, err)
	}
	if want := []report.LoadError{{Kind: "protocol", Count: 3}, {Kind: "refused", Count: 1}}; !reflect.DeepEqual(got[1].Steps[0].ErrorReasons, want) {
		t.Errorf("HL-001 error_reasons = %+v, want %+v", got[1].Steps[0].ErrorReasons, want)
	}
	if want := []report.BlockedPeriod{{SinceMs: 1000, UntilMs: 4000, Kind: "memory"}}; !reflect.DeepEqual(got[0].Steps[0].Blocked, want) {
		t.Errorf("AMQL-001 blocked = %+v, want %+v", got[0].Steps[0].Blocked, want)
	}
	// and the whole push, not only its ramp
	b, _ := json.Marshal(push)
	lkNoTexts(t, "the push", b)
}

// A push queued by an older executor (its ramp still holds the texts) is re-sent by an updated one after an upgrade on
// the same volume: the outbox sends the ramp as words, counts and times. The entry on disk is the executor's own.
func TestOutbox_DrainSendsAQueuedRampWithoutTexts(t *testing.T) {
	old := `[{"scenario_id":"HL-001","target":"api-lab","driver":"http","steps":[{"step":1,"sessions":10,"status":"measured",` +
		`"window_seconds":0,"offered_per_s":0,"sent_per_s":0,"confirmed_per_s":0,"delivered_per_s":0,"delivered_ratio":0,` +
		`"errors":{"transport":3},"comfortable":false,"error_reasons":[` +
		`{"reason":"org.apache.http.ProtocolException: Invalid header: session=hunter2","count":2},` +
		`{"reason":"org.apache.http.MalformedChunkCodingException: Bad chunk header: ` + lkMarker + `","count":1}]}]},` +
		`{"scenario_id":"AMQL-001","target":"lab-broker","driver":"amqp","steps":[{"step":1,"sessions":4,"status":"blocked",` +
		`"window_seconds":0,"offered_per_s":0,"sent_per_s":0,"confirmed_per_s":0,"delivered_per_s":0,"delivered_ratio":0,` +
		`"blocked":[{"since_ms":10,"until_ms":20,"reason":"low on disk ` + lkMarker + `"}],"comfortable":false}]}]`
	o := &Outbox{Dir: filepath.Join(t.TempDir(), "outbox"), Log: func(string, ...any) {}}
	p := samplePush("20261015T170000000", 1, 1)
	p.LoadRamp = json.RawMessage(old)
	if err := o.Put(p); err != nil {
		t.Fatal(err)
	}
	var sent []federation.ResultsPush
	if d, _ := o.Drain(context.Background(), func(_ context.Context, p federation.ResultsPush) error { sent = append(sent, p); return nil }); d != 1 {
		t.Fatalf("delivered %d, want 1", d)
	}
	ramp := sent[0].LoadRamp
	lkNoTexts(t, "the re-sent load_ramp", ramp)
	for _, want := range []string{`"error_reasons":[{"kind":"protocol","count":3}]`, `"blocked":[{"since_ms":10,"until_ms":20,"kind":"disk"}]`} {
		if !bytes.Contains(ramp, []byte(want)) {
			t.Errorf("re-sent load_ramp lacks %s: %s", want, ramp)
		}
	}
}

// A queued ramp that does not decode is not sent at all (the field is optional): never as raw bytes.
func TestOutbox_DrainDropsAQueuedRampItCannotRead(t *testing.T) {
	o := &Outbox{Dir: filepath.Join(t.TempDir(), "outbox"), Log: func(string, ...any) {}}
	p := samplePush("20261015T170000001", 1, 1)
	p.LoadRamp = json.RawMessage(`{"not":"an array","reason":"` + lkMarker + `"}`)
	if err := o.Put(p); err != nil {
		t.Fatal(err)
	}
	var sent []federation.ResultsPush
	o.Drain(context.Background(), func(_ context.Context, p federation.ResultsPush) error { sent = append(sent, p); return nil })
	if len(sent) != 1 || sent[0].LoadRamp != nil {
		t.Fatalf("sent = %+v, want one push with no load_ramp", sent)
	}
}
