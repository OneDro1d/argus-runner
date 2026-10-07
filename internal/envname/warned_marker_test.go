package envname

import (
	"os"
	"strings"
	"testing"
)

// an onboarding runs several argus processes (and some in containers); each used to
// print "ARGUS_AUTHOR_TOKEN is deprecated" once, so the operator saw it three times. The first
// warning now leaves a marker in the environment, which every child inherits, so the line appears
// once per onboarding, not once per process.
func TestLookup_WarnedMarkerSuppressesWarning(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_M1", "ARGUS_TEST_OLD_M1"
	t.Setenv(oldVar, "old-value")
	t.Setenv(WarnedEnv, "SOMETHING_ELSE,"+oldVar)

	var got string
	stderr := captureStderr(t, func() { got = Lookup(newVar, oldVar) })
	if got != "old-value" {
		t.Errorf("Lookup() = %q, want the fallback value", got)
	}
	if stderr != "" {
		t.Errorf("an ancestor process already warned for %s (marker set); got a second warning: %q", oldVar, stderr)
	}
}

func TestLookup_FirstWarningLeavesMarkerForChildren(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_M2", "ARGUS_TEST_OLD_M2"
	t.Setenv(oldVar, "old-value")
	t.Setenv(WarnedEnv, "")

	stderr := captureStderr(t, func() { Lookup(newVar, oldVar) })
	if !strings.Contains(stderr, oldVar) {
		t.Fatalf("the first fallback must still warn: %q", stderr)
	}
	if !strings.Contains(os.Getenv(WarnedEnv), oldVar) {
		t.Errorf("%s = %q: the warning left no marker, so a child process would warn again", WarnedEnv, os.Getenv(WarnedEnv))
	}
	if strings.Contains(os.Getenv(WarnedEnv), "old-value") {
		t.Errorf("the marker must carry names only, never a value")
	}
}
