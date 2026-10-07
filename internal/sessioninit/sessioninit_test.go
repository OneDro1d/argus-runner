package sessioninit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppNames(t *testing.T) {
	for _, tc := range []struct{ in, up, low string }{
		{"msgbus", "MSGBUS", "msgbus"},
		{"Shop-2.0", "SHOP_2_0", "shop-2-0"},
		{"my app", "MY_APP", "my-app"},
	} {
		up, low, err := AppNames(tc.in)
		if err != nil || up != tc.up || low != tc.low {
			t.Errorf("AppNames(%q) = %q, %q, %v; want %q, %q", tc.in, up, low, err, tc.up, tc.low)
		}
	}
	for _, bad := range []string{"", "  ", "../x", "1abc", "-a"} {
		if _, _, err := AppNames(bad); err == nil {
			t.Errorf("AppNames(%q) accepted a name that is not an app name", bad)
		}
	}
}

func testerOpts(t *testing.T) (Options, string) {
	t.Helper()
	home := t.TempDir()
	work := t.TempDir()
	return Options{Kind: Tester, App: "msgbus", Home: home, Dir: filepath.Join(home, ".config", "argus", "bin"),
		ControlPlane: "https://cp.example", MCPJSON: filepath.Join(work, ".mcp.json")}, home
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestTesterInit_WritesTheDocumentedPatternWithEmptyToken(t *testing.T) {
	o, home := testerOpts(t)
	res, err := Init(o)
	if err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(home, ".config", "argus")
	if m := mode(t, envDir); m != 0o700 {
		t.Errorf("config dir mode = %o, want 700", m)
	}
	envFile := filepath.Join(envDir, "tester.env")
	if m := mode(t, envFile); m != 0o600 {
		t.Errorf("tester.env mode = %o, want 600", m)
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "\nARGUS_TESTER_TOKEN_MSGBUS=\n") {
		t.Errorf("env file lacks the empty stanza:\n%s", env)
	}
	if !strings.Contains(string(env), "ARGUS_CP_URL=https://cp.example\n") {
		t.Errorf("env file lacks ARGUS_CP_URL:\n%s", env)
	}
	helper, _ := os.ReadFile(res.Helper)
	wantHelper := "#!/usr/bin/env bash\nset -euo pipefail\nset -a\n. ~/.config/argus/tester.env\nset +a\n" +
		"printf '{\"Authorization\":\"Bearer %s\"}\\n' \"$ARGUS_TESTER_TOKEN_MSGBUS\"\n"
	if string(helper) != wantHelper {
		t.Errorf("headersHelper differs from the documented pattern:\n%s", helper)
	}
	wrapper, _ := os.ReadFile(res.Wrapper)
	wantWrapper := "#!/usr/bin/env bash\nset -euo pipefail\nset -a\n. ~/.config/argus/tester.env\nset +a\n" +
		"export ARGUS_CP_AUTHOR_TOKEN=\"$ARGUS_TESTER_TOKEN_MSGBUS\"\nexport ARGUS_CP_TOKEN=\"$ARGUS_TESTER_TOKEN_MSGBUS\"\nexec argus \"$@\"\n"
	if string(wrapper) != wantWrapper {
		t.Errorf("wrapper differs from the documented pattern:\n%s", wrapper)
	}
	for _, p := range []string{res.Helper, res.Wrapper} {
		if m := mode(t, p); m != 0o700 {
			t.Errorf("%s mode = %o, want 700", p, m)
		}
	}
	mcp, _ := os.ReadFile(o.MCPJSON)
	if !strings.Contains(string(mcp), `"headersHelper"`) || !strings.Contains(string(mcp), res.Helper) ||
		!strings.Contains(string(mcp), "https://cp.example/mcp") {
		t.Errorf(".mcp.json entry wrong:\n%s", mcp)
	}
	if strings.Contains(string(mcp), "Bearer") || strings.Contains(string(mcp), "Authorization") {
		t.Errorf(".mcp.json must carry no token or header:\n%s", mcp)
	}
	if res.TokenSet {
		t.Errorf("a fresh init reported the token as set")
	}
}

func TestTesterInit_NeverOverwritesAndIsIdempotent(t *testing.T) {
	o, home := testerOpts(t)
	envDir := filepath.Join(home, ".config", "argus")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(envDir, "tester.env")
	const marker = "odts_PLANTED-MARKER-do-not-touch"
	pre := "ARGUS_CP_URL=https://other.example\nARGUS_TESTER_TOKEN_MSGBUS=" + marker + "\nARGUS_TESTER_TOKEN_SHOP=another-apps-token"
	// no trailing newline on purpose: appending must not glue a line onto the last one
	if err := os.WriteFile(envFile, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Init(o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TokenSet {
		t.Errorf("a non-empty token was not reported as set")
	}
	got, _ := os.ReadFile(envFile)
	if string(got) != pre {
		t.Errorf("existing env file was modified:\n%q\nwant\n%q", got, pre)
	}
	if m := mode(t, envFile); m != 0o600 {
		t.Errorf("an existing world-readable env file was left at %o, want it tightened to 600", m)
	}
	if strings.Contains(res.Message(), marker) {
		t.Errorf("the message carries a token value")
	}
	// A second app appends only its own line, after fixing the missing newline.
	o2 := o
	o2.App = "third"
	o2.MCPJSON = "" // one .mcp.json names one app's helper: the "argus" key is replaced, not stacked
	if _, err := Init(o2); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(envFile)
	if !strings.HasPrefix(string(got), pre+"\n") || !strings.HasSuffix(string(got), "ARGUS_TESTER_TOKEN_THIRD=\n") {
		t.Errorf("second app was not appended cleanly:\n%q", got)
	}
	// Re-running for the first app changes nothing at all.
	before, _ := os.ReadFile(envFile)
	mcpBefore, _ := os.ReadFile(o.MCPJSON)
	if _, err := Init(o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(envFile)
	mcpAfter, _ := os.ReadFile(o.MCPJSON)
	if string(before) != string(after) || string(mcpBefore) != string(mcpAfter) {
		t.Errorf("re-running changed a file")
	}
}

func TestTesterInit_EmptyStanzaIsKeptNotDuplicated(t *testing.T) {
	o, _ := testerOpts(t)
	if _, err := Init(o); err != nil {
		t.Fatal(err)
	}
	res, err := Init(o)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.EnvFile)
	if n := strings.Count(string(b), "ARGUS_TESTER_TOKEN_MSGBUS="); n != 1 {
		t.Errorf("stanza appears %d times", n)
	}
}

func TestTesterInit_KeepsAForeignMCPServerByteForByte(t *testing.T) {
	o, _ := testerOpts(t)
	orig := "{\n  \"mcpServers\": {\n    \"other\": {\"type\": \"http\", \"url\": \"https://x.example/mcp\"}\n  }\n}\n"
	if err := os.WriteFile(o.MCPJSON, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(o.MCPJSON)
	if !strings.Contains(string(b), `"other": {"type": "http", "url": "https://x.example/mcp"}`) || !strings.Contains(string(b), `"argus"`) {
		t.Errorf("merge lost the foreign server or missed ours:\n%s", b)
	}
}

func TestBuilderInit_LayoutAndNoMCPUntilAsked(t *testing.T) {
	dir := t.TempDir()
	res, err := Init(Options{Kind: Builder, App: "msgbus", Dir: dir, ControlPlane: "https://cp.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.EnvFile, filepath.Join(dir, ".argus-builder")) {
		t.Errorf("builder env file %s is not inside the notepad dir's .argus-builder", res.EnvFile)
	}
	if m := mode(t, filepath.Dir(res.EnvFile)); m != 0o700 {
		t.Errorf("state dir mode %o", m)
	}
	if m := mode(t, res.EnvFile); m != 0o600 {
		t.Errorf("env file mode %o", m)
	}
	env, _ := os.ReadFile(res.EnvFile)
	if !strings.Contains(string(env), "ARGUS_BUILDER_TOKEN_MSGBUS=\n") {
		t.Errorf("builder stanza missing:\n%s", env)
	}
	if strings.Contains(string(env), "TESTER") || strings.Contains(string(env), "AUTHOR") {
		t.Errorf("builder env must not name tester/author variables:\n%s", env)
	}
	helper, _ := os.ReadFile(res.Helper)
	if !strings.Contains(string(helper), res.EnvFile) || !strings.Contains(string(helper), `"$ARGUS_BUILDER_TOKEN_MSGBUS"`) {
		t.Errorf("builder helper does not read the builder env file:\n%s", helper)
	}
	wrapper, _ := os.ReadFile(res.Wrapper)
	w := string(wrapper)
	for _, want := range []string{"export ARGUS_RUNNER_TOKEN=\"$ARGUS_BUILDER_TOKEN_MSGBUS\"", "unset ARGUS_CP_AUTHOR_TOKEN", "ARGUS_SESSION_FILE="} {
		if !strings.Contains(w, want) {
			t.Errorf("builder wrapper lacks %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "export ARGUS_CP_AUTHOR_TOKEN") {
		t.Errorf("builder wrapper must never export an author token variable:\n%s", w)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
		t.Errorf(".mcp.json was written before the token was verified")
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(gi), ".argus-builder/") {
		t.Errorf(".gitignore does not guard the token directory:\n%s", gi)
	}
	// idempotent .gitignore
	if _, err := Init(Options{Kind: Builder, App: "msgbus", Dir: dir, ControlPlane: "https://cp.example"}); err != nil {
		t.Fatal(err)
	}
	gi2, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(gi) != string(gi2) {
		t.Errorf(".gitignore changed on re-run")
	}
}

func TestTesterInit_McpUrlFollowsTheEnvFileWhenNoFlagIsGiven(t *testing.T) {
	o, home := testerOpts(t)
	envDir := filepath.Join(home, ".config", "argus")
	_ = os.MkdirAll(envDir, 0o700)
	_ = os.WriteFile(filepath.Join(envDir, "tester.env"), []byte("ARGUS_CP_URL=https://from-file.example\n"), 0o600)
	o.ControlPlane = ""
	if _, err := Init(o); err != nil {
		t.Fatal(err)
	}
	mcp, _ := os.ReadFile(o.MCPJSON)
	if !strings.Contains(string(mcp), "https://from-file.example/mcp") {
		t.Errorf(".mcp.json ignores the env file's ARGUS_CP_URL:\n%s", mcp)
	}
}

func TestReadToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.env")
	_ = os.WriteFile(p, []byte("# c\nA=1\nexport ARGUS_BUILDER_TOKEN_X='sec ret'\nARGUS_BUILDER_TOKEN_Y=\n"), 0o600)
	if v, err := ReadToken(p, "ARGUS_BUILDER_TOKEN_X"); err != nil || v != "sec ret" {
		t.Errorf("ReadToken X = %q, %v", v, err)
	}
	if v, err := ReadToken(p, "ARGUS_BUILDER_TOKEN_Y"); err != nil || v != "" {
		t.Errorf("ReadToken Y = %q, %v", v, err)
	}
	if set, err := TokenSet(p, "ARGUS_BUILDER_TOKEN_X"); err != nil || !set {
		t.Errorf("TokenSet X = %v, %v", set, err)
	}
}

func TestJudgeBuilderTools(t *testing.T) {
	if err := JudgeBuilderTools([]string{"runner__run", "runner__get_report", "runner__validate_config"}); err != nil {
		t.Errorf("runner-only list refused: %v", err)
	}
	err := JudgeBuilderTools([]string{"runner__run", "author_write_scenario", "author_get_reveal"})
	if err == nil {
		t.Fatal("a builder token that sees author tools was accepted")
	}
	if !strings.Contains(err.Error(), "author_write_scenario") || !strings.Contains(err.Error(), "author scope") {
		t.Errorf("refusal must name the offending tool and say author scope: %v", err)
	}
	if err := JudgeBuilderTools(nil); err == nil {
		t.Error("an empty tool list was accepted")
	}
	if err := JudgeBuilderTools([]string{"runner__run", "router_list_tools"}); err == nil {
		t.Error("a non-runner__ tool was accepted")
	}
}

func TestCountAuthorTools(t *testing.T) {
	if n := CountAuthorTools([]string{"author_a", "author_b", "runner__run", "other"}); n != 2 {
		t.Errorf("CountAuthorTools = %d", n)
	}
}
