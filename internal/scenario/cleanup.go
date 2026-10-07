package scenario

import (
	"regexp"
	"strings"
)

// VR12-C1 / VR12-C2 (V29-015) — `## CLEANUP` IS A CONTRACT, AND IT IS EXECUTED.
//
// `Scenario.Cleanup{SQL,Bash}` was parsed and READ BY NOTHING: the only references in the whole
// non-test tree were the two parser lines that wrote it, and `grep -rni "cleanup" internal/argus/`
// returned nothing — no defer, no teardown hook, on any path. Meanwhile FOUR documents told authors
// the section was required and enforced, and every operator was shipped a `cleanup_enabled: true`
// switch that no code read.
//
// Audited across all 122 example files: 58 create data that outlives the run, and 32 are confirmed
// to create data on a real system that nothing anywhere deletes. Twenty of those are chain
// scenarios — theirs is VR12-CH1's fix, because a chain can express cleanup as a step. TWELVE are
// NOT chains (DB-004/005/006, ORDE-007/013/015/017/018/019, EXT-001, MSGF-003, ORDE-008), and a
// non-chain scenario has no step list, so for those twelve `## CLEANUP` is the ONLY channel there
// is.
//
// ⛔ WHERE IT CAME FROM. This is not our bug alone: it is inherited from example/argus, the
// project Argus was lifted from (no longer vendored here as a submodule — see ORIGIN.md). Its
// `scripts/run.sh:31` parsed `--no-cleanup` into `NO_CLEANUP=true` and never read the variable
// again; its `scripts/parse_scenario.py:138-148` extracted cleanup into `sql`/`bash` that nothing
// consumed — character for character the same defect. The Go port did not break this; it
// faithfully reproduced a hole, and inherited the documentation of a pipeline stage that never
// existed. That is why four documents describe a feature nobody ever wrote.

// CleanupForm is what a `## CLEANUP` block declares.
type CleanupForm string

const (
	// CleanupSQL runs against the database the suite already targets (`targets.database`) — the
	// SAME connection a `Database State` VERIFY uses. No new configuration.
	CleanupSQL CleanupForm = "sql"
	// CleanupBash runs inside the executor container, same working directory and environment as
	// the run. Never more privilege than the scenario itself has.
	CleanupBash CleanupForm = "bash"
	// CleanupNA is the honest exemption: nothing runs, and the written justification is the record.
	CleanupNA CleanupForm = "na"
)

// CleanupBlock is ONE declared cleanup block, in file order.
type CleanupBlock struct {
	Form CleanupForm `json:"form"`
	// Body is the SQL / shell to run, or — for CleanupNA — the justification text.
	Body string `json:"body"`
}

// Cleanup is the parsed `## CLEANUP` section.
//
// ⛔ IT IS A LIST, AND ORDER MATTERS. The old parser took the sql block ELSE the bash block —
// first wins, every later block silently discarded — which is precisely VR12-E8's defect wearing a
// different hat. Multiple blocks are legal, they run in the order written, each is reported
// separately, and NONE may be silently dropped.
type Cleanup struct {
	Blocks []CleanupBlock `json:"blocks,omitempty"`
	// SQL and Bash are kept for the callers that read the first block of each kind. ⚠ They are a
	// CONVENIENCE over Blocks, never the source of truth: reading them is how a caller silently
	// drops the second block.
	SQL  string `json:"sql,omitempty"`
	Bash string `json:"bash,omitempty"`
}

// IsNA reports whether this scenario declared the honest exemption.
func (c Cleanup) IsNA() bool {
	return len(c.Blocks) == 1 && c.Blocks[0].Form == CleanupNA
}

// Runnable returns the blocks that actually execute (everything but the N/A exemption).
func (c Cleanup) Runnable() []CleanupBlock {
	var out []CleanupBlock
	for _, b := range c.Blocks {
		if b.Form != CleanupNA {
			out = append(out, b)
		}
	}
	return out
}

var (
	// A fenced block, with its language tag. Walked in order, so a scenario's blocks stay in the
	// order the author wrote them.
	cleanupFenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9_+-]*)[ \t]*\r?\n(.*?)```")
	// `N/A` — the exemption — must be followed by a justification. `N/A` alone is not a reason.
	cleanupNARe = regexp.MustCompile(`(?i)^\s*N/?A\b`)
)

// ParseCleanup reads a `## CLEANUP` body into its declared blocks, in order.
//
// It returns the blocks it UNDERSTOOD plus the fence languages it did not, so the validator can
// refuse the unknown ones BY NAME (VR12-C3) instead of dropping them the way the old parser did.
func ParseCleanup(body string) (Cleanup, []string) {
	var c Cleanup
	var unknown []string
	for _, m := range cleanupFenceRe.FindAllStringSubmatch(body, -1) {
		lang, code := strings.ToLower(strings.TrimSpace(m[1])), strings.TrimSpace(m[2])
		switch lang {
		case "sql":
			c.Blocks = append(c.Blocks, CleanupBlock{Form: CleanupSQL, Body: code})
			if c.SQL == "" {
				c.SQL = code
			}
		case "bash", "sh", "shell":
			c.Blocks = append(c.Blocks, CleanupBlock{Form: CleanupBash, Body: code})
			if c.Bash == "" {
				c.Bash = code
			}
		default:
			// ⛔ NAMED, never dropped. An unrecognised fence used to vanish silently, which is how
			// an author's deletion code could sit in a file for months doing nothing.
			name := lang
			if name == "" {
				name = "(no language tag)"
			}
			unknown = append(unknown, name)
		}
	}
	if len(c.Blocks) == 0 && len(unknown) == 0 {
		// No fence at all. The only other legal shape is the N/A exemption.
		if text := strings.TrimSpace(body); cleanupNARe.MatchString(text) {
			c.Blocks = append(c.Blocks, CleanupBlock{Form: CleanupNA, Body: text})
		}
	}
	return c, unknown
}

// cleanupJustified reports whether an `N/A` carries a justification beyond the letters themselves.
//
// The escape hatch is accepted DELIBERATELY: a syntactic validator cannot tell an honest
// "N/A — this scenario creates nothing" from a lazy one, and deciding "does this scenario create
// data" needed forty independent agents in the row's own audit — it is not decidable from the file
// text. The written justification is the speed bump; abuse is a review matter, not a validator one.
func cleanupJustified(text string) bool {
	rest := cleanupNARe.ReplaceAllString(strings.TrimSpace(text), "")
	rest = strings.TrimLeft(rest, " \t—-–:,.")
	// ⚠ ANY non-empty justification passes — "N/A — read-only." IS a justification, and 23
	// shipped scenarios say exactly that. A longer bar (I first wrote `>= 3` words) generated 23 new
	// authoring errors on files the row's own measurement counts as CORRECT, which breaks this
	// requirement's stated WHAT-MUST-NOT-CHANGE: a proper `N/A — <reason>` keeps validating exactly
	// as today. The validator's job is to refuse a BARE `N/A`, not to grade prose.
	return len(strings.Fields(rest)) >= 1
}
