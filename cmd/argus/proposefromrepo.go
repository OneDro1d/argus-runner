package main

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/reporoute"
)

// cmdProposeFromRepo is T5.1+T5.3 (MVP2-SPRINT.md E5): `argus propose-from-repo --repo <dir> --out
// <dir> [--config <argus-config.yaml>] [--json]`. It reads --repo READ-ONLY, extracts HTTP routes,
// builds a draft scenario per route, gates each through toolcore.ValidateAll and — when --config
// declares money_handling — scenario.MoneyGuardViolations, and writes every accepted draft under
// --out. See internal/reporoute for the extraction/gating logic itself; this is the thin CLI shell.
func cmdProposeFromRepo(cf *commonFlags) int {
	if cf.repo == "" {
		return emitErr(exitUsage, "propose-from-repo: --repo is required (the repository to scan, read-only)")
	}
	if cf.outDir == "" {
		return emitErr(exitUsage, "propose-from-repo: --out is required (the directory accepted scenario drafts are written into)")
	}
	rep, err := reporoute.Run(reporoute.Options{Repo: cf.repo, Out: cf.outDir, Config: cf.configPath})
	if err != nil {
		if err == reporoute.ErrOutInsideRepo {
			return emitErr(exitUsage, "propose-from-repo: %v", err)
		}
		return emitErr(exitErr, "propose-from-repo: %v", err)
	}
	if cf.jsonOut {
		emit(rep)
	} else {
		printProposeFromRepoReport(rep)
	}
	return exitOK
}

func printProposeFromRepoReport(rep *reporoute.Report) {
	fmt.Printf("propose-from-repo %s (money_handling=%v)\n", rep.Repo, rep.MoneyHandling)
	fmt.Printf("  test source files skipped: %d\n", rep.SkippedTestFiles)
	fmt.Printf("  routes found: %d\n", len(rep.Routes))
	for ext, n := range rep.RoutesByExtractor {
		fmt.Printf("    %-12s %d\n", ext, n)
	}
	if len(rep.AmbiguousCalls) > 0 {
		fmt.Printf("  ambiguous calls skipped: %d\n", len(rep.AmbiguousCalls))
		for _, a := range rep.AmbiguousCalls {
			fmt.Printf("    %-6s %-40s %s:%d: %s\n", a.Method, a.Path, a.File, a.Line, a.Reason)
		}
	}
	fmt.Printf("  drafts accepted: %d\n", len(rep.Accepted))
	var needsValue []reporoute.Draft
	for _, d := range rep.Accepted {
		if d.NeedsValue {
			needsValue = append(needsValue, d)
			continue
		}
		note := ""
		if d.Proposed {
			note = "  (proposed — writes to the SUT, needs human review)"
		}
		fmt.Printf("    %-6s %-40s -> %s%s\n", d.Method, d.Path, d.File, note)
	}
	if len(needsValue) > 0 {
		fmt.Printf("  needs a real value before it can run: %d\n", len(needsValue))
		for _, d := range needsValue {
			fmt.Printf("    %-6s %-40s -> %s  (path parameter unfilled; skipped by scenario discovery)\n", d.Method, d.Path, d.File)
		}
	}
	fmt.Printf("  drafts refused: %d\n", len(rep.Refused))
	for _, r := range rep.Refused {
		fmt.Printf("    %-6s %-40s %s: %s\n", r.Method, r.Path, r.Source, r.Reason)
	}
	if len(rep.NotProposed) > 0 {
		fmt.Printf("  not proposed (money_handling): %d\n", len(rep.NotProposed))
		for _, r := range rep.NotProposed {
			fmt.Printf("    %-6s %-40s %s: %s\n", r.Method, r.Path, r.Source, r.Reason)
		}
	}
	if len(rep.UnresolvedPrefixes) > 0 {
		fmt.Printf("  unresolved register() prefixes: %d\n", len(rep.UnresolvedPrefixes))
		for _, u := range rep.UnresolvedPrefixes {
			fmt.Printf("    %s:%d prefix=%q: %s\n", u.File, u.Line, u.Prefix, u.Reason)
		}
	}
	if len(rep.UnparseableFiles) > 0 {
		fmt.Printf("  files that could not be parsed: %d\n", len(rep.UnparseableFiles))
		for _, u := range rep.UnparseableFiles {
			fmt.Printf("    %s: %s\n", u.File, u.Reason)
		}
	}
	if len(rep.SchemaFiles) > 0 {
		fmt.Printf("  schema/migration files found (not turned into scenarios): %s\n", strings.Join(rep.SchemaFiles, ", "))
	}
	if len(rep.DocFiles) > 0 {
		fmt.Printf("  doc files found (input for a human/authoring agent): %s\n", strings.Join(rep.DocFiles, ", "))
	}
}
