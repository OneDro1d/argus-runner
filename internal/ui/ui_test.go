package ui

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// VR-L3 / UC-62: exit 0 WITH a results artifact -> passed (a test actually ran + left a trace).
func TestClassify_Pass(t *testing.T) {
	if s, _ := Classify(0, true, ""); s != "passed" {
		t.Fatalf("exit 0 with results should be passed, got %q", s)
	}
}

// VR-L3 (blind-gate finding 2026-06-21): "exits 0 but produces NO trace/results artifact"
// is a DISTINCT EXECUTION failure, NEVER a silent green ("absent results != pass"). An
// all-skipped / no-tests-run Playwright invocation exits 0 yet exercises nothing.
func TestClassify_ExitZeroNoArtifactIsExecutionFailure(t *testing.T) {
	s, o := Classify(0, false, "1 skipped")
	if s != report.StatusError {
		t.Fatalf("exit 0 with NO results artifact must be the execution-error status (not a silent green), got %q", s)
	}
	if !strings.Contains(o, "EXECUTION") {
		t.Fatalf("observed should name it an execution failure: %q", o)
	}
}

// A SUT failure (non-zero WITH results) -> "failed" (DOM/backend).
func TestClassify_SUTFailure(t *testing.T) {
	if s, _ := Classify(1, true, "1 failed"); s != "failed" {
		t.Fatalf("non-zero with results should be a SUT failure, got %q", s)
	}
}

// VR-L3: an execution/harness failure (non-zero WITHOUT results) is the DISTINCT
// "error" status — never a SUT pass/fail, never a silent green.
func TestClassify_ExecutionFailureDistinct(t *testing.T) {
	s, o := Classify(1, false, "browserType.launch: Executable doesn't exist\n...")
	if s != report.StatusError {
		t.Fatalf("a harness failure must be the distinct execution-error status, got %q", s)
	}
	if !strings.Contains(o, "EXECUTION") {
		t.Fatalf("observed should name it an execution failure: %q", o)
	}
}

// #152 review point 3: Classify's no-artifact row quotes stderr's first line, and Playwright opens
// stderr with colour codes on a line of their own — cutting before stripping returned "".
func TestClassify_FirstLineSkipsColourCodes(t *testing.T) {
	_, obs := Classify(1, false, "\x1b[2m\x1b[22m\n\x1b[31mError: browserType.launch: Executable doesn't exist\x1b[39m\n    at x")
	if !strings.HasSuffix(obs, ": Error: browserType.launch: Executable doesn't exist") {
		t.Errorf("Classify dropped the error behind the colour codes: %q", obs)
	}
}
