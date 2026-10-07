// Package scenario parses and validates argus scenario markdown. The parser is a
// faithful Go port of argus scripts/parse_scenario.py (lenient — never errors,
// just omits absent fields), extended with the vision `references` field
// (DF-DEC-008). The semantic validator (validate.go) layers VR-A10's rules on top
// with line-level errors — argus's parser has none.
package scenario

import (
	"fmt"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// CanonicalLayers are the layer strings the validator accepts — must match the runtime
// authority (argus.layerDir keys; argus preflight.py's LAYER_TO_CONFIG — DF-DEC-005).
// "Web UI" is the D4 web-UI layer (ui-tagged scenarios drive the vendored Playwright path).
var CanonicalLayers = []string{
	"HTTP Ingestion", "Message Flow", "Database State",
	"External Delivery", "Error Path", "Rate Limiting", "Permissions", "Web UI",
	AMQPLoadLayer, // NEW name on purpose, so an executor that predates it refuses the scenario
	HTTPLoadLayer, // the same, for the stepped HTTP ramp
}

// ⛔ CanonicalPriorities is GONE (VR12-M2 / V30-003). `**Priority**` was parsed, carried into
// AsParsedMap, surfaced in exactly one place (author__list_scenarios) and read by NOTHING that
// decides anything — no UI, no JMX template, no dashboard, no runner logic, no filter, no ordering.
// specs/06:71 already expresses priority as a TAG. The key is refused as unknown, by name, with a
// message saying it was removed rather than mistyped — 119 shipped scenarios carried it.

// Trigger is the parsed ## TRIGGER section.
type Trigger struct {
	Method  string `json:"method,omitempty"`
	URL     string `json:"url,omitempty"`
	Payload string `json:"payload,omitempty"`
	// PayloadRaw marks a payload declared in a ```text fence: the author is saying these bytes are
	// DELIBERATELY not JSON (VR12-T4). It is how a negative test like MSGF-002 — post a truncated
	// body, watch the edge reject it — stays expressible under a rule that says a ```json payload
	// must parse. The fence language is the declaration.
	PayloadRaw bool `json:"-"`
	// Headers are custom request headers declared in the TRIGGER (e.g. Authorization for a
	// permissions test). Keys are canonicalized (Authorization). Present-with-empty means
	// "send no token" — Unit 6 / 4.8.
	Headers map[string]string `json:"headers,omitempty"`
}

// Verify is the parsed ## VERIFY section (one of sql / http / description).
type Verify struct {
	SQL         string `json:"sql,omitempty"`
	HTTP        string `json:"http,omitempty"`
	Description string `json:"description,omitempty"`
}

// Scenario is the parsed scenario (mirrors parse_scenario.py's dict + references).
type Scenario struct {
	ID     string   `json:"id,omitempty"`
	Layers []string `json:"layers,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// Target is the ONE word that selects a named extra target of the scenario's kind
	// (`- **Target**: graph` → targets.http_targets.graph on an HTTP layer; see TargetKind).
	// Empty = the plain slot, the default. VR10-S3 / V28-012.
	Target  string  `json:"target,omitempty"`
	Trigger Trigger `json:"trigger"`
	Verify  Verify  `json:"verify"`

	Expect []string `json:"expect"`
	// VR12-E1 (V29-020 point 5): `## EXPECT` splits into `### Runnable` and `### Non-runnable`.
	// ⛔ Expect above keeps BOTH parts in file order — it is the report/holdout set. These ride
	// alongside it; they never replace it.
	ExpectRunnable    []string `json:"expect_runnable,omitempty"`
	ExpectNonRunnable []string `json:"expect_non_runnable,omitempty"`
	// ExpectSubheaded is false for a scenario written before VR12-E1 (no `### ` heading at all).
	// Parse then treats every bullet as runnable — today's behaviour — while Validate refuses it.
	ExpectSubheaded          bool     `json:"-"`
	ExpectUnknownSubsections []string `json:"-"`
	ExpectStrandedBullets    int      `json:"-"`
	Timeout                  string   `json:"timeout"`
	Cleanup                  Cleanup  `json:"cleanup"`
	// DeclaredSections / DeclaredMetaKeys are what the AUTHOR wrote — the raw names, including any
	// the closed lists do not define. VR12-S1 / VR12-M1 refuse those by name.
	DeclaredSections []string `json:"-"`
	DeclaredMetaKeys []string `json:"-"`
	// CleanupDeclared records whether a `## CLEANUP` heading was present AT ALL — distinct from a
	// section that is present and empty. VR12-C1 refuses both, with different messages, because the
	// fix is different: one needs a section, the other needs a body.
	CleanupDeclared bool `json:"-"`
	// CleanupUnknownFences names every fence language in `## CLEANUP` that this parser does not
	// run. ⛔ NAMED, never dropped (VR12-C3).
	CleanupUnknownFences []string `json:"-"`
	References           []string `json:"references,omitempty"`

	// LoadDeclared is true when the file carries a `## LOAD` section at all — distinct from a
	// declared-but-invalid one (Load stays nil in both cases; Validate tells them apart, VR-A10).
	LoadDeclared bool `json:"-"`
	// Load is the parsed `## LOAD` block (AC-11, mode 2 — "under load"): an optional profile that
	// drives the JMeter thread group beyond the product's one-thread/one-loop default. Absent (nil)
	// means that default, unchanged, on every scenario written before this existed.
	Load *LoadProfile `json:"load,omitempty"`
	// loadRaw carries the RAW strings behind Load, keyed by field name, so Validate can re-parse
	// them and report a line-level error naming exactly what was written — Parse itself stays
	// lenient (VR-A10's split: the parser never errors, the validator does).
	loadRaw map[string]string
	// AMQPLoad is the parsed `## LOAD` of an `AMQP Load` scenario. Load stays nil
	// for such a scenario, so every consumer of Load (survival plane, ComputeLoadStats, the process
	// backstop) ignores it. amqpLoadRaw carries the raw strings for the validator.
	AMQPLoad    *AMQPLoadProfile `json:"amqp_load,omitempty"`
	amqpLoadRaw map[string]string
	// HTTPLoad is the parsed `## LOAD` of an `HTTP Load` scenario, kept only when it has no
	// problem. Load stays nil for such a scenario, for the reason it does on AMQP Load.
	HTTPLoad    *HTTPLoadProfile `json:"http_load,omitempty"`
	httpLoadRaw map[string]string

	// CompareDeclared is true when the file carries a `## COMPARE` section at all.
	CompareDeclared bool `json:"-"`
	// Compare is the parsed `## COMPARE` section, kept ONLY when it has no problem. Absent (nil) on
	// every scenario written before this existed, and on a declared-but-invalid one: CompareProblems
	// tells the two apart, so a run in compare mode can refuse instead of running with a rule ignored.
	Compare         *compare.Rules `json:"compare,omitempty"`
	compareProblems []compare.Problem

	// section heading -> 1-based line number where the "## Heading" appears.
	sectionLine map[string]int
}

// CompareProblems are the problems buildCompare found in a declared `## COMPARE` section.
func (s *Scenario) CompareProblems() []compare.Problem { return s.compareProblems }

// LoadProfile is the validated `## LOAD` block (AC-11): a declared load for mode 2. Every field is
// a positive, bounded number (validate.go); DurationSeconds may be 0 to mean "loop-count only, no
// scheduler cutoff" — the JMeter thread group (internal/argus.DeriveProps) reads these as
// `load.users` / `load.ramp` / `load.duration`.
type LoadProfile struct {
	Users           int     `json:"users"`
	RampSeconds     int     `json:"ramp_seconds"`
	DurationSeconds int     `json:"duration_seconds"`
	TargetP95Ms     int     `json:"target_p95_ms"`
	MaxErrorRate    float64 `json:"max_error_rate"`
}

var (
	headingRe = regexp.MustCompile(`^##\s+(.+)`)
	// The id capture takes the WHOLE rest of the line (trimmed), not the first word: an id with a
	// space in it must reach the validator intact so the safety rule can refuse it by name
	// (VR10-S4-1). Capturing \S+ turned "my scenario" into the perfectly valid id "my" — a silent
	// truncation, which is the very failure mode V28-006 exists to remove.
	idRe     = regexp.MustCompile(`\*\*ID\*\*:[ \t]*(.+)`)
	layerRe  = regexp.MustCompile(`\*\*Layer\*\*:\s*(.+)`)
	tagsRe   = regexp.MustCompile(`\*\*Tags\*\*:\s*(.+)`)
	targetRe = regexp.MustCompile(`\*\*Target\*\*:\s*(\S+)`) // VR10-S3: one word, the named target
	methodRe = regexp.MustCompile("^(GET|POST|PUT|PATCH|DELETE)\\s+`([^`]+)`")
	headerRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*):[ \t]?(.*)$`) // Unit 6: a TRIGGER header line
)

// loadFieldNames is the closed set of `## LOAD` keys (AC-11), field name -> the `**Bold Name**` an
// author writes. ONE list, walked by both Parse (lenient extraction) and Validate (bounds
// checking), so the two can never disagree on what the section contains.
var loadFieldNames = []struct{ key, label string }{
	{"users", "Users"},
	{"ramp_seconds", "Ramp Seconds"},
	{"duration_seconds", "Duration Seconds"},
	{"target_p95_ms", "Target P95 Ms"},
	{"max_error_rate", "Max Error Rate"},
}

// loadFieldRe finds one `**Label**: value` line inside the `## LOAD` body.
func loadFieldRe(label string) *regexp.Regexp {
	return regexp.MustCompile(`\*\*` + regexp.QuoteMeta(label) + `\*\*:\s*(.+)`)
}

// parseLoadRaw extracts the RAW strings for every declared LOAD field (VR-A10 split: lenient —
// a missing or nonsense value is simply absent from the map, never an error here).
func parseLoadRaw(body string) map[string]string {
	out := map[string]string{}
	for _, f := range loadFieldNames {
		if m := loadFieldRe(f.label).FindStringSubmatch(body); m != nil {
			out[f.key] = strings.TrimSpace(m[1])
		}
	}
	return out
}

type section struct {
	body string
	line int // 1-based line of the "## Heading"
}

func splitSections(text string) map[string]section {
	out := map[string]section{}
	var cur string
	var curLines []string
	var curStart int
	flush := func() {
		if cur != "" {
			out[cur] = section{body: strings.TrimSpace(strings.Join(curLines, "\n")), line: curStart}
		}
	}
	for i, line := range strings.Split(text, "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = strings.TrimSpace(m[1])
			curLines = nil
			curStart = i + 1
		} else {
			curLines = append(curLines, line)
		}
	}
	flush()
	return out
}

func firstCodeBlock(text, lang string) string {
	var re *regexp.Regexp
	if lang != "" {
		re = regexp.MustCompile("(?s)```" + lang + "\\s*\\n(.*?)```")
	} else {
		re = regexp.MustCompile("(?s)```\\w*\\s*\\n(.*?)```")
	}
	if m := re.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// parseTriggerHeaders extracts `Header-Name: value` lines from the TRIGGER body that precede
// the payload code-fence (Unit 6 / 4.8). The METHOD `url` line and JSON body lines are not
// headers. Keys are canonicalized (Authorization); a present-but-empty value is kept (it
// means "send no token"). Returns nil when there are none.
func parseTriggerHeaders(trig string) map[string]string {
	var out map[string]string
	for _, line := range strings.Split(trig, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "```") {
			break // stop at the payload block
		}
		if l == "" || methodRe.MatchString(l) {
			continue
		}
		m := headerRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[textproto.CanonicalMIMEHeaderKey(m[1])] = strings.TrimSpace(m[2])
	}
	return out
}

// subHeadingRe matches a `### ` sub-heading inside a section body. headingRe (`^##\s+`) deliberately
// does NOT match it — `##` followed by `#` is not whitespace — so a `### ` line has always fallen
// into its section's body as ordinary text. VR12-E1 gives it meaning inside ## EXPECT.
var subHeadingRe = regexp.MustCompile(`^###\s+(.+?)\s*$`)

// ExpectParts is `## EXPECT` split by VR12-E1 (V29-020 point 5).
//
// ⛔ THE INVARIANT: All is every bullet, in file order, whatever sub-section it came from. It is what
// Scenario.Expect is set from, and Expect is what internal/argus builds Failure.Expected from and what
// redactExpected NILs for the product hat. Narrowing it would change the report AND the holdout.
type ExpectParts struct {
	All         []string // every bullet, file order — the report/holdout set
	Runnable    []string // bullets the product must execute
	NonRunnable []string // prose: expected results it cannot check automatically
	Subheaded   bool     // at least one `### ` sub-heading was present
	Unknown     []string // `### ` names that are neither Runnable nor Non-runnable
	Stranded    int      // bullets that appeared BEFORE the first `### ` in a sub-headed section
}

// RunnableExpect returns the bullets the product must EXECUTE. Every runtime consumer of a
// scenario's claims reads this, never Expect.
//
// ⛔ V31-002 (R1) DELETED ITS FALLBACK. It used to return every bullet when a file was not
// sub-headed — the branch that let an OLD file be half-executed by new strict code. The owner,
// 2026-09-12: *"there always will be a split so that if is redundant."* Old-format scenarios are not
// supported; a flat `## EXPECT` now declares NO runnable check, and the run reports that as `error`
// (R10) instead of silently judging bullets the author never split.
//
// ⚠ A Scenario built by hand — `&scenario.Scenario{Expect: []string{…}}` — therefore has NO runnable
// bullets. That is now correct rather than a trap: nothing in production builds one (asserted by
// TestNoProductionCodeBuildsAScenarioByHand), and a TEST that builds one must set ExpectRunnable
// explicitly, which is also what makes each test say which bullets it means to be executed.
func (s *Scenario) RunnableExpect() []string {
	return s.ExpectRunnable
}

// splitExpect parses the ## EXPECT body into its two sub-sections.
//
// ⛔ V31-002 (R1): it is no longer lenient on the old shape. A body with no `### ` heading leaves
// Runnable EMPTY — the bullets are still counted in All, so the file is read and reportable, but
// nothing is executed. Validate refuses such a file at the author path (a bullet directly under
// `## EXPECT`), and the run reports `error` because nothing could be compared (R10) — neither of
// them by judging the file's FORMAT.
func splitExpect(body string) ExpectParts {
	var p ExpectParts
	cur := ""
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := subHeadingRe.FindStringSubmatch(s); m != nil {
			name := strings.TrimSpace(m[1])
			switch strings.ToLower(name) {
			case "runnable":
				cur = "runnable"
			case "non-runnable":
				cur = "non-runnable"
			default:
				cur = "unknown"
				p.Unknown = append(p.Unknown, name)
			}
			p.Subheaded = true
			continue
		}
		if !strings.HasPrefix(s, "- ") {
			continue
		}
		b := strings.TrimSpace(s[2:])
		p.All = append(p.All, b)
		switch cur {
		case "runnable":
			p.Runnable = append(p.Runnable, b)
		case "non-runnable":
			p.NonRunnable = append(p.NonRunnable, b)
		case "unknown":
			// counted in All (it is still in the file and still reaches the report), but it is
			// neither a claim nor prose until the author picks a real sub-section.
		default:
			p.Stranded++
		}
	}
	return p
}

func bulletList(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "- ") {
			out = append(out, strings.TrimSpace(s[2:]))
		}
	}
	return out
}

// Parse extracts the structured scenario from markdown (lenient, like parse_scenario.py).
func Parse(text string) *Scenario {
	secs := splitSections(text)
	s := &Scenario{Timeout: "10s", sectionLine: map[string]int{}}
	for name, sec := range secs {
		s.sectionLine[name] = sec.line
	}

	// VR12-S1: the `## ` names the AUTHOR wrote, in file order — including the undefined ones. The
	// validator refuses those by name and offers the nearest defined one, because the defect this
	// closes is a TYPO silently deleting a section.
	for name := range secs {
		s.DeclaredSections = append(s.DeclaredSections, name)
	}
	sort.Strings(s.DeclaredSections)

	meta := secs["Metadata"].body
	s.DeclaredMetaKeys = metaKeysIn(meta)
	if m := idRe.FindStringSubmatch(meta); m != nil {
		s.ID = strings.TrimSpace(m[1]) // trailing spaces / a CR are not part of the id
	}
	if m := layerRe.FindStringSubmatch(meta); m != nil {
		for _, l := range strings.Split(m[1], "->") {
			if t := strings.TrimSpace(l); t != "" {
				s.Layers = append(s.Layers, t)
			}
		}
	}
	// VR12-M2 (V30-003): `**Priority**` is REMOVED. It was parsed, carried into AsParsedMap and
	// surfaced in exactly one place (author__list_scenarios) — and read by NOTHING that decides
	// anything: no UI, no JMX template, no dashboard, no runner logic, no filter, no ordering. It
	// was always redundant; specs/06:71 already expresses priority as a TAG.
	if m := tagsRe.FindStringSubmatch(meta); m != nil {
		for _, t := range strings.Split(m[1], ",") {
			if tt := strings.TrimSpace(t); tt != "" {
				s.Tags = append(s.Tags, tt)
			}
		}
	}
	if m := targetRe.FindStringSubmatch(meta); m != nil {
		s.Target = m[1]
	}

	trig := secs["TRIGGER"].body
	if m := methodRe.FindStringSubmatch(trig); m != nil {
		s.Trigger.Method, s.Trigger.URL = m[1], m[2]
	}
	if p := firstCodeBlock(trig, "json"); p != "" {
		s.Trigger.Payload = p
	} else if p := firstCodeBlock(trig, "text"); p != "" {
		// VR12-T4 (V30-003): a ```text payload is the author DECLARING that these bytes are
		// deliberately not JSON. MSGF-002 exists to post a truncated body and watch the edge reject
		// it — under "a ```json payload must PARSE" it could not say so, and the choice was either
		// to refuse a legitimate negative test or to stop checking the payloads that ARE meant to be
		// JSON. The fence language is the declaration, so both rules can hold.
		s.Trigger.Payload, s.Trigger.PayloadRaw = p, true
	}
	s.Trigger.Headers = parseTriggerHeaders(trig)

	ver := secs["VERIFY"].body
	if sql := firstCodeBlock(ver, "sql"); sql != "" {
		s.Verify.SQL = sql
	} else if http := firstCodeBlock(ver, ""); http != "" {
		s.Verify.HTTP = http
	} else if ver != "" {
		s.Verify.Description = ver
	}

	ep := splitExpect(secs["EXPECT"].body)
	s.Expect = ep.All
	s.ExpectRunnable, s.ExpectNonRunnable = ep.Runnable, ep.NonRunnable
	s.ExpectSubheaded = ep.Subheaded
	s.ExpectUnknownSubsections, s.ExpectStrandedBullets = ep.Unknown, ep.Stranded

	if t := strings.TrimSpace(secs["TIMEOUT"].body); t != "" {
		s.Timeout = t
	}

	// VR12-C1: EVERY declared block, in order. The old code took the sql block ELSE the bash
	// block — first wins, the rest discarded — which is VR12-E8's defect wearing a different hat.
	s.Cleanup, s.CleanupUnknownFences = ParseCleanup(secs["CLEANUP"].body)
	_, s.CleanupDeclared = secs["CLEANUP"]

	// References (DF-DEC-008): a "## References" section of "- " bullets.
	if ref, ok := secs["References"]; ok {
		s.References = bulletList(ref.body)
	}

	// `## LOAD` (AC-11): lenient extraction, exactly like every other section here — a value that
	// does not parse just leaves Load nil (Validate is where nonsense is refused, with a line
	// number). Absent section = absent Load = the one-thread/one-loop default, untouched.
	if sec, ok := secs["LOAD"]; ok {
		s.LoadDeclared = true
		if isAMQPLoad(s) {
			// this layer has its OWN fields. Load stays nil, so every legacy consumer of
			// it (survival plane, ComputeLoadStats, the process backstop) ignores the scenario.
			s.amqpLoadRaw = parseAMQPLoadRaw(sec.body)
			if p, probs := buildAMQPLoad(s.amqpLoadRaw); len(probs) == 0 {
				s.AMQPLoad = p
			}
		} else if IsHTTPLoad(s) {
			// the HTTP ramp's own fields, the AMQP ramp's names where they mean the same thing.
			s.httpLoadRaw = parseHTTPLoadRaw(sec.body)
			if p, probs := buildHTTPLoad(s.httpLoadRaw); len(probs) == 0 {
				s.HTTPLoad = p
			}
		} else {
			s.loadRaw = parseLoadRaw(sec.body)
			if lp, ok := lenientLoadProfile(s.loadRaw); ok {
				s.Load = lp
			}
		}
	}

	// `## COMPARE`: kept only when it has no problem. A declared-but-invalid section
	// leaves Compare nil and CompareProblems non-empty, so Validate refuses it and a compare-mode run
	// refuses the check, instead of running with a rule ignored.
	if sec, ok := secs["COMPARE"]; ok {
		s.CompareDeclared = true
		rules, probs := buildCompare(text, sec.line)
		s.Compare, s.compareProblems = rules, probs
	}

	return s
}

// lenientLoadProfile builds a LoadProfile ONLY when every field is present and parses cleanly —
// the same all-or-nothing leniency the rest of Parse applies (a malformed **ID** is not half-kept
// either). A partial/bad block is caught by Validate; DeriveProps sees a nil Load and applies the
// unchanged default, which is the correct runtime behaviour for a file that should never have
// reached a run in that shape.
func lenientLoadProfile(raw map[string]string) (*LoadProfile, bool) {
	users, err1 := strconv.Atoi(raw["users"])
	ramp, err2 := strconv.Atoi(raw["ramp_seconds"])
	dur, err3 := strconv.Atoi(raw["duration_seconds"])
	p95, err4 := strconv.Atoi(raw["target_p95_ms"])
	rate, err5 := strconv.ParseFloat(raw["max_error_rate"], 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return nil, false
	}
	return &LoadProfile{
		Users: users, RampSeconds: ramp, DurationSeconds: dur,
		TargetP95Ms: p95, MaxErrorRate: rate,
	}, true
}

// AsParsedMap renders the scenario as the bundle's `scenario.parsed` map (test-hat only).
func (s *Scenario) AsParsedMap() map[string]any {
	m := map[string]any{
		"trigger": s.Trigger, "verify": s.Verify, "expect": s.Expect,
		"timeout": s.Timeout, "layers": s.Layers,
	}
	// VR12-S1: `id` is `required` in schemas/scenario.schema.yaml and was NEVER emitted here, so every
	// parsed scenario failed its own published contract. Additive — a consumer gains a key it could
	// previously only read from `markdown`. (V30-003: the schema becomes the source of truth, and it
	// must be made TRUE before anything loads it.)
	if s.ID != "" {
		m["id"] = s.ID
	}
	// ⛔ `priority` is NOT emitted (VR12-M2 / V30-003): the key is gone from the contract, so
	// emitting it would put a property in the parsed map that the schema no longer declares — and
	// `additionalProperties: false` would then make every parsed scenario violate its own contract,
	// which is the exact shape of defect the schema correction fixed.
	if len(s.Tags) > 0 {
		m["tags"] = s.Tags
	}
	if s.Target != "" {
		m["target"] = s.Target
	}
	if s.Compare != nil {
		m["compare"] = s.Compare
	}
	return m
}

// Tag literals for the runner-native paths. Duplicated from argus (which this package cannot
// import — that would be an import cycle), exactly as config.isMCPScenario does.
const (
	tagMCP   = "mcp"
	tagChain = ChainTag
	tagUI    = "ui"
)

// TargetKind answers which KIND of named target the **Target** word selects on this scenario —
// the word means one thing per layer (VR10-S3-5): mcp by tag; else the terminal layer decides
// (HTTP Ingestion / Error Path / Rate Limiting / Permissions → http; Database State → database;
// Message Flow → message_broker). Where the word has no meaning it is refused, never ignored:
// a Web UI scenario uses app_url, targets.external has no named form, and a chain selects its MCP
// target PER STEP (`"target": "<name>"` on the step). ("", nil) when the scenario carries no word.
func TargetKind(s *Scenario) (string, error) {
	if s == nil || s.Target == "" {
		return "", nil
	}
	switch {
	case contains(s.Tags, tagChain):
		return "", fmt.Errorf("**Target** is not read on a chain scenario — a chain selects its MCP target per step: put \"target\": %q on the step", s.Target)
	case contains(s.Tags, tagMCP):
		return "mcp", nil
	case contains(s.Tags, tagUI):
		return "", fmt.Errorf("**Target** %q has no meaning on a Web UI scenario — a UI step uses app_url", s.Target)
	}
	if len(s.Layers) == 0 {
		return "", fmt.Errorf("**Target** %q needs a **Layer** — the word means one thing per layer", s.Target)
	}
	switch terminal := s.Layers[len(s.Layers)-1]; terminal {
	case "HTTP Ingestion", "Error Path", "Rate Limiting", "Permissions", HTTPLoadLayer:
		return "http", nil
	case "Database State":
		return "database", nil
	case "Message Flow", AMQPLoadLayer:
		return "message_broker", nil
	case "Web UI":
		return "", fmt.Errorf("**Target** %q has no meaning on a Web UI scenario — a UI step uses app_url", s.Target)
	case "External Delivery":
		return "", fmt.Errorf("**Target** %q has no meaning on an External Delivery scenario — targets.external has no named form", s.Target)
	default:
		return "", fmt.Errorf("**Target** %q has no meaning on layer %q", s.Target, terminal)
	}
}

// metaKeyRe finds every `**Key**:` in a `## Metadata` block — including keys nobody defined.
// ⛔ It deliberately does NOT know the closed list: knowing it here would let the parser skip an
// unknown key, and skipping is exactly what made `**Owner**: me` invisible and a misspelt
// `**Layr**` silently empty its field.
var metaKeyRe = regexp.MustCompile(`(?m)^\s*[-*]?\s*\*\*([^*]+)\*\*\s*:`)

// metaKeysIn lists the `**Key**` names an author declared, in file order.
func metaKeysIn(meta string) []string {
	var out []string
	for _, m := range metaKeyRe.FindAllStringSubmatch(meta, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}
