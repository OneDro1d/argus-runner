package scenario

import (
	"encoding/json"
	"fmt"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// V30-003 — THE SECTION CONTRACT. One closed list per level, loaded from the schema, refused BY NAME.
//
// ⛔ THE DEFECT IS NOT UNTIDINESS: A TYPO IN A DEFINED NAME DELETES A SECTION IN SILENCE.
// `## TIMEOUT` mistyped falls back to 10s. `## CLEANUP` mistyped means a scenario that promised to
// clean up simply does not. `## VERIFY`, `## References` — silent. Only `## Metadata` was caught.
//
// Every refusal here names the offending text AND the nearest defined name, because a rule that
// says "invalid section" to someone who typed `## TIEMOUT` has not helped them.

// sectionErrors is VR12-S1: only the defined `## ` names (the closed list in schemas/scenario.schema.yaml,
// nine since `## COMPARE`), and the required ones are present.
func sectionErrors(s *Scenario, text string) []Error {
	var errs []Error
	defined := SectionNames()
	for _, name := range s.DeclaredSections {
		if KnownSection(name) {
			continue
		}
		msg := fmt.Sprintf("## %s is not a defined section — a scenario has exactly %d: %s",
			name, len(defined), strings.Join(defined, ", "))
		if near := NearestName(name, defined); near != "" {
			msg += fmt.Sprintf(". Did you mean `## %s`? ⛔ A typo in a defined name DELETES that "+
				"section in silence, which is why this is refused rather than ignored", near)
		} else {
			msg += ". Prose about the requirement belongs in `## References`; prose about what this " +
				"test proves belongs in `## EXPECT` → `### Non-runnable` (V30-003)"
		}
		errs = append(errs, Error{Line: lineContaining(text, "## "+name, 1), Message: msg})
	}
	for _, req := range RequiredSections() {
		if _, ok := s.sectionLine[req]; !ok {
			// ⚠ CLEANUP has its own richer message (VR12-C1) and TIMEOUT its own (VR12-TO1); this is
			// the generic backstop for the rest, so no required section can go unmentioned.
			if req == "CLEANUP" || req == "TIMEOUT" {
				continue
			}
			errs = append(errs, Error{Line: 1, Message: "## " + req + " is required and is missing (V30-003)"})
		}
	}
	return errs
}

// metadataErrors is VR12-M1/M2/M3/M4: a closed FOUR-key list, and exactly one dispatch tag.
func metadataErrors(s *Scenario, text string, metaLine int) []Error {
	var errs []Error
	defined := MetadataKeys()
	for _, k := range s.DeclaredMetaKeys {
		if in(k, defined) {
			continue
		}
		msg := fmt.Sprintf("`**%s**` is not a defined Metadata key — the list is closed: %s",
			k, strings.Join(defined, ", "))
		if strings.EqualFold(k, "Priority") {
			// Named specially: 119 shipped scenarios carry it, so an author WILL hit this and
			// deserves to know it was removed rather than mistyped.
			msg = "`**Priority**` was REMOVED in 0.3.31 (V30-003): it was parsed, carried, and read " +
				"by nothing that decides anything — no UI, no template, no dashboard, no filter, no " +
				"ordering. Express priority as a TAG instead (specs/06:71 already does). Delete the line"
		} else if near := NearestName(k, defined); near != "" {
			msg += fmt.Sprintf(". Did you mean `**%s**`? A misspelt key silently EMPTIES its field", near)
		}
		errs = append(errs, Error{Line: lineContaining(text, "**"+k+"**", metaLine), Message: msg})
	}

	// VR12-M3/M4 — EXACTLY ONE DISPATCH TAG. Dispatch is by TAG and nothing validated one, so
	// `Tags: chian` silently routed a chain scenario to JMeter: it changed which ENGINE ran the
	// test, in silence. `ACC-006` carried BOTH `chain` and `mcp`; the switch ran it as a chain and
	// its `mcp` tag was ignored without a word.
	var found []string
	for _, tag := range s.Tags {
		if in(tag, DispatchTags()) {
			found = append(found, tag)
		}
	}
	tagLine := lineContaining(text, "**Tags**", metaLine)
	switch {
	case len(found) == 0:
		msg := fmt.Sprintf("`**Tags**` must carry exactly ONE dispatch tag (%s) — it selects which "+
			"ENGINE runs this scenario", strings.Join(DispatchTags(), " | "))
		// Offer the nearest, so a typo'd tag is a one-word fix rather than a hunt.
		for _, tag := range s.Tags {
			if near := NearestName(tag, DispatchTags()); near != "" {
				msg += fmt.Sprintf(". `%s` looks like `%s` — a mistyped dispatch tag routes the "+
					"scenario to the WRONG engine in silence", tag, near)
				break
			}
		}
		errs = append(errs, Error{Line: tagLine, Message: msg + " (V30-003)"})
	case len(found) > 1:
		errs = append(errs, Error{Line: tagLine, Message: fmt.Sprintf(
			"`**Tags**` carries %d dispatch tags (%s) and a scenario runs on exactly one engine. The "+
				"runner takes the first match and ignores the rest WITHOUT SAYING SO — pick one (V30-003)",
			len(found), strings.Join(found, ", "))})
	}
	return errs
}

// triggerErrors is VR12-T1/T2/T4.
func triggerErrors(s *Scenario, text string, metaLine int) []Error {
	var errs []Error
	trigLine := s.sectionStart("TRIGGER", metaLine)
	body := sectionBodyOf(text, "TRIGGER")

	// T1 — the METHOD verb, checked in Go rather than only in a schema nothing loaded.
	//
	// ⚠ methodRe ITSELF carries the enum (GET|POST|PUT|PATCH|DELETE followed by a backticked URL), so
	// an unknown verb does not produce a WRONG method — it produces NO method, and the line falls
	// through as unrecognised text. THAT is the silence this rule closes: a line reading FETCH /orders
	// left Trigger.Method empty and the scenario simply did not fire a request. So the check is on the
	// LINE SHAPE, not on the parsed value, and it names the verb the author actually wrote.
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "```") || methodRe.MatchString(l) {
			break // the real METHOD line, or the payload — either way the header region is over
		}
		m := verbShapedRe.FindStringSubmatch(l)
		if m == nil {
			continue // not a METHOD line at all; T2 below decides whether it is a stray header
		}
		errs = append(errs, Error{
			Line: lineContaining(text, l, trigLine),
			Message: fmt.Sprintf("`%s` is not a legal METHOD — one of %s. ⛔ An unrecognised verb does "+
				"not become a WRONG request, it becomes NO request: the line is ignored and the "+
				"scenario fires nothing (V30-003)", m[1], strings.Join(Methods(), ", ")),
		})
		break
	}

	// T2 — before the fence, only the METHOD line and PLAUSIBLE header lines.
	//
	// ⛔ THE RECONCILIATION WITH V29-018 D4, which look like they contradict and do not. That row
	// says "do NOT tighten headerRe to guess which lines were meant as headers; guessing is what
	// this register keeps re-finding". Both hold, because they act at different points: the PARSER
	// regex stays exactly as permissive as it is (so nothing changes at run time), and the VALIDATOR
	// refuses the implausible NAME at author time — while VR12-T3 has author__validate_scenario
	// return the header list, so the author SEES the stray line. Guessing is what the parser must
	// not do; refusing by a stated rule, with the list shown back, is not guessing.
	//
	// ⚡ AND IT IS NOW ARMED. A prose line that becomes a header used to be harmless because only
	// `Authorization` was ever sent. VR12-T3 puts every author header on the wire.
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "```") {
			break // the payload fence ends the header region
		}
		if l == "" || methodRe.MatchString(l) {
			continue
		}
		m := headerRe.FindStringSubmatch(l)
		if m == nil {
			continue // not header-shaped at all; the parser ignores it and so do we
		}
		if plausibleHeaderName(m[1]) {
			continue
		}
		errs = append(errs, Error{
			Line: lineContaining(text, l, trigLine),
			Message: fmt.Sprintf("`%s:` before the payload fence would be SENT as an HTTP header, and "+
				"%q is not a plausible header name — this looks like a note. Prose about the "+
				"requirement belongs in `## References`; prose about what this test proves belongs in "+
				"`## EXPECT` → `### Non-runnable` (V30-003)", m[1], m[1]),
		})
	}

	// T4 — a ```json payload must PARSE. Presence was checked; the JSON never was.
	if p := strings.TrimSpace(s.Trigger.Payload); p != "" && !s.Trigger.PayloadRaw {
		// ${VAR} / ${cid} are legal in a payload and are not JSON, so they are masked to a string
		// before the parse — the rule is "the SHAPE is valid JSON", not "it is JSON right now".
		if !json.Valid([]byte(maskVars(p))) {
			errs = append(errs, Error{
				Line:    lineContaining(text, "```json", trigLine),
				Message: "the ```json payload does not parse as JSON (V30-003) — a malformed payload reaches the SUT as a bad request and reads as a SUT defect",
			})
		}
	}
	return errs
}

// verifyErrors is VR12-VF1/VF3/VF10.
func verifyErrors(s *Scenario, text string, metaLine int) []Error {
	var errs []Error
	line, declared := s.sectionLine["VERIFY"]
	if !declared {
		return nil
	}
	// VF1 — REFUSED on mcp / chain / ui. ⭐ Measured: 0 of the 83 mcp/chain/ui scenarios carry one,
	// so this rule costs ZERO migration — every author already treats it as N/A.
	if tag := NativeEngineTag(s); tag != "" {
		return append(errs, Error{Line: line, Message: fmt.Sprintf(
			"## VERIFY has no meaning on a `%s` scenario — its verdict comes from the engine that runs "+
				"it, not from a query. Delete the section (V30-003)", tag)})
	}
	// VF10 — a bare or ```http fence is refused. `Verify.HTTP` is a vestigial catch-all for ANY
	// untagged fence, used by zero scenarios; under "only what is defined" it must not be expressible.
	if strings.TrimSpace(s.Verify.HTTP) != "" {
		return append(errs, Error{Line: line, Message: "## VERIFY may hold a ```sql block or `N/A` + a " +
			"reason — a bare or ```http fence is not a defined form and nothing executes it (V30-003)"})
	}
	// VF3 — executable OR `N/A` + reason. A descriptive LINE beside a block stays legal.
	if q := strings.TrimSpace(s.Verify.SQL); q != "" {
		// VF4-VF12: what the query may SAY. The columns come from the scenario's own runnable
		// EXPECT bullets, so VF6 can catch an assertion on a column the query never returns.
		dbx := ParseDBExpect(s.RunnableExpect())
		var cols []string
		for name := range dbx.Columns {
			cols = append(cols, name)
		}
		// `standing-state` (VF5b): opts OUT of VF5's run-scoping requirement for a query that
		// measures a STANDING invariant of the SUT rather than a row this run created. Reused tag
		// parsing (s.Tags, from `## Metadata` `- **Tags**:`), same as every other tag literal.
		standing := contains(s.Tags, standingStateTag)
		for _, msg := range VerifySQLErrors(q, cols, standing) {
			errs = append(errs, Error{Line: lineContaining(text, "```sql", line), Message: msg})
		}
		if standing {
			// VF5b — the abuse guard: the tag is refused unless the EXPECT it rides with actually
			// commits to a computed value, so it can never become a quieter "some row exists".
			for _, msg := range StandingStateExpectErrors(dbx) {
				errs = append(errs, Error{Line: lineContaining(text, "```sql", line), Message: msg})
			}
		} else {
			// VF14 (AC-D31) — row existence judged on a query that always returns one row. Not run
			// under `standing-state`: VF5b already refuses the same EXPECT forms there, and saying it
			// twice would read as two defects.
			for _, msg := range AggregateExpectErrors(q, dbx) {
				errs = append(errs, Error{Line: lineContaining(text, "```sql", line), Message: msg})
			}
		}
		return errs
	}
	if d := strings.TrimSpace(s.Verify.Description); cleanupNARe.MatchString(d) && cleanupJustified(d) {
		return nil
	}
	return append(errs, Error{Line: line, Message: "## VERIFY is prose only — it must hold an " +
		"executable ```sql block, or `N/A — <why nothing needs verifying beyond the EXPECT>`. " +
		"`Verify.Description` is read by NOTHING, so a prose-only VERIFY is a section that looks " +
		"like it does something and does not (V30-003)"})
}

// timeoutErrors is VR12-TO1/TO2/TO3.
//
// Three published passages say TIMEOUT is required and none of it was enforced anywhere — not in
// validate.go, not in argus's preflight.py. specs/02:119's published validation-error example is
// literally `{"line": 23, "message": "TIMEOUT field missing"}`: an error the specs document and the
// code has never emitted.
func timeoutErrors(s *Scenario, text string, metaLine int) []Error {
	line := s.sectionStart("TIMEOUT", metaLine)
	if _, ok := s.sectionLine["TIMEOUT"]; !ok {
		return []Error{{Line: line, Message: "## TIMEOUT is required (V30-003) — without it the parser " +
			"silently substitutes 10s, so a scenario that needs longer fails for a reason its author " +
			"never chose. Give it the value this scenario actually needs"}}
	}
	raw := strings.TrimSpace(s.Timeout)
	m := timeoutRe.FindStringSubmatch(raw)
	if m == nil {
		return []Error{{Line: line, Message: fmt.Sprintf(
			"## TIMEOUT %q does not match `^[0-9]+(ms|s)$` — e.g. `15s` or `500ms` (V30-003)", raw)}}
	}
	// TO3 — <= 30s, or <= 60s when the Layer includes Rate Limiting (specs/06:75 NFR-5: "RATE-001
	// may take longer (ramp); cap 60s").
	n, _ := strconv.Atoi(m[1])
	ms := n
	if m[2] == "s" {
		ms = n * 1000
	}
	cap, why := timeoutCeiling(s)
	if ms > cap {
		return []Error{{Line: line, Message: fmt.Sprintf(
			"## TIMEOUT %s exceeds the ceiling of %s. A scenario that needs longer is usually waiting "+
				"for something it should assert on instead (V30-003)", raw, why)}}
	}
	return nil
}

var timeoutRe = regexp.MustCompile(`^([0-9]+)(ms|s)$`)

// DefaultTimeout is the fallback TimeoutDuration returns for a scenario carrying no parseable
// `## TIMEOUT` — the SAME 10s the parser already defaults Scenario.Timeout to (parse.go:334,
// VR12-TO1). Validate refuses an absent/malformed TIMEOUT before a scenario reaches a run on the
// normal authoring path (timeoutErrors above); this constant exists so a Scenario built some other
// way (a unit test, a pre-VR12-TO1 catalog row) is still bounded at run time rather than handed an
// unbounded context.
const DefaultTimeout = 10 * time.Second

// TimeoutDuration parses the declared `## TIMEOUT` into a time.Duration for RUN-TIME enforcement
// (VR12-TO-RUN — the twin of timeoutErrors' AUTHOR-TIME check, same grammar and same regexp, so a
// value that already passed validation always parses here too). An empty or malformed value falls
// back to DefaultTimeout instead of making every caller invent its own number or (worse) run under
// context.Background() with no bound at all — which is the defect this method exists to close.
func (s *Scenario) TimeoutDuration() time.Duration {
	m := timeoutRe.FindStringSubmatch(strings.TrimSpace(s.Timeout))
	if m == nil {
		return DefaultTimeout
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return DefaultTimeout
	}
	if m[2] == "s" {
		return time.Duration(n) * time.Second
	}
	return time.Duration(n) * time.Millisecond
}

// varRe masks `${…}` so a payload carrying one can still be checked for JSON SHAPE.
var varRe = regexp.MustCompile(`\$\{[^}]*\}`)

func maskVars(s string) string { return varRe.ReplaceAllString(s, "x") }

// knownHeaderNames are the header names that carry no `-` and are still real. Everything else must
// look like a header: contain a `-`, or start with `X-`.
var knownHeaderNames = map[string]bool{
	"Accept": true, "Authorization": true, "Cookie": true, "Date": true, "Expect": true,
	"From": true, "Host": true, "Origin": true, "Pragma": true, "Range": true,
	"Referer": true, "Server": true, "TE": true, "Trailer": true, "Upgrade": true, "Via": true,
	"Warning": true, "Allow": true, "Age": true, "ETag": true, "Link": true, "Location": true,
	"Vary": true, "Connection": true,
}

// plausibleHeaderName is VR12-T2's test: a known name, or one containing `-`, or `X-`-prefixed.
//
// ⚠ It is deliberately GENEROUS. Refusing a real header would be worse than accepting a stray note,
// and the measurement says the risk is small either way: 0 of 119 scenarios trip this rule today —
// the only three header lines in the whole estate are genuine `Authorization` headers.
func plausibleHeaderName(name string) bool {
	c := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	return knownHeaderNames[c] || strings.Contains(c, "-")
}

// sectionBodyOf returns one `## ` section's raw text — the validator needs the TRIGGER's header
// region, which the parsed Scenario does not keep.
func sectionBodyOf(text, name string) string {
	_, rest, ok := strings.Cut(text, "\n## "+name+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// timeoutCeiling is VR12-TO3's ceiling, in ms, with the reason to print.
//
// ⚠ A DELIBERATE DEVIATION FROM specs/06 NFR-5's FLAT 30s, recorded here so it is never read as
// drift. That number was written for ONE HTTP request, before chain scenarios and before chained
// LAYERS existed. Measured on the shipped catalogue, a flat 30s would refuse 41 of 119 scenarios:
//
//	36 chain scenarios declaring 120s   — a 7-step chain at 30s gives each step four seconds
//	 5 chained-layer scenarios at 45s   — `HTTP Ingestion -> Database State` waits for an async
//	                                      insert; `-> External Delivery` waits for a webhook
//
// Failing those would make 41 scenarios red for a reason nobody chose, which is precisely the
// defect class this whole round exists to remove. So the ceiling follows the SHAPE: how much does
// this scenario have to WAIT before it can assert?
//
// ⛔ The numbers are the estate's own measured values, not invented headroom — 45s ≤ 60s and
// 120s ≤ 120s — so no scenario is being quietly given room it did not already take.
// ⚠ OWNER-VISIBLE: specs/06 NFR-5 is amended by this. It is listed in the QA record as a deviation
// to confirm, not smuggled in.
func timeoutCeiling(s *Scenario) (int, string) {
	switch {
	case contains(s.Tags, ChainTag):
		return 120000, "120s (a chain runs N steps in sequence)"
	case contains(s.Layers, "Rate Limiting"):
		return 60000, "60s (the Rate Limiting ceiling — a limiter test ramps; specs/06 NFR-5)"
	case len(s.Layers) > 1:
		return 60000, "60s (a chained layer waits for an async settle — an insert, a delivery)"
	default:
		return 30000, "30s (specs/06 NFR-5)"
	}
}

// verbShapedRe matches a line SHAPED like a METHOD line — an uppercase word then a backticked URL —
// whatever the word is. It is how T1 can name a verb the parser refused to recognise.
var verbShapedRe = regexp.MustCompile("^([A-Z]{3,10})\\s+`[^`]+`")
