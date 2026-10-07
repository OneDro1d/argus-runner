package reporoute

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// walk descends repoAbs read-only (spec item 2: skip node_modules/.git/vendor/dist/build/dot-dirs),
// dispatches each file to the extractor its extension/content implies, and appends routes, schema
// files and doc files onto rep.
func walk(repoAbs string, rep *Report) error {
	return filepath.WalkDir(repoAbs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot even stat is reported, not fatal to the whole walk.
			rep.UnparseableFiles = append(rep.UnparseableFiles, UnparseableFile{File: relTo(repoAbs, p), Reason: err.Error()})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != repoAbs && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel := relTo(repoAbs, p)
		if isTestSourceFile(rel) {
			// spec item 1: test sources are never scanned for routes (or schema/doc files) — the
			// count feeds the summary so a human can sanity-check nothing real was skipped.
			rep.SkippedTestFiles++
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))

		switch ext {
		case ".yaml", ".yml", ".json":
			handleMaybeOpenAPI(p, rel, rep)
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
			extractJSTS(p, rel, rep)
		case ".go":
			extractGo(p, rel, rep)
		case ".sql":
			rep.SchemaFiles = append(rep.SchemaFiles, rel)
		case ".md":
			if isDocFile(name) {
				rep.DocFiles = append(rep.DocFiles, rel)
			}
		}
		if isMigrationDir(rel) {
			rep.SchemaFiles = append(rep.SchemaFiles, rel)
		}
		return nil
	})
}

func relTo(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// docFileRe matches README(.md), ARCHITECTURE*, REQUIREMENTS* at any case (spec item 5).
var docFileRe = regexp.MustCompile(`(?i)^(README|ARCHITECTURE.*|REQUIREMENTS.*)\.md$`)

func isDocFile(base string) bool {
	return docFileRe.MatchString(base)
}

// isMigrationDir flags a path that sits under a conventional migrations/schema directory, so a
// non-.sql migration (e.g. a numbered .js/.ts Knex/Prisma migration) is still listed (spec item 5:
// "DB schema... do NOT generate scenarios from them... list... as input for a human").
var migrationDirRe = regexp.MustCompile(`(?i)(^|/)(migrations?|schema)(/|$)`)

func isMigrationDir(rel string) bool {
	return migrationDirRe.MatchString(rel)
}
