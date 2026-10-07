package envname

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns whatever fn wrote there.
// The helper contract (see envname.go) requires the deprecation warning to land on STDERR ONLY —
// never stdout, since CLI commands emit JSON on stdout and a warning line would corrupt that
// protocol — so every test here captures stderr specifically rather than trusting a return value.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String()
}

func TestLookup_NewNameWins_NoWarning(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_1", "ARGUS_TEST_OLD_1"
	t.Setenv(newVar, "new-value")
	t.Setenv(oldVar, "old-value")

	var got string
	stderr := captureStderr(t, func() { got = Lookup(newVar, oldVar) })

	if got != "new-value" {
		t.Errorf("Lookup() = %q, want %q (new name must win when both are set)", got, "new-value")
	}
	if stderr != "" {
		t.Errorf("Lookup() with both set printed a warning, want none: %q", stderr)
	}
}

func TestLookup_FallsBackToOldName_WithWarning(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_2", "ARGUS_TEST_OLD_2"
	t.Setenv(oldVar, "old-value")
	// newVar deliberately left unset.

	var got string
	stderr := captureStderr(t, func() { got = Lookup(newVar, oldVar) })

	if got != "old-value" {
		t.Errorf("Lookup() = %q, want fallback value %q", got, "old-value")
	}
	if !strings.Contains(stderr, oldVar) || !strings.Contains(stderr, newVar) {
		t.Errorf("deprecation warning must name BOTH vars, got: %q", stderr)
	}
	if strings.Contains(stderr, "old-value") {
		t.Errorf("deprecation warning must never contain the value, got: %q", stderr)
	}
}

func TestLookup_EmptyNewName_TreatedAsUnset(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_3", "ARGUS_TEST_OLD_3"
	t.Setenv(newVar, "")
	t.Setenv(oldVar, "old-value")

	got := Lookup(newVar, oldVar)
	if got != "old-value" {
		t.Errorf("Lookup() = %q, want fallback %q when new name is set but empty", got, "old-value")
	}
}

func TestLookup_NeitherSet_ReturnsEmpty_NoWarning(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_4", "ARGUS_TEST_OLD_4"

	var got string
	stderr := captureStderr(t, func() { got = Lookup(newVar, oldVar) })

	if got != "" {
		t.Errorf("Lookup() = %q, want empty", got)
	}
	if stderr != "" {
		t.Errorf("Lookup() with neither set printed a warning, want none: %q", stderr)
	}
}

func TestLookup_WarnsOnlyOncePerProcess(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_5", "ARGUS_TEST_OLD_5"
	t.Setenv(oldVar, "old-value")

	var first, second string
	stderr := captureStderr(t, func() {
		first = Lookup(newVar, oldVar)
		second = Lookup(newVar, oldVar)
	})

	if first != "old-value" || second != "old-value" {
		t.Fatalf("Lookup() calls = %q, %q, want both %q", first, second, "old-value")
	}
	if n := strings.Count(stderr, oldVar); n != 1 {
		t.Errorf("expected exactly one warning line naming %s, got %d in: %q", oldVar, n, stderr)
	}
}

func TestLookup_NeverPrintsToStdout(t *testing.T) {
	newVar, oldVar := "ARGUS_TEST_NEW_6", "ARGUS_TEST_OLD_6"
	t.Setenv(oldVar, "old-value")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	Lookup(newVar, oldVar)
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("Lookup() wrote to stdout: %q (CLI commands emit JSON on stdout; a warning there would corrupt it)", buf.String())
	}
}

func TestLookup_DistinctPairsWarnIndependently(t *testing.T) {
	// Regression guard for a "warn once per process" implementation keyed on the WRONG thing (e.g.
	// a single global bool instead of per-old-name): a warning already emitted for the executor-secret
	// pair must not silently suppress the warning for the cp-author-token pair.
	t.Setenv(ExecutorSecretDeprecated, "old-exec-value")
	t.Setenv(CPAuthorTokenDeprecated, "old-cp-value")

	var stderr string
	stderr = captureStderr(t, func() {
		Lookup(ExecutorSecret, ExecutorSecretDeprecated)
		Lookup(CPAuthorToken, CPAuthorTokenDeprecated)
	})
	if !strings.Contains(stderr, ExecutorSecretDeprecated) {
		t.Errorf("missing warning for %s in: %q", ExecutorSecretDeprecated, stderr)
	}
	if !strings.Contains(stderr, CPAuthorTokenDeprecated) {
		t.Errorf("missing warning for %s in: %q", CPAuthorTokenDeprecated, stderr)
	}
}

// item 9 (msgbus tester 2026-09-28): "the executor's startup line 'ARGUS_AUTHOR_TOKEN is
// deprecated' must name the fix: `argus secrets migrate`... and say when the alias ends" — a bare
// "will be removed in a later release" is not a remedy AND not a floor the reader can plan around.
func TestLookup_ExecutorSecretDeprecated_NamesTheMigrateCommandAndTheFloor(t *testing.T) {
	resetWarned(t, ExecutorSecretDeprecated) // warned is a process-global "once" set; other tests in this file already trip it for the real constants
	t.Setenv(ExecutorSecretDeprecated, "old-exec-value")

	var got string
	stderr := captureStderr(t, func() { got = Lookup(ExecutorSecret, ExecutorSecretDeprecated) })

	if got != "old-exec-value" {
		t.Fatalf("Lookup() = %q, want the fallback value", got)
	}
	for _, want := range []string{"argus secrets migrate", "v0.4.0", "0.3.x", ExecutorSecretDeprecated, ExecutorSecret} {
		if !strings.Contains(stderr, want) {
			t.Errorf("deprecation warning missing %q, got: %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "old-exec-value") {
		t.Errorf("deprecation warning must never contain the value, got: %q", stderr)
	}
}

// TestLookup_CPAuthorTokenDeprecated_KeepsTheGenericWording: the OTHER pair has no server-side
// migration command (a person's own session token, nothing to rewrite), so it must NOT claim one —
// naming a command that doesn't exist for this var would be worse than the vague wording it replaces.
func TestLookup_CPAuthorTokenDeprecated_KeepsTheGenericWording(t *testing.T) {
	resetWarned(t, CPAuthorTokenDeprecated)
	t.Setenv(CPAuthorTokenDeprecated, "old-cp-value")

	stderr := captureStderr(t, func() { Lookup(CPAuthorToken, CPAuthorTokenDeprecated) })

	if strings.Contains(stderr, "argus secrets migrate") {
		t.Errorf("the cp-author-token pair has no migrate command — must not claim one: %q", stderr)
	}
	if !strings.Contains(stderr, CPAuthorTokenDeprecated) || !strings.Contains(stderr, CPAuthorToken) {
		t.Errorf("deprecation warning must still name both vars, got: %q", stderr)
	}
}

// resetWarned clears the process-global "warned once" mark for oldName, so a test exercising the
// warning TEXT for one of the two real, shared constants is not silently skipped by an earlier
// test in this file (or run) having already tripped it.
func resetWarned(t *testing.T, oldName string) {
	t.Helper()
	// An earlier warning in this process leaves the cross-process marker (WarnedEnv) behind; a test of
	// the warning TEXT must not be silenced by it. t.Setenv restores the prior value afterwards.
	t.Setenv(WarnedEnv, "")
	warnedMu.Lock()
	delete(warned, oldName)
	warnedMu.Unlock()
	t.Cleanup(func() {
		warnedMu.Lock()
		delete(warned, oldName)
		warnedMu.Unlock()
	})
}

func TestConstants_NamedCorrectly(t *testing.T) {
	cases := map[string]string{
		ExecutorSecret:           "ARGUS_EXECUTOR_SECRET",
		ExecutorSecretDeprecated: "ARGUS_AUTHOR_TOKEN",
		CPAuthorToken:            "ARGUS_CP_AUTHOR_TOKEN",
		CPAuthorTokenDeprecated:  "ARGUS_CP_TOKEN",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant = %q, want %q", got, want)
		}
	}
}
