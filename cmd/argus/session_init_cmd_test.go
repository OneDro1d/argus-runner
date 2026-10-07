package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// session_init_cmd_test.go — `argus tester init` / `argus builder init` (onboarding review items 18, 19).
// Every test runs against a temp HOME / temp notepad dir and an httptest control plane; the real
// ~/.config/argus is never touched.

func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ARGUS_CP_URL", "")
	return home
}

func runInit(t *testing.T, args ...string) (rc int, stdout string) {
	t.Helper()
	stdout = captureStdout(t, func() { rc = dispatch(args) })
	return
}

func TestTesterInit_WritesEverythingAndTellsTheHumanTheOneThing(t *testing.T) {
	home := withTempHome(t)
	work := t.TempDir()
	mcp := filepath.Join(work, ".mcp.json")
	dir := filepath.Join(work, "bin")
	rc, out := runInit(t, "tester", "init", "msgbus", "--dir", dir, "--mcp-json", mcp, "--control-plane", "https://cp.example")
	if rc != exitOK {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	envFile := filepath.Join(home, ".config", "argus", "tester.env")
	want := "paste your token into " + envFile + " after ARGUS_TESTER_TOKEN_MSGBUS="
	if !strings.Contains(out, want) {
		t.Errorf("stdout lacks the one thing to do (%q):\n%s", want, out)
	}
	for _, p := range []string{envFile, filepath.Join(dir, "argus-headers-msgbus.sh"), filepath.Join(dir, "argus-msgbus"), mcp} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("not written: %s", p)
		}
	}
}

func TestTesterInit_FlagsAfterTheAppAreFine_AndNoAppIsAUsageError(t *testing.T) {
	withTempHome(t)
	if rc, _ := runInit(t, "tester", "init", "--dir", t.TempDir(), "--mcp-json", filepath.Join(t.TempDir(), ".mcp.json")); rc != exitUsage {
		t.Errorf("no app: rc=%d, want exitUsage", rc)
	}
	if rc, _ := runInit(t, "tester", "init", "msgbus", "extra"); rc != exitUsage {
		t.Errorf("two positionals: rc=%d, want exitUsage", rc)
	}
	if rc, _ := runInit(t, "tester", "nonsense"); rc != exitUsage {
		t.Errorf("unknown subcommand: rc=%d, want exitUsage", rc)
	}
}

func TestTesterInit_ANonEmptyTokenIsNeverTouchedOrPrinted(t *testing.T) {
	home := withTempHome(t)
	envDir := filepath.Join(home, ".config", "argus")
	_ = os.MkdirAll(envDir, 0o700)
	envFile := filepath.Join(envDir, "tester.env")
	const secret = "odts_SECRET-MARKER-42"
	body := "ARGUS_TESTER_TOKEN_MSGBUS=" + secret + "\n"
	_ = os.WriteFile(envFile, []byte(body), 0o600)
	work := t.TempDir()
	rc, out := runInit(t, "tester", "init", "msgbus", "--dir", filepath.Join(work, "bin"), "--mcp-json", filepath.Join(work, ".mcp.json"))
	if rc != exitOK {
		t.Fatalf("rc=%d", rc)
	}
	if strings.Contains(out, secret) || strings.Contains(out, "SECRET-MARKER") {
		t.Errorf("LEAK: stdout carries the token:\n%s", out)
	}
	if strings.Contains(out, "paste your token") {
		t.Errorf("told the human to paste a token that is already there:\n%s", out)
	}
	got, _ := os.ReadFile(envFile)
	if !strings.HasPrefix(string(got), body) {
		t.Errorf("the existing line was changed:\n%s", got)
	}
}

// builderCP is testerCP with tools the builder test chooses (runner-only, or with an author tool leaking in).
func builderSetup(t *testing.T, token string, tools []string, unauth bool) (dir string, cp *testerCP) {
	t.Helper()
	withTempHome(t)
	dir = t.TempDir()
	cp = newTesterCP(t, token, tools, "i", "null")
	if unauth {
		cp = newTesterCP(t, "never-this-one", tools, "i", "null")
	}
	return dir, cp
}

func setBuilderToken(t *testing.T, dir, token string) {
	t.Helper()
	p := filepath.Join(dir, ".argus-builder", "builder.env")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(b), "ARGUS_BUILDER_TOKEN_MSGBUS=\n", "ARGUS_BUILDER_TOKEN_MSGBUS="+token+"\n", 1)
	if out == string(b) {
		t.Fatal("stanza not found")
	}
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

const builderTok = "odts_BUILDER-tok-77aa"

func TestBuilderInit_WithoutATokenWritesTheFilesAndDoesNotWireMCP(t *testing.T) {
	dir, cp := builderSetup(t, builderTok, []string{"runner__run"}, false)
	rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
	if rc != exitOK {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "paste your builder token into "+filepath.Join(dir, ".argus-builder", "builder.env")+" after ARGUS_BUILDER_TOKEN_MSGBUS=") {
		t.Errorf("no instruction:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
		t.Errorf(".mcp.json was written for a builder with no verified token")
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.bearers) != 0 {
		t.Errorf("the control plane was contacted without a token")
	}
}

func TestBuilderInit_RunnerOnlyToolsPassAndWireMCP(t *testing.T) {
	dir, cp := builderSetup(t, builderTok, []string{"runner__run", "runner__get_report", "runner__validate_config"}, false)
	if rc, _ := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL); rc != exitOK {
		t.Fatal("first run failed")
	}
	setBuilderToken(t, dir, builderTok)
	rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
	if rc != exitOK {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "3 tools, all runner__") {
		t.Errorf("no evidence line:\n%s", out)
	}
	mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil || !strings.Contains(string(mcp), "argus-headers-msgbus-builder.sh") {
		t.Errorf(".mcp.json not wired after a passing verification: %v\n%s", err, mcp)
	}
	if strings.Contains(out, builderTok) || strings.Contains(out, "BUILDER-tok") {
		t.Errorf("LEAK: token in stdout:\n%s", out)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.bearers) == 0 || cp.bearers[0] != builderTok {
		t.Errorf("verification did not present the builder token")
	}
}

// The builder-sees-an-author-tool case: the run must FAIL loudly, refuse to wire .mcp.json, and pull back
// an entry an earlier run may have written.
func TestBuilderInit_AnAuthorToolFailsLoudlyAndUnwires(t *testing.T) {
	dir, cp := builderSetup(t, builderTok, []string{"runner__run", "author_write_scenario"}, false)
	if rc, _ := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL); rc != exitOK {
		t.Fatal("first run failed")
	}
	setBuilderToken(t, dir, builderTok)
	// an entry from some earlier wiring
	mcpPath := filepath.Join(dir, ".mcp.json")
	_ = os.WriteFile(mcpPath, []byte(`{"mcpServers":{"argus":{"type":"http","url":"x","headersHelper":"y"},"keep":{"type":"http","url":"z"}}}`), 0o600)
	rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
	if rc != exitDenied {
		t.Fatalf("rc=%d, want exitDenied (%d)\n%s", rc, exitDenied, out)
	}
	for _, want := range []string{"author_write_scenario", "author scope", "REFUSED"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure text lacks %q:\n%s", want, out)
		}
	}
	mcp, _ := os.ReadFile(mcpPath)
	if strings.Contains(string(mcp), `"argus"`) || !strings.Contains(string(mcp), `"keep"`) {
		t.Errorf("the argus entry must be removed and the foreign one kept:\n%s", mcp)
	}
	if strings.Contains(out, builderTok) {
		t.Errorf("LEAK: token in stdout")
	}
}

func TestBuilderInit_ASignInChallengeIsRefused(t *testing.T) {
	dir, cp := builderSetup(t, builderTok, []string{"runner__run"}, true)
	if rc, _ := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL); rc != exitOK {
		t.Fatal("first run failed")
	}
	setBuilderToken(t, dir, builderTok)
	rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
	if rc != exitDenied {
		t.Fatalf("rc=%d, want exitDenied\n%s", rc, out)
	}
	for _, want := range []string{"sign-in", "REFUSED", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
		t.Errorf(".mcp.json written despite a sign-in challenge")
	}
}

func TestBuilderInit_NoToolsAtAllFails(t *testing.T) {
	dir, cp := builderSetup(t, builderTok, nil, false)
	_, _ = runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL)
	setBuilderToken(t, dir, builderTok)
	if rc, out := runInit(t, "builder", "init", "msgbus", "--dir", dir, "--control-plane", cp.srv.URL); rc != exitDenied {
		t.Errorf("rc=%d\n%s", rc, out)
	}
}

func TestInitCommands_HelpNamesThemselvesAndTheirFlags(t *testing.T) {
	for cmd, wants := range map[string][]string{
		"tester":  {"tester init <app>", "ARGUS_TESTER_TOKEN_<APP>", "--dir", "--mcp-json", "never reads", "0600"},
		"builder": {"builder init <app>", "ARGUS_BUILDER_TOKEN_<APP>", "runner__", "sign-in", "--dir"},
	} {
		rc, out := runHelp(t, cmd, "--help")
		if rc != exitOK {
			t.Errorf("%s --help rc=%d", cmd, rc)
		}
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("%s --help lacks %q:\n%s", cmd, w, out)
			}
		}
	}
}
