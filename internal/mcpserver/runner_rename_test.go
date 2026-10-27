package mcpserver

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// AC-D70 (design #678 B2, B5): the executor's runner tools have ONE underscore, the old names still dispatch for one
// release without being listed, and the executor reports its real version.

var runnerToolNames = []string{"runner_validate_config", "runner_run", "runner_get_report", "runner_get_sagas",
	"runner_get_tail_logs", "runner_get_dashboard_url"}

func listedNames(t *testing.T, s *Server, sid, tok string) []string {
	t.Helper()
	raw, _ := s.Dispatch(sid, tok, reqBytes(9, "tools/list", map[string]any{}))
	var r struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("tools/list: %s", raw)
	}
	var out []string
	for _, tl := range r.Result.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestRunnerRename_TheExecutorListsOneUnderscoreNamesOnly(t *testing.T) {
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(toolcore.Env{Instance: "local"})...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{rtok, atok} {
		handshake(t, s, "s"+tok, tok)
		if got := strings.Join(listedNames(t, s, "s"+tok, tok), ","); got != strings.Join(runnerToolNames, ",") {
			t.Fatalf("tools/list = %q, want %q", got, strings.Join(runnerToolNames, ","))
		}
	}
}

// The old name reaches the SAME tool — through the same closed-schema check, the same pre-check and, for the run tool,
// the same async path — so an old router or an agent wired straight to the executor keeps working.
func TestRunnerRename_TheOldNamesStillDispatchToTheSameTool(t *testing.T) {
	env := writeFixtureReport(t)
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", atok)
	call := func(name string, args map[string]any) string {
		raw, _ := s.Dispatch("s", atok, reqBytes(1, "tools/call", map[string]any{"name": name, "arguments": args}))
		return string(raw)
	}
	for _, n := range runnerToolNames {
		old := strings.Replace(n, "runner_", "runner__", 1)
		// No instance_id: both names are refused by the same check with the same text.
		if a, b := call(n, map[string]any{}), call(old, map[string]any{}); a != b || !strings.Contains(a, "instance_id is required") {
			t.Fatalf("%s vs %s without instance_id:\n%s\n%s", n, old, a, b)
		}
		// An undeclared argument: refused by the closed schema for the old name too (it names what was called).
		if got := call(old, map[string]any{"instance_id": "local", "junk": "x"}); !strings.Contains(got, "junk") || strings.Contains(got, `"code":-32601`) {
			t.Fatalf("%s with an undeclared argument: %s", old, got)
		}
	}
	// A real answer through the old name: the report, with the test hat's expected value.
	if a, b := call("runner_get_report", map[string]any{"instance_id": "local"}), call("runner__get_report", map[string]any{"instance_id": "local"}); a != b || !strings.Contains(a, sentinelExpected) {
		t.Fatalf("get_report by the two names:\n%s\n%s", a, b)
	}
}

func TestServerInfo_VersionIsTheServersOwnAndM25WhenUnset(t *testing.T) {
	version := func(s *Server) string {
		raw, _ := s.Dispatch("v", atok, reqBytes(1, "initialize", map[string]any{}))
		var r struct {
			Result struct {
				ServerInfo struct{ Name, Version string } `json:"serverInfo"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.Result.ServerInfo.Name != "argus" {
			t.Fatalf("initialize: %s", raw)
		}
		return r.Result.ServerInfo.Version
	}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(toolcore.Env{Instance: "local"})...)
	if err != nil {
		t.Fatal(err)
	}
	if got := version(s); got != "M2.5" {
		t.Fatalf("a server whose Version is unset reports %q, want the old \"M2.5\"", got)
	}
	s.Version = "0.3.73-test"
	if got := version(s); got != "0.3.73-test" {
		t.Fatalf("serverInfo.version = %q, want the server's own %q", got, "0.3.73-test")
	}
}

// B5's done criterion: no text the executor's tools or toolcore give an agent names a two-underscore tool. The tools are
// walked as built (names, descriptions, every description in a schema), and every string literal in the non-test Go
// files of this package and of internal/toolcore is scanned — so a text added later is caught too. The one place the old
// spelling may be written is the alias helper.
func TestRunnerRename_NoTextTheExecutorGivesNamesAnOldTool(t *testing.T) {
	bad := func(s string) bool { return strings.Contains(s, "runner__") || strings.Contains(s, "author__") }
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []string:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		case string:
			if bad(x) {
				t.Errorf("%s names an old tool: %q", path, x)
			}
		}
	}
	for _, tl := range DefaultTools(toolcore.Env{Instance: "local"}) {
		walk(tl.Name+".name", tl.Name)
		walk(tl.Name+".description", tl.Description)
		walk(tl.Name+".inputSchema", tl.InputSchema)
		if len(tl.Aliases) != 1 || tl.Aliases[0] != strings.Replace(tl.Name, "runner_", "runner__", 1) {
			t.Errorf("%s: aliases %v, want exactly its old spelling", tl.Name, tl.Aliases)
		}
	}

	scanned := 0
	for _, dir := range []string{".", filepath.Join("..", "toolcore")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, f, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == "legacyRunnerName" {
					return false // the alias helper: the one place the old spelling is written
				}
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && bad(lit.Value) {
					t.Errorf("%s: a string literal names an old tool: %s", fset.Position(lit.Pos()), lit.Value)
				}
				return true
			})
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d files — the scan is not looking where the texts are", scanned)
	}
}
