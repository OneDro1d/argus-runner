package argus

import (
	"path/filepath"
	"testing"
)

// ARGUS-CMP-3: a compare run captures the environment fingerprint even with no `## LOAD`; every other
// mode is exactly as before (nil for a non-load run).
func TestCaptureEnvironmentForMode_CompareCapturesWithoutLoad_OthersDoNot(t *testing.T) {
	t.Setenv(sutNamespaceEnvVar, "")
	scns := []scenarioFile{plainScenarioFile(t)}
	for _, mode := range []string{"", "build", "final", "scheduled", "rehearsal"} {
		if got := captureEnvironmentForMode(scns, mode); got != nil {
			t.Errorf("mode %q: a non-load run must capture nothing, got %+v", mode, got)
		}
	}
	got := captureEnvironmentForMode(scns, "compare")
	if got == nil || got.Captured || got.Reason == "" {
		t.Fatalf("mode compare: want a Capture that says why it could not run (no namespace here), got %+v", got)
	}
	if captureEnvironmentIfNeeded(scns) != nil {
		t.Errorf("the old entry point changed behaviour")
	}
}

// The stored outputs and the capture scratch are bodies. They sit in the results directory, so the
// allow-list that bundling code takes (PublishableResults) must keep them out: pinned here by name.
func TestResultsBundle_NeverCarriesStoredOutputsOrCaptureScratch(t *testing.T) {
	for _, rel := range []string{"outputs/run-1/S-1.1.json", "outputs/run-1/S-1__create.1.json", "capture/c-123/1.body", "capture/c-123/1.status"} {
		if PublishableResult(rel) {
			t.Errorf("%s may leave the environment", rel)
		}
	}
	if !PublishableResult("report.json") || !PublishableResult("runs/run-1.json") {
		t.Errorf("the allow-list lost its own entries")
	}
}

func TestDockerRunner_JMeterCapturePathIsTheContainersViewOfTheResultsVolume(t *testing.T) {
	d := &DockerRunner{HostResultsRoot: filepath.Join("/", "srv", "results")}
	if got := d.JMeterCapturePath(filepath.Join("/", "srv", "results", "local", "capture", "c-1")); got != "/results/local/capture/c-1" {
		t.Errorf("got %q", got)
	}
	// outside the volume: unchanged, so JMeter finds nothing and the capture records nothing
	out := filepath.Join("/", "elsewhere", "c-1")
	if got := d.JMeterCapturePath(out); got != out {
		t.Errorf("a path outside the results volume must be returned unchanged, got %q", got)
	}
}
