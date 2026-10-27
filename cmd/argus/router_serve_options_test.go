package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/router"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// AC-D69/AC-D70: `router serve` builds the router's server from routerServeWiring. Both planes — the control plane's
// author tools and each executor's runner tools — are read and called with the one host alias; without it, a
// containerised router reads its upstreams from its own loopback and every session starts with no tools — a mistake no
// router package test can see, because those tests build their own Options.

func TestRouterServeWiring_BothPlanesGetTheAliasTheTimeoutsAndTheVersion(t *testing.T) {
	t.Setenv(router.HostAliasEnv, "host.docker.internal")
	t.Setenv(router.ListTimeoutEnv, "7s")
	t.Setenv(router.AuthorCallTimeoutEnv, "4m")
	t.Setenv(router.RunnerCallTimeoutEnv, "3m")
	t.Setenv("ARGUS_VERSION", "9.8.7-test")
	var warns []string
	opts := routerServeWiring(func(w string) { warns = append(warns, w) })
	if opts.HostAlias != "host.docker.internal" {
		t.Fatalf("alias %q — want host.docker.internal", opts.HostAlias)
	}
	if opts.ListTimeout != 7*time.Second || opts.CallTimeout != 4*time.Minute || opts.RunnerCallTimeout != 3*time.Minute {
		t.Fatalf("list timeout %s, author call timeout %s, runner call timeout %s — want 7s, 4m and 3m from the environment",
			opts.ListTimeout, opts.CallTimeout, opts.RunnerCallTimeout)
	}
	if opts.Version != "9.8.7-test" {
		t.Fatalf("the router's version %q — want the build version, %q", opts.Version, "9.8.7-test")
	}
	if len(warns) != 0 {
		t.Fatalf("valid settings warned: %v", warns)
	}
	if opts.Logf == nil {
		t.Fatal("the server's own warnings go nowhere")
	}
	opts.Logf("router: %s", "a warning")
	if len(warns) != 1 || warns[0] != "router: a warning" {
		t.Fatalf("the server's warning was not passed on: %v", warns)
	}

	t.Setenv(router.ListTimeoutEnv, "soon")
	t.Setenv(router.AuthorCallTimeoutEnv, "1s")
	t.Setenv(router.RunnerCallTimeoutEnv, "9h")
	warns = nil
	opts = routerServeWiring(func(w string) { warns = append(warns, w) })
	if len(warns) != 3 || !strings.Contains(warns[0], router.ListTimeoutEnv) || !strings.Contains(warns[1], router.AuthorCallTimeoutEnv) ||
		!strings.Contains(warns[2], router.RunnerCallTimeoutEnv) {
		t.Fatalf("mistyped settings must each warn: %v", warns)
	}
	if opts.ListTimeout != router.DefaultListTimeout || opts.CallTimeout != router.MinAuthorCallTimeout || opts.RunnerCallTimeout != router.MaxRunnerCallTimeout {
		t.Fatalf("mistyped settings gave %s, %s and %s", opts.ListTimeout, opts.CallTimeout, opts.RunnerCallTimeout)
	}
}

// serverInfoVersion reads serverInfo.version from a real initialize.
func serverInfoVersion(t *testing.T, srv *mcpserver.Server, tok string) string {
	t.Helper()
	raw, _ := srv.Dispatch("v", tok, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	var r struct {
		Result struct {
			ServerInfo struct{ Version string } `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("initialize: %s", raw)
	}
	return r.Result.ServerInfo.Version
}

// AC-D70 (design #678 B2, A9 #4): the executor reports its real version — the build version, read where it already lives
// (buildinfo, the value it sends the control plane as RunnerVersion) — through the ONE constructor both executor entry
// points use, and the router reports its own. The control plane is not built here (its server needs the database); it is
// left as it was, "M2.5": a server with no Version answers that (internal/mcpserver/hooks_unset_ext_test.go), and
// internal/control's TestControlPlane_ItsMCPServerGetsNoBuildVersion pins that its constructor sets none.
func TestExecutorAndRouter_ReportTheirBuildVersion(t *testing.T) {
	t.Setenv("ARGUS_VERSION", "9.8.7-test")
	t.Setenv("ARGUS_CP_URL", "") // no control-plane fence wiring in this test
	ex, err := newExecutorMCPServer(auth.Config{RunnerToken: "r", AuthorToken: "a"}, toolcore.Env{Instance: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got := serverInfoVersion(t, ex, "r"); got != "9.8.7-test" {
		t.Fatalf("the executor reports %q, want the build version %q", got, "9.8.7-test")
	}
	rs, err := router.NewServer(router.New(), routerServeWiring(func(string) {}))
	if err != nil {
		t.Fatal(err)
	}
	ex2, _ := newExecutorMCPServer(auth.Config{RunnerToken: "r", AuthorToken: "a"}, toolcore.Env{Instance: "local"})
	if rs.Version != "9.8.7-test" || ex2.Version != rs.Version {
		t.Fatalf("the router's server reports %q", rs.Version)
	}
}

// Both executor entry points build their server through newExecutorMCPServer — a site that built its own would report
// the old fixed "M2.5" and the router would rank that executor below every real one.
func TestExecutorEntryPoints_UseTheOneConstructor(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	users := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch {
			case identName(c.Fun) == "newExecutorMCPServer":
				users[fd.Name.Name] = true
			case fd.Name.Name != "newExecutorMCPServer" && isSelector(c.Fun, "mcpserver", "DefaultTools"):
				t.Errorf("%s builds the executor's tools itself at %s — use newExecutorMCPServer", fd.Name.Name, fset.Position(c.Pos()))
			}
			return true
		})
		return false
	})
	for _, want := range []string{"cmdServe", "cmdServeMode"} {
		if !users[want] {
			t.Errorf("%s does not build its executor server through newExecutorMCPServer (users: %v)", want, users)
		}
	}
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && identName(sel.X) == pkg && sel.Sel.Name == name
}

// cmdRouter must build the server from routerServeWiring's options, not from Options of its own.
func TestRouterServe_BuildsItsServerFromRouterServeWiring(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var serve *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "cmdRouter" {
			serve = fd
		}
	}
	if serve == nil {
		t.Fatal("cmdRouter — the function that implements `router serve` — is not in main.go; if it moved, point this test at it")
	}
	var wired []string // the names routerServeWiring's result is assigned to
	var newServer []*ast.CallExpr
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Rhs) == 1 {
				if c, ok := x.Rhs[0].(*ast.CallExpr); ok && identName(c.Fun) == "routerServeWiring" {
					for _, l := range x.Lhs {
						wired = append(wired, identName(l))
					}
				}
			}
		case *ast.CallExpr:
			if isSelector(x.Fun, "router", "NewServer") {
				newServer = append(newServer, x)
			}
		}
		return true
	})
	if len(wired) != 1 || len(newServer) != 1 {
		t.Fatalf("cmdRouter: routerServeWiring assigned to %v, router.NewServer called %d times — want one result and one call", wired, len(newServer))
	}
	call := newServer[0]
	if len(call.Args) != 2 || identName(call.Args[1]) != wired[0] {
		t.Fatalf("router.NewServer at %s is not given routerServeWiring's options (%s)", fset.Position(call.Pos()), wired[0])
	}
}
