package scenario

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Warnings returns advisory (non-failing) lints for the scenario. FINDING-7/FINDING-9: a
// database-state / message-flow scenario whose EXPECT carries a data assertion the layer
// template cannot execute (timestamp range / JSONB path / cast / an unsupported operator /
// a column-looking bullet that doesn't parse) is warned about, so the author gets a SIGNAL
// instead of a silent fall-back to the corr-id-substring proxy. validate-scenario emits
// these; they do not make a scenario invalid.
func Warnings(text string) []string {
	s := Parse(text)
	var w []string

	// VR12-E3 — a MISSING `## EXPECT` sub-section warns; it never blocks. Owner 2026-09-10:
	// "Just warn if any runnable or non-runnable subsection is missing." The point is to tell the
	// author what they did NOT write: an EXPECT with no `### Runnable` asserts nothing at all
	// (legal — see VR12-E12 — but rarely intended), and one with no `### Non-runnable` records
	// nothing about what the test proves for the next human to read it.
	if s.ExpectSubheaded {
		if len(s.ExpectRunnable) == 0 {
			w = append(w, "## EXPECT has no '### Runnable' sub-section, so this scenario asserts NOTHING that the "+
				"product can check — it is legal, and it is almost never what an author means. If the verdict is "+
				"meant to come from somewhere else (a ui scenario's Playwright exit code), that is fine; "+
				"otherwise move the claim into '### Runnable'")
			// ⚠ It no longer names "a chain's per-step bullets" as the other legitimate home: since
			// VR12-E10 a chain's claims live in '### Runnable' too, as `- step <name>: <assertion>`,
			// and a chain with no runnable bullet is REFUSED by the empty-step guard, not warned.
		}
		if len(s.ExpectNonRunnable) == 0 {
			w = append(w, "## EXPECT has no '### Non-runnable' sub-section — nothing records what this test proves "+
				"for the next person who reads it. Optional, but cheap to add")
		}
	}
	// VR12-E5 (V29-020): a `### Non-runnable` bullet that LOOKS like an assertion. A warning, never
	// an error — it IS understood, it is merely in the wrong place, and refusing the write would
	// stop an author recording something true because they put it one heading too low.
	w = append(w, expectShapeWarnings(s)...)

	// VR13-BF (V31-004 fix 3): `body has text containing …` names a JSON FIELD called `text`, which
	// is the one form the product's own guidance taught and the one form those answers cannot satisfy.
	w = append(w, bodyHasTextWarnings(s)...)
	// an AMQP Load profile that asks more of the generator than the pod can give.
	w = append(w, amqpLoadWarnings(s)...)
	// a declared Agreement that Repeats cannot support statistically.
	w = append(w, compareWarnings(s)...)

	// VF5b (warning half): a `standing-state` VERIFY that still carries `${correlation_id}` already
	// scopes itself to this run — the tag is then probably unnecessary, but it's advisory only,
	// never a refusal, because the query can legitimately still be a standing check that HAPPENS to
	// also reference the run's own id (e.g. a "not blocked on my own in-flight row" clause).
	if contains(s.Tags, standingStateTag) && strings.Contains(s.Verify.SQL, corrToken) {
		w = append(w, fmt.Sprintf("## VERIFY is tagged `%s` but its query still contains `%s` — that "+
			"already scopes it to this run, so the tag is probably unnecessary (VF5b)",
			standingStateTag, corrToken))
	}

	// ⚠ WIDENED, NOT BUILT (V29-020 housekeeping 1): this used to inspect only Database State /
	// Message Flow / Web UI, which is precisely why an http-ingestion scenario measured
	// `warnings: null` — the authoring gate the contract promises was silent for the commonest layer
	// in the estate.
	if contains(s.Layers, "Database State") || contains(s.Layers, "Message Flow") {
		// VR12-E9: RUNNABLE bullets only. A sentence the author deliberately filed under
		// '### Non-runnable' is documentation, and warning that documentation "is not run-time
		// verified" tells them what they already said.
		for _, u := range ParseDBExpect(s.RunnableExpect()).Uncoverable {
			w = append(w, fmt.Sprintf("EXPECT %q is not run-time-verified by this layer (unsupported form: range / JSONB / cast / operator, or a column assertion that didn't parse) — express it as 'NAME == VALUE' / 'NAME equals VALUE' / 'NAME is null' / 'row_count == N' / 'no rows', or move it to `### Non-runnable`, where prose about what this test proves belongs (V29-020)", u))
		}
	}
	// FINDING (UC-83): a Web UI scenario verifies rendered-DOM + "no backend 4xx/5xx" only.
	// An EXPECT that reaches past that (a DB row, a saga, a specific HTTP status code) is NOT
	// verified by the UI layer — warn so the author moves it to a chained step / its own layer
	// instead of getting silent false confidence.
	if contains(s.Layers, "Web UI") {
		// ⛔ V31-006: RUNNABLE ONLY. It warned about a line the author had ALREADY moved to
		// `### Non-runnable`, telling them to move it where it already was.
		// ⭐ V29-021: a bullet the ui GRAMMAR executes is never warned about, whatever words it
		// happens to contain. uiExceedRe matches `saga` / `row_count` / a status code anywhere, and
		// `- dom has .banner containing saga` is a DOM check the product now RUNS — telling the
		// author to move it would be telling them to delete a check that works. The warning is for a
		// bullet that reaches past the layer AND is executed by nothing.
		executed := map[string]bool{}
		if asserts, _ := ParseUIExpect(s.RunnableExpect()); len(asserts) > 0 {
			for _, a := range asserts {
				executed[a.Bullet] = true
			}
		}
		for _, b := range s.RunnableExpect() {
			if executed[bulletText(b)] {
				continue
			}
			if uiExceedRe.MatchString(b) {
				w = append(w, fmt.Sprintf("EXPECT %q exceeds the Web UI layer (which verifies rendered DOM + no backend 4xx/5xx) — assert a DB row / saga / HTTP status code in a chained step or its own layer, or move it to `### Non-runnable` (V29-020)", b))
			}
		}
	}
	return w
}

// uiExceedRe flags EXPECT bullets that reach past what the Web UI layer can verify: a
// saga, a DB row/count/persistence assertion, or a literal HTTP status code. Ordinary
// rendered-DOM bullets ("the dashboard renders", "no backend 4xx/5xx") do not match.
var uiExceedRe = regexp.MustCompile(`(?i)\bsaga\b|\brow_count\b|\b\d+\s+rows?\b|\bpersisted\b|\bobjects?\s+table\b|\bdatabase\s+(?:state|row)\b|\bstatus\s*(?:code\b|[=:]\s*[1-5]\d\d\b)`)

// FINDING-7/FINDING-9 (2026-06-19): the database-state / message-flow content layers used
// to assert only that the JDBC/queue result CONTAINED the correlation_id — a "a row exists"
// proxy that never evaluated the scenario's rich EXPECT, so authored assertions were
// silently unverified (false confidence). FINDING-9: the first fix only matched the literal
// "==" operator, which NONE of the prose-authored scenarios used, so they silently fell back
// to the proxy with no signal. This grammar accepts the prose synonyms AND warns (via
// Uncoverable) on column-looking bullets it can't execute, so the fall-back is never silent.
//
// Grammar (per clause; clauses split on , ; and "and"; a leading "N row(s) with/where ..."
// prefix is stripped; values must be a single token, an "A or B" alternation, or NULL):
//   - column equals : "NAME == VALUE" | "NAME = VALUE" | "NAME equals VALUE" | "NAME is VALUE"
//   - column in set : "NAME is A or B"  (alternation)
//   - column is null: "NAME is null"
//   - row count     : "row_count == N" | "N row(s)"
//   - negative      : "no rows" | "zero rows" | "0 rows" | "rejected" | "not persisted" | "has_rows == false"
type DBExpect struct {
	HasRows     bool              // default true; false = negative (zero-row / rejected) scenario
	RowCount    int               // -1 when unspecified
	Columns     map[string]string // name -> "value" | "v1|v2" (alternation) | "__NULL__"
	Uncoverable []string          // EXPECT bullets/clauses the layer can't execute (advisory)
}

var (
	// VR12-E9 — ANCHORED, so a negative must BE the clause, not merely appear inside a sentence.
	//
	// This was the last unanchored matcher in this file, and its neighbour dbNullRe below already
	// carried the comment explaining why anchoring matters ("this avoids matching mid-prose") — one
	// of the two was fixed and the other was not. Unanchored, the word "rejected" anywhere in a
	// bullet flipped HasRows to false and INVERTED what the scenario asserts about the database:
	// "the malformed variant is rejected by the API before it reaches the database" turned a
	// row_count==1 assertion into "expect no rows".
	//
	// The declared negatives are unchanged; only their position is now fixed.
	dbNegativeRe    = regexp.MustCompile(`(?i)^\s*(?:(?:no|zero|0)\s+rows?|rejected|not\s+persisted|has_rows\s*[=:]+\s*false)\s*$`)
	dbRowCountKwRe  = regexp.MustCompile(`(?i)\brow[_ ]?count\s*[=:]*\s*(\d+)\b`)
	dbRowCountNRe   = regexp.MustCompile(`(?i)\b(\d+)\s+rows?\b`)
	dbUncoverableRe = regexp.MustCompile(`(?i)within|±|\+/-|\brange\b|->>?|jsonb|::|\bbetween\b`)
	// anchored, so a column assertion must BEGIN the clause (modulo a "column " prefix) —
	// this avoids matching mid-prose like "the order is created".
	dbNullRe = regexp.MustCompile(`(?i)^(?:column\s+)?([a-z][a-z0-9_]*)\s+is\s+null$`)
	dbColRe  = regexp.MustCompile(`(?i)^(?:column\s+)?([a-z][a-z0-9_]*)\s*(?:==|=|equals|equal to|equal|is)\s+(.+)$`)
	// a column-looking clause with an operator we do NOT execute (=> warn, never silent).
	dbLooseOpRe   = regexp.MustCompile(`(?i)^(?:column\s+)?[a-z][a-z0-9_]*\s+(?:matches|contains|like|in)\b|^(?:column\s+)?[a-z][a-z0-9_]*\s*(?:!=|<>|>=|<=|>|<)\s`)
	dbRowPrefixRe = regexp.MustCompile(`(?i)^(?:exactly\s+)?\d+\s+rows?\s+(?:with|where|containing|having|whose)\s+`)
	dbClauseSplit = regexp.MustCompile(`(?i)\s*(?:,|;|\sand\s)\s*`)
	dbSingleTok   = regexp.MustCompile(`^\S+$`)
)

// ParseDBExpect extracts the structured DB/content assertions from EXPECT bullets.
func ParseDBExpect(expect []string) DBExpect {
	out := DBExpect{HasRows: true, RowCount: -1, Columns: map[string]string{}}
	seen := map[string]bool{}
	warn := func(s string) {
		if !seen[s] {
			seen[s] = true
			out.Uncoverable = append(out.Uncoverable, s)
		}
	}
	for _, b := range expect {
		// ⛔ A bullet that opens with `body` belongs to the BODY grammar (ParseBodyAsserts), which
		// parses it or refuses it — it is never a column. Read here, `body contains x` matched
		// dbLooseOpRe as a column named `body` with an unsupported operator, and every whole-body check
		// on the JMeter path was reported "cannot execute" while it ran (hub-dev ARG-SYN-005, run
		// 20260918T132528316).
		if ClaimsToBeBodyAssert(b) {
			continue
		}
		// VR12-E9: the negative check moved from the WHOLE BULLET to each CLAUSE, because the
		// regex is now anchored. Bullet-level + anchored would have refused a legitimate compound
		// like "no rows, status=400"; clause-level + anchored accepts that and still refuses
		// "…is rejected by the API before it reaches the database".
		for _, raw := range dbClauseSplit.Split(b, -1) {
			if dbNegativeRe.MatchString(strings.TrimSpace(raw)) {
				out.HasRows = false
			}
		}
		if m := dbRowCountKwRe.FindStringSubmatch(b); m != nil {
			out.RowCount, _ = strconv.Atoi(m[1])
		} else if m := dbRowCountNRe.FindStringSubmatch(b); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > 0 { // "0 rows" is the negative case, handled above
				out.RowCount = n
			}
		}
		for _, raw := range dbClauseSplit.Split(b, -1) {
			cl := strings.TrimSpace(dbRowPrefixRe.ReplaceAllString(strings.TrimSpace(raw), ""))
			if cl == "" {
				continue
			}
			// unsupported-form clauses (range / jsonb / cast / operator) -> warn (signal).
			if dbUncoverableRe.MatchString(cl) || dbLooseOpRe.MatchString(cl) {
				warn(cl)
				continue
			}
			if m := dbNullRe.FindStringSubmatch(cl); m != nil {
				out.Columns[strings.ToLower(m[1])] = "__NULL__"
				continue
			}
			if m := dbColRe.FindStringSubmatch(cl); m != nil {
				name := strings.ToLower(m[1])
				if name == "row_count" || name == "has_rows" {
					continue // grammar keyword, not a real column
				}
				val := strings.Trim(strings.TrimSpace(m[2]), `"'.`)
				if strings.EqualFold(val, "null") {
					out.Columns[name] = "__NULL__"
					continue
				}
				// value must be a single token or an "A or B" / "A/B" alternation of tokens.
				alts := regexp.MustCompile(`(?i)\s+or\s+|/`).Split(val, -1)
				ok := len(alts) > 0
				var toks []string
				for _, a := range alts {
					a = strings.Trim(strings.TrimSpace(a), `"'.`)
					if a == "" || !dbSingleTok.MatchString(a) {
						ok = false
						break
					}
					toks = append(toks, a)
				}
				if ok {
					out.Columns[name] = strings.Join(toks, "|")
				} else {
					warn(cl) // column-looking but the value is prose we can't execute
				}
			}
		}
	}
	if !out.HasRows { // a negative scenario asserts zero rows; column/count assertions don't apply
		out.RowCount = 0
		out.Columns = map[string]string{}
	}
	return out
}

// Props renders the DBExpect as the runner's -J property set for the content JMX templates.
// Columns are serialized "name=value;name=value" where value is a single token, a
// pipe-joined alternation ("v1|v2"), or the "__NULL__" sentinel. The JMX maps each name to
// its result-set column and asserts the actual cell value matches.
func (d DBExpect) Props() map[string]string {
	p := map[string]string{}
	if d.HasRows {
		p["expect.has_rows"] = "true"
	} else {
		p["expect.has_rows"] = "false"
	}
	if d.RowCount >= 0 {
		p["expect.row_count"] = strconv.Itoa(d.RowCount)
	}
	if len(d.Columns) > 0 {
		var pairs []string
		for k, v := range d.Columns {
			pairs = append(pairs, k+"="+v)
		}
		for i := 0; i < len(pairs); i++ { // stable order (deterministic prop + tests)
			for j := i + 1; j < len(pairs); j++ {
				if pairs[j] < pairs[i] {
					pairs[i], pairs[j] = pairs[j], pairs[i]
				}
			}
		}
		p["expect.columns"] = strings.Join(pairs, ";")
	}
	return p
}
