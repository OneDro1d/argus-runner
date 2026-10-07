package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dispatchedCommands reads main.go and returns every command string the binary dispatches: the
// pre-auth `if cmd == "x"` sites, and the `case "x":` arms of the one post-auth switch.
//
// ⚠️ It reads the SOURCE rather than trusting a second hand-kept list, because a hand-kept list is
// exactly what this test exists to catch. Comments are stripped first: a `cmd == "x"` inside a
// comment is prose, not a dispatch.
//
// ⛔ WHAT THIS CANNOT CATCH, stated because a test built out of the thing it checks will otherwise
// be trusted further than it deserves: it knows the two dispatch FORMS that exist today — the
// `if cmd == "x"` chain and the one `switch cmd` — and a third form (a handler map, a prefix match,
// a dispatch in another file) would be invisible to it AND to whoever compiled the list from the
// same grep. Checked by hand on 2026-09-23: `cmd` is used nowhere else in main.go for dispatch, and
// main.go is the only file that dispatches. **If you add a new dispatch shape, teach this test
// about it in the same edit** — otherwise it will keep passing and mean less than it did.
func dispatchedCommands(t *testing.T) map[string]bool {
	t.Helper()
	blob, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code strings.Builder
	for _, line := range strings.Split(string(blob), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	src := code.String()

	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`cmd == "([a-z0-9-]+)"`).FindAllStringSubmatch(src, -1) {
		got[m[1]] = true
	}

	// The post-auth switch: everything between the `switch cmd {` nearest above the unknown-command
	// default and that default itself.
	end := strings.Index(src, `emitErr(exitUsage, "unknown command %q", cmd)`)
	if end < 0 {
		t.Fatal(`main.go no longer contains the unknown-command default — this test cannot find the dispatch switch`)
	}
	start := strings.LastIndex(src[:end], "switch cmd {")
	if start < 0 {
		t.Fatal("main.go has an unknown-command default with no `switch cmd {` above it")
	}
	for _, m := range regexp.MustCompile(`case ((?:"[a-z0-9-]+"(?:, )?)+):`).FindAllStringSubmatch(src[start:end], -1) {
		for _, lit := range strings.Split(m[1], ", ") {
			got[strings.Trim(lit, `"`)] = true
		}
	}
	if len(got) < 20 {
		t.Fatalf("only %d dispatched commands found — the extraction is broken, not the list", len(got))
	}
	return got
}

// TestUsageListsEveryDispatchedCommand is the guard on `argus --help`. It fails in BOTH directions:
// a command that dispatches but is not listed (the 2026-09-23 defect — `preflight` and fifteen
// `cloud-*` commands were missing while docs/DEPLOY-ARGUS.md told operators to run them), and a
// command listed that no longer dispatches (help that promises what the binary refuses).
func TestUsageListsEveryDispatchedCommand(t *testing.T) {
	dispatched := dispatchedCommands(t)
	listed := map[string]bool{}
	for _, c := range usageCommands {
		if listed[c] {
			t.Errorf("%q is listed twice in usageCommands", c)
		}
		listed[c] = true
	}

	var missing, phantom []string
	for c := range dispatched {
		if !listed[c] {
			missing = append(missing, c)
		}
	}
	for c := range listed {
		if !dispatched[c] {
			phantom = append(phantom, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	if len(missing) > 0 {
		t.Errorf("`argus --help` does not list %d command(s) the binary dispatches: %v\n"+
			"An agent that reads --help concludes they do not exist — and cannot tell an old binary from a typo.", len(missing), missing)
	}
	if len(phantom) > 0 {
		t.Errorf("`argus --help` lists %d command(s) the binary does not dispatch: %v", len(phantom), phantom)
	}
}

// TestUsageCommandsAreSorted keeps the list reviewable: an unsorted list makes an addition look like
// a rewrite in a diff, which is how the last curation went unnoticed.
func TestUsageCommandsAreSorted(t *testing.T) {
	if !sort.StringsAreSorted(usageCommands) {
		t.Error("usageCommands is not sorted")
	}
}
