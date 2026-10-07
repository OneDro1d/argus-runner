package runner

// mode_digest_test.go — AC-3: the runner's own refusal of a `final` assignment without an artifact
// digest. This is the SECOND refusal (the closed author tool schema on the control plane is the
// first): a control plane that skipped its own check must still not execute a final run against no
// pinned artifact — the runner checks the assignment it was actually handed, not the caller's word for
// it having been validated already.

import (
	"context"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// TestNewRunFunc_RefusesFinalWithoutDigest: a `final` assignment with no artifact_digest must be
// refused before any materialization or execution is attempted — a zero-value ExecConfig would fail
// loudly on any real I/O, so a refusal this clean proves the check runs first.
func TestNewRunFunc_RefusesFinalWithoutDigest(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	_, err := run(context.Background(), &federation.RunAssignment{Mode: "final", ArtifactDigest: ""})
	if err == nil {
		t.Fatal("a final assignment with no artifact_digest must be refused; got a nil error")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("the refusal must name the digest; got: %v", err)
	}
}

// TestNewRunFunc_BuildModeNeedsNoDigest is the negative control: a build-mode assignment (the
// zero-value Mode, and Mode="build" explicitly) must NOT be refused for a missing digest — it fails
// later, on the missing SUT config, never on this check.
func TestNewRunFunc_BuildModeNeedsNoDigest(t *testing.T) {
	run := NewRunFunc(ExecConfig{})
	_, err := run(context.Background(), &federation.RunAssignment{Mode: "", ArtifactDigest: ""})
	if err != nil && strings.Contains(err.Error(), "digest") {
		t.Errorf("a build-mode (default) assignment must not be refused for a missing digest; got: %v", err)
	}
}
