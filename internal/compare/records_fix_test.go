package compare

import "testing"

// the predicate that says which checks record an output (records.go); mirrors the executor's dispatch.
func TestRecordsOutput_OnlyWhatTheExecutorRecords(t *testing.T) {
	cases := []struct {
		name string
		f    RecorderFacts
		want bool
	}{
		{"http-ingestion template", RecorderFacts{TemplateBase: "http-ingestion"}, true},
		{"http-idempotency template", RecorderFacts{TemplateBase: "http-idempotency"}, true},
		{"database-state template", RecorderFacts{TemplateBase: "database-state"}, false},
		{"message-flow template", RecorderFacts{TemplateBase: "message-flow"}, false},
		{"external-delivery template", RecorderFacts{TemplateBase: "external-delivery"}, false},
		{"saga-presence template", RecorderFacts{TemplateBase: "saga-presence"}, false},
		{"an http template with ## LOAD", RecorderFacts{TemplateBase: "http-ingestion", Load: true}, false},
		{"a chain with an http step", RecorderFacts{Chain: true, ChainHasHTTPStep: true}, true},
		{"a chain with no http step", RecorderFacts{Chain: true}, false},
		{"an mcp check", RecorderFacts{MCP: true, TemplateBase: "http-ingestion"}, false},
		{"a ui check", RecorderFacts{UI: true, TemplateBase: "http-ingestion"}, false},
		{"an AMQP Load check", RecorderFacts{AMQPLoad: true, TemplateBase: "http-ingestion"}, false},
	}
	for _, tc := range cases {
		if got := RecordsOutput(tc.f); got != tc.want {
			t.Errorf("%s: RecordsOutput = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// counts.could_not_run counts the reference member's own could-not-run cells (see result_calc.go).
func TestResult_TheReferenceMembersOwnCouldNotRunCellIsCountedAndNamed(t *testing.T) {
	rules := &Rules{Reference: RefMeasured, Output: OutputSel{Status: true, Body: true}}
	in := func(refRun Run) Input {
		return Input{
			SetHash: "S",
			Checks:  []Check{{ID: "A", Path: "a.md", Rules: rules}},
			Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
			Runs:    []Run{refRun},
		}
	}
	cases := []struct {
		name string
		run  Run
		note string
		cnr  int
	}{
		{"another set", Run{ID: "r1", Member: "old", Status: "completed", SetHash: "OTHER", Outcomes: map[string]string{"A": "passed"}}, NoteReferenceOtherSet, 1},
		{"failed", Run{ID: "r1", Member: "old", Status: "failed"}, NoteReferenceFailed, 1},
		{"not run yet", Run{ID: "r1", Member: "old", Status: "queued"}, NoteReferenceNotRun, 0},
	}
	for _, tc := range cases {
		out := Result(in(tc.run))
		if out.Counts.CouldNotRun != tc.cnr {
			t.Errorf("%s: counts.could_not_run = %d, want %d", tc.name, out.Counts.CouldNotRun, tc.cnr)
		}
		found := false
		for _, n := range out.Notes {
			if n == tc.note {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: notes = %v, want %q", tc.name, out.Notes, tc.note)
		}
		if out.Verdict != VerdictIncomplete {
			t.Errorf("%s: verdict = %q, want incomplete", tc.name, out.Verdict)
		}
	}
}
