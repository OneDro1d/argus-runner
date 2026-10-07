package main

// (a), the executor half. `serve --mode runner` builds the federated executor's
// runner.ExecConfig inline, and the relayed validate_config (internal/runner/relay.go) validates
// through THAT config's environment, not through env(cf). The line that copies ARGUS_OBS_MODE into it
// is inside a 200-line serve function that cannot be run without a control plane, so it is checked
// the way it can be: parse main.go, find the runner.ExecConfig literal, and require its ObsMode
// field to be read from ARGUS_OBS_MODE. (The behaviour behind the field is proven in
// internal/runner/relay_obs_none_test.go; the real-binary run is in validate_config_obs_none_test.go.)

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestServeRunner_ExecConfigCarriesARGUS_OBS_MODE(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, wired := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ExecConfig" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "runner" {
			return true
		}
		found++
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "ObsMode" {
				continue
			}
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				continue
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fn.Sel.Name != "Getenv" {
				continue
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok {
				if s, _ := strconv.Unquote(lit.Value); s == "ARGUS_OBS_MODE" {
					wired++
				}
			}
		}
		return true
	})
	if found == 0 {
		t.Fatal("no runner.ExecConfig literal found in main.go: this test must be updated with the code, not skipped")
	}
	if wired != found {
		t.Errorf("%d runner.ExecConfig literal(s) in main.go, %d set ObsMode from os.Getenv(\"ARGUS_OBS_MODE\"): the executor's own validate_config would refuse an --obs none instance's config for a missing Grafana URL", found, wired)
	}
}
