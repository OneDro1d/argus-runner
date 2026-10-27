package runner

// ownwords_sites_test.go — AC-F1: the rule only holds if every place that fills either field goes through it. This
// reads the repo's own Go source (go/parser, not a text search) and fails on any non-test place outside the control
// plane that sets a failure reason by anything but failureReason(err), or builds a command answer's error by anything
// but commandErrorText(err) — the argument being the error itself, a plain name.
//
// What it reads, and what it cannot: a `FailureReason:` key in ANY composite literal and any `.FailureReason =`
// assignment (the name alone is enough — only the federation wire has that field outside the control plane); a
// `federation.CommandResult{…}` literal under whatever name the federation package is imported, and the elided
// elements of a slice, array or map of it. It does NOT see `x.Error = …` after a literal: "Error" is a field of other
// types too (the summary readings, which follow the same rule since AC-F1.1 — summaryErrorTexts, pinned by
// TestSummaryReadingError_TheReadingsErrorIsSetOnlyThroughTheRule), and telling them apart needs type information this
// guard does not load. The marker test and TestCommandResult_SendsOnlyTheWords stand behind that form.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const federationImportPath = "github.com/OneDro1d/argus-runner/internal/federation"

// ruleSites lists, for one parsed file, every place it fills either field ("file:line") and every one that does not go
// through the rule.
func ruleSites(fset *token.FileSet, f *ast.File, rel string) (reasons, errs, bad []string) {
	// The rule's call, AND its argument is the error itself — a plain name. A call in that place (failureReason(
	// ownWordsf("%w", err)), failureReason(errors.New(localDetail(err)))) could vouch for anything, so it is refused too.
	isRule := func(e ast.Expr, name string) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok || len(c.Args) != 1 {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		if !ok || id.Name != name {
			return false
		}
		_, plain := c.Args[0].(*ast.Ident)
		return plain
	}
	at := func(n ast.Node) string { return rel + ":" + strconv.Itoa(fset.Position(n.Pos()).Line) }
	fedName := ""
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == federationImportPath {
			fedName = "federation"
			if im.Name != nil {
				fedName = im.Name.Name
			}
		}
	}
	isCommandResult := func(t ast.Expr) bool {
		s, ok := t.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "CommandResult" {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && fedName != "" && x.Name == fedName
	}
	checkCommandResult := func(lit *ast.CompositeLit) {
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Error" {
					errs = append(errs, at(kv))
					if !isRule(kv.Value, "commandErrorText") {
						bad = append(bad, at(kv)+" builds a command answer's error without the rule — use commandErrorText(err)")
					}
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if k, ok := n.Key.(*ast.Ident); ok && k.Name == "FailureReason" {
				reasons = append(reasons, at(n))
				if !isRule(n.Value, "failureReason") {
					bad = append(bad, at(n)+" sets a failure reason without the rule — use failureReason(err)")
				}
			}
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "FailureReason" && i < len(n.Rhs) {
					reasons = append(reasons, at(n))
					if !isRule(n.Rhs[i], "failureReason") {
						bad = append(bad, at(n)+" sets a failure reason without the rule — use failureReason(err)")
					}
				}
			}
		case *ast.CompositeLit:
			if isCommandResult(n.Type) {
				checkCommandResult(n)
			}
			var elt ast.Expr // a slice, array or map OF CommandResult: its elements' type is elided
			switch t := n.Type.(type) {
			case *ast.ArrayType:
				elt = t.Elt
			case *ast.MapType:
				elt = t.Value
			}
			if elt != nil && isCommandResult(elt) {
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						el = kv.Value
					}
					if lit, ok := el.(*ast.CompositeLit); ok && lit.Type == nil {
						checkCommandResult(lit)
					}
				}
			}
		}
		return true
	})
	return reasons, errs, bad
}

func TestOwnWords_EveryPlaceThatFillsEitherFieldGoesThroughTheRule(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var reasonSites, errorSites []string
	fset := token.NewFileSet()
	werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel == "internal/control", strings.HasPrefix(rel, "internal/control/"): // the control plane: out of scope
				return filepath.SkipDir
			case d.Name() == "testdata", d.Name() == "node_modules", d.Name() == "vendor", strings.HasPrefix(d.Name(), "."):
				if rel != "." {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("%s does not parse: %v", rel, perr)
			return nil
		}
		reasons, errs, bad := ruleSites(fset, f, rel)
		reasonSites, errorSites = append(reasonSites, reasons...), append(errorSites, errs...)
		for _, b := range bad {
			t.Error(b)
		}
		return nil
	})
	if werr != nil {
		t.Fatal(werr)
	}
	// Positive control: the guard really reads the places it guards — the two run-failure sites and the command
	// answer.
	for _, want := range []string{"internal/runner/executor.go", "internal/runner/direct.go"} {
		if !containsPrefix(reasonSites, want+":") {
			t.Errorf("the guard found no failure-reason site in %s (found %v) — it is not reading what it guards", want, reasonSites)
		}
	}
	if !containsPrefix(errorSites, "internal/runner/client.go:") {
		t.Errorf("the guard found no command-answer error in client.go (found %v)", errorSites)
	}
	t.Logf("failure-reason sites: %v; command-error sites: %v", reasonSites, errorSites)
}

// The guard itself, both ways: every leaking form it claims to read is caught, and the rule's own forms pass.
func TestOwnWords_TheSitesGuardCatchesEveryFormItReads(t *testing.T) {
	const probe = `package probe

import (
	fed "github.com/OneDro1d/argus-runner/internal/federation"
)

func a(err error) fed.ResultsPush   { return fed.ResultsPush{FailureReason: err.Error()} }
func b(err error) fed.ResultsPush   { return fed.ResultsPush{FailureReason: failureReason(ownWordsf("%w", err))} }
func c(err error, p *fed.ResultsPush) { p.FailureReason = err.Error() }
func d(err error) fed.CommandResult { return fed.CommandResult{Error: err.Error()} }
func e(err error) []fed.CommandResult { return []fed.CommandResult{{Error: err.Error()}} }
func f(err error) map[string]fed.CommandResult { return map[string]fed.CommandResult{"x": {Error: err.Error()}} }
func g(err error) fed.CommandResult { return fed.CommandResult{Error: commandErrorText(err)} }
func h(err error) fed.ResultsPush   { return fed.ResultsPush{FailureReason: failureReason(err)} }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probe.go", probe, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	reasons, errs, bad := ruleSites(fset, f, "probe.go")
	want := map[string]bool{"probe.go:7": true, "probe.go:8": true, "probe.go:9": true, "probe.go:10": true, "probe.go:11": true, "probe.go:12": true}
	got := map[string]bool{}
	for _, b := range bad {
		got[strings.SplitN(b, " ", 2)[0]] = true
	}
	for line := range want {
		if !got[line] {
			t.Errorf("the guard missed the leaking form at %s (it flagged %v)", line, bad)
		}
	}
	for line := range got {
		if !want[line] {
			t.Errorf("the guard flagged the rule's own form at %s", line)
		}
	}
	if len(reasons) != 4 || len(errs) != 4 {
		t.Errorf("the guard read %d failure-reason and %d command-error sites in the probe, want 4 and 4", len(reasons), len(errs))
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
