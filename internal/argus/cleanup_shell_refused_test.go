package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// (#709) — a bash cleanup the shell refuses (exit 2 inside a second) says so in the
// report row. Real `sh -c` through the local executor; no output is captured.

func runBashCleanup(t *testing.T, body string) report.CleanupResult {
	t.Helper()
	s := cleanupScenario(t, "```bash\n"+body+"\n```")
	r := &LocalJMeterRunner{TemplatesDir: t.TempDir()}
	got := RunCleanup(&config.Config{}, s, "tr-cln", r)
	if len(got) != 1 {
		t.Fatalf("one result per block: %+v", got)
	}
	return got[0]
}

func TestCleanup_ShellSyntaxErrorSaysTheShellRefusedIt(t *testing.T) {
	// `if` with no `then`/`fi` is a syntax error in dash and bash alike: exit status 2, immediately.
	res := runBashCleanup(t, "if true; then")
	if res.Outcome != report.CleanupFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Observed, "shell refused the script") || !strings.Contains(res.Observed, "bash-only syntax") ||
		!strings.Contains(res.Observed, "a command in the block that exits 2 looks the same") {
		t.Errorf("observed must say the shell refused the script: %q", res.Observed)
	}
	if !strings.Contains(res.Observed, "not captured") {
		t.Errorf("the output must stay discarded, and the row must say so: %q", res.Observed)
	}
}

func TestCleanup_SlowExitTwoKeepsTheOldText(t *testing.T) {
	res := runBashCleanup(t, "sleep 1.2; exit 2")
	if res.Outcome != report.CleanupFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.HasPrefix(res.Observed, "did not complete (its output is deliberately not captured") {
		t.Errorf("a slow exit 2 is not a syntax error; the old text must stay: %q", res.Observed)
	}
}

func TestCleanup_FastOtherExitKeepsTheOldText(t *testing.T) {
	res := runBashCleanup(t, "exit 1")
	if !strings.HasPrefix(res.Observed, "did not complete") {
		t.Errorf("only status 2 is read as a refusal: %q", res.Observed)
	}
}
