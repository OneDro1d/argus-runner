package reporoute

import (
	"path/filepath"
	"regexp"
	"strings"
)

// testDirNames are directory names whose entire contents are test/fixture sources, never the SUT's
// own routes — spec item 1 of the review.
var testDirNames = map[string]bool{
	"__tests__": true, "test": true, "tests": true, "testdata": true,
	"fixtures": true, "__mocks__": true, "e2e": true,
}

// testFileNameRe matches `*.test.*` / `*.spec.*` (e.g. idempotency.test.ts, foo.spec.js) — spec item
// 1: the exact shape that produced fake routes out of
// packages/website-api/src/middleware/idempotency.test.ts, a file NOT under any test directory.
var testFileNameRe = regexp.MustCompile(`\.(test|spec)\.[^./]+$`)

// isTestSourceFile reports whether rel (repo-relative, slash-separated) is a test source that must
// never be scanned for routes — spec item 1: "*.test.*, *.spec.*, *_test.go" and directories named
// "__tests__, test, tests, testdata, fixtures, __mocks__, e2e".
func isTestSourceFile(rel string) bool {
	base := filepath.Base(rel)
	if testFileNameRe.MatchString(base) {
		return true
	}
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if testDirNames[seg] {
			return true
		}
	}
	return false
}
