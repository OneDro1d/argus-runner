package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// V29-001 — THE ASSEMBLY OF THE RELOAD, WHICH NOTHING TESTED AT ALL.
//
// ── WHAT SHIPPED ─────────────────────────────────────────────────────────────
//
// 0.3.29's `router serve` never called router.WatchState. The mechanism (internal/router/reload.go) was intact
// and its five tests were green; the CALL had been dropped by the V27-009 merge (commit 09722d4) when the serve
// path was rewritten around runRecordHeartbeats. Measured on the first throwaway onboard after the router had
// already moved to 0.3.29: both freshly wired folders were "unrecognized router token" and the test folder's
// author tools had "no control plane configured" — until `docker restart argus-router`.
//
// ── WHY THIS IS AN AST WALK AND NOT A STRING MATCH ────────────────────────────
//
// A grep for "router.WatchState(" is satisfied by a comment, and by a call in a function that never runs. This
// test finds the serve path's function, walks ITS body, and asserts the call is there, is a `go` statement (a
// synchronous call would block serve before it ever binds), and is wired to the live table and the state dir it
// is serving — not to a copy. Positive control: red on the 0.3.29 tag (1b945a4), green with the block restored.
//
// A behavioural test (start serve, wire a folder, initialize with its token) would be stronger still; it is not
// written because the harness cannot run a freshly built binary under Smart App Control on the build machine,
// and internal/router/reload_test.go already proves the mechanism end to end.
func TestRouterServe_ReloadsItsStateWhileServing(t *testing.T) {
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

	var found []*ast.CallExpr
	var asGo []*ast.CallExpr
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			if isWatchState(x.Call) {
				asGo = append(asGo, x.Call)
			}
		case *ast.CallExpr:
			if isWatchState(x) {
				found = append(found, x)
			}
		}
		return true
	})
	if len(found) == 0 {
		t.Fatal("`router serve` never calls router.WatchState: every folder wired after the router process started " +
			"stays \"unrecognized router token\" until the router is restarted, and the onboarding banner still says " +
			"ALL SET (V29-001). Restore the `go router.WatchState(*stateDir, tbl, router.ReloadInterval, nil, ...)` " +
			"block before router.BindAddr.")
	}
	if len(asGo) == 0 {
		t.Fatalf("router.WatchState is called at %s but NOT as a `go` statement — it loops forever, so serve would "+
			"never reach ListenAndServe", fset.Position(found[0].Pos()))
	}
	call := asGo[0]
	// AC-15 added `ready` (arg 4, between stop and onChange) so a caller — namely
	// internal/router/reload_test.go — can wait for WatchState's baseline stat instead of racing it
	// with a fixed sleep. `router serve` has no such caller to synchronize with, so it passes nil.
	if len(call.Args) != 7 {
		t.Fatalf("router.WatchState called with %d args at %s, want 7 (dir, table, interval, stop, ready, onChange, onErr)",
			len(call.Args), fset.Position(call.Pos()))
	}
	// arg 0: *stateDir — the flag the table was loaded from, dereferenced
	if star, ok := call.Args[0].(*ast.StarExpr); !ok || identName(star.X) != "stateDir" {
		t.Errorf("arg 0 is %s, want *stateDir — the watcher must stat the SAME state file serve loaded", exprString(call.Args[0]))
	}
	// arg 1: tbl — the live table the authenticator and the tools hold; a fresh table would reload into nothing
	if identName(call.Args[1]) != "tbl" {
		t.Errorf("arg 1 is %s, want tbl — the table router.Authenticator and router.Tools were given", exprString(call.Args[1]))
	}
	// arg 2: router.ReloadInterval — the declared cadence, not a literal someone can quietly set to an hour
	if sel, ok := call.Args[2].(*ast.SelectorExpr); !ok || identName(sel.X) != "router" || sel.Sel.Name != "ReloadInterval" {
		t.Errorf("arg 2 is %s, want router.ReloadInterval", exprString(call.Args[2]))
	}
	// arg 4: ready — nil here; `router serve` has nothing to synchronize its startup with
	if identName(call.Args[4]) != "nil" {
		t.Errorf("arg 4 is %s, want nil — router serve has no caller waiting on WatchState's baseline stat", exprString(call.Args[4]))
	}
	// args 5, 6: both callbacks present. onErr in particular: a reload that keeps failing must keep saying so.
	for i := 5; i <= 6; i++ {
		if _, ok := call.Args[i].(*ast.FuncLit); !ok {
			t.Errorf("arg %d is %s, want a func literal — %s must be wired, or a refused reload is silent",
				i, exprString(call.Args[i]), map[int]string{5: "onChange", 6: "onErr"}[i])
		}
	}
}

func isWatchState(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	return ok && identName(sel.X) == "router" && sel.Sel.Name == "WatchState"
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.FuncLit:
		return "func literal"
	case *ast.BasicLit:
		return x.Value
	}
	return "<expr>"
}
