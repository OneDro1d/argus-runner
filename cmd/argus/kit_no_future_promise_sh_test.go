package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// V29-005 / VR11-T1 — A SHIPPED SCRIPT NEVER PROMISES A FUTURE BUILD.
//
// The step-9/9 PARTIALLY SET banner in onboard.sh — the one message built to catch a regression of
// the router's state reload (V29-001) — told the operator "the fix ships in the next execution-plane
// image". True on the FIRST 0.3.29 build, false from the FINAL one on: from then the banner could only
// mean the reload REGRESSED, and the sentence invited the operator to wait instead of reporting it.
// The text was fixed on the branch (f41b61c); this guard stops the next such promise. They are added
// in good faith during every build, about the next version, and the promise outlives the build that
// made it true.
//
// READ, NOT RUN (the pattern of demo_shared_project_sh_test.go:72-78): the four shipped scripts and
// every lib/*.sh are read and scanned by line. Scope is EXACTLY those — never docs/ or the findings
// register, where a row may legitimately say a fix is coming.

// kitFuturePromiseRe is the row's regex, verbatim.
var kitFuturePromiseRe = regexp.MustCompile(`(?i)(the fix ships|will be fixed in|ships in the next|in a (later|future) (build|image|release))`)

// kitFuturePromiseScripts are the shipped scripts the guard reads, relative to the kit's onboarding
// directory; lib/*.sh is added by glob.
var kitFuturePromiseScripts = []string{"onboard.sh", "teardown.sh", "update.sh", "test-orderservice.sh"}

type kitFuturePromise struct {
	file   string // relative to the scanned directory, slash-separated
	line   int    // 1-based
	phrase string // the matched text, as written in the script
	text   string // the whole line, trimmed
}

// kitFuturePromisesIn scans ONE onboarding directory (the shipped kit, or a scratch copy of it) and
// returns every future-build promise it finds plus the list of files it read. A missing script is an
// error — the guard must never pass by scanning nothing.
func kitFuturePromisesIn(dir string) (found []kitFuturePromise, scanned []string, err error) {
	files := make([]string, 0, len(kitFuturePromiseScripts)+16)
	for _, s := range kitFuturePromiseScripts {
		files = append(files, filepath.Join(dir, s))
	}
	libs, err := filepath.Glob(filepath.Join(dir, "lib", "*.sh"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(libs)
	files = append(files, libs...)
	for _, f := range files {
		blob, err := os.ReadFile(f)
		if err != nil {
			return nil, scanned, err
		}
		rel, _ := filepath.Rel(dir, f)
		rel = filepath.ToSlash(rel)
		scanned = append(scanned, rel)
		for i, line := range strings.Split(string(blob), "\n") {
			if m := kitFuturePromiseRe.FindString(line); m != "" {
				found = append(found, kitFuturePromise{file: rel, line: i + 1, phrase: m, text: strings.TrimSpace(line)})
			}
		}
	}
	return found, scanned, nil
}

// guardKitAgainstFuturePromises runs the guard over one onboarding directory and reports every hit
// the row's way: file, line, the matched phrase, and why it is refused.
func guardKitAgainstFuturePromises(t *testing.T, dir string) (scanned []string) {
	t.Helper()
	found, scanned, err := kitFuturePromisesIn(dir)
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	for _, p := range found {
		t.Errorf("%s:%d promises a future build — matched %q:\n  %s\n"+
			"  a shipped script must describe what the operator should DO now, never promise a future build — "+
			"the promise outlives the build that made it true.", p.file, p.line, p.phrase, p.text)
	}
	return scanned
}
