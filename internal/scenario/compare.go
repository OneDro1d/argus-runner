package scenario

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// compare.go -- the `## COMPARE` section (ARGUS-CMP-2).
//
// The section says what "the same output" means for a check, BEFORE any run: which parts of the
// response are compared, which fields are ignored, which arrays are unordered, how many runs, what
// share must agree. It rides in the scenario body, so the set hash seals it.
//
// ONE READER. internal/compare.BuildRules reads the lines; Parse keeps Scenario.Compare only when
// there is no problem, Validate turns every problem into a line-level error, and an executor in
// compare mode refuses a check with a problem. They cannot disagree about what the block means.
// Outside compare mode the section is inert on purpose: a build, final or scheduled run judges the
// check by its EXPECT exactly as before.

var (
	compareCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	compareKeyLineRe = regexp.MustCompile(`^\s*[-*]?\s*\*\*([^*]+)\*\*\s*:\s*(.*?)\s*$`)
)

// parseCompareRaw reads the `**Key**: value` lines of the section whose heading is on headingLine
// (1-based). Lines carry their absolute file line. HTML comments are removed (a template may show a
// hint beside each key) with their newlines kept, so line numbers stay true. A line that is neither
// blank nor a key line is returned as a problem, never skipped.
func parseCompareRaw(text string, headingLine int) ([]compare.RawKV, []compare.Problem) {
	lines := strings.Split(text, "\n")
	var body []string
	for i := headingLine; i < len(lines); i++ { // headingLine is 1-based, so index headingLine is the line after it
		if headingRe.MatchString(lines[i]) {
			break
		}
		body = append(body, lines[i])
	}
	stripped := compareCommentRe.ReplaceAllStringFunc(strings.Join(body, "\n"), func(c string) string {
		return strings.Repeat("\n", strings.Count(c, "\n"))
	})
	var kvs []compare.RawKV
	var probs []compare.Problem
	for i, l := range strings.Split(stripped, "\n") {
		line := headingLine + 1 + i
		if strings.TrimSpace(l) == "" {
			continue
		}
		if m := compareKeyLineRe.FindStringSubmatch(l); m != nil {
			kvs = append(kvs, compare.RawKV{Key: strings.TrimSpace(m[1]), Value: m[2], Line: line})
			continue
		}
		shown := strings.TrimSpace(l)
		if len(shown) > 60 {
			shown = shown[:60] + "..."
		}
		probs = append(probs, compare.Problem{Line: line,
			Msg: fmt.Sprintf("has a line that is not a `**Key**: value` line: %q", shown)})
	}
	return kvs, probs
}

// buildCompare is the ONE reader of the section.
func buildCompare(text string, headingLine int) (*compare.Rules, []compare.Problem) {
	kvs, stray := parseCompareRaw(text, headingLine)
	rules, probs := compare.BuildRules(kvs)
	all := append(stray, probs...)
	if len(all) > 0 {
		return nil, all
	}
	return rules, nil
}

// compareErrors is every rule of the section, as line-level errors.
func compareErrors(s *Scenario, text string, metaLine int) []Error {
	if !s.CompareDeclared {
		return nil
	}
	head := s.sectionStart("COMPARE", metaLine)
	var errs []Error
	add := func(line int, format string, a ...any) {
		if line <= 0 {
			line = head
		}
		errs = append(errs, Error{Line: line, Message: "## COMPARE " + fmt.Sprintf(format, a...)})
	}
	for _, p := range s.compareProblems {
		msg := p.Msg
		if p.UnknownKey {
			if near := NearestName(p.Key, compare.Keys()); near != "" {
				msg += fmt.Sprintf(". Did you mean `**%s**`? A misspelt key is refused here rather than left to do nothing", near)
			}
		}
		add(p.Line, "%s", msg)
	}
	r := s.Compare
	if r == nil {
		return errs
	}
	keyLine := func(key string) int {
		for i, l := range strings.Split(text, "\n") {
			if i+1 > head && strings.Contains(l, "**"+key+"**") {
				return i + 1
			}
		}
		return head
	}

	// the layer or engine must record an output at all: a comparison that could never be measured is
	// refused here, naming the layer, instead of reading "not measured" for ever
	if r.Reference == compare.RefMeasured {
		steps, stepsOK := chainStepsOf(s)
		switch {
		case contains(s.Tags, ChainTag):
			if stepsOK && !anyHTTPStep(steps) {
				add(keyLine("Reference"), "**Reference**: measured needs a check whose output is recorded, and this chain has no `http` step: only `http` steps record an output yet")
			}
		case NativeEngineTag(s) != "":
			add(keyLine("Reference"), "**Reference**: measured needs a check whose output is recorded, and the `%s` engine records no output yet (supported: the http engine on HTTP Ingestion, Error Path, Rate Limiting and Permissions, and chain `http` steps)", NativeEngineTag(s))
		case !IsStatusLayer(PrimaryLayer(s)):
			add(keyLine("Reference"), "**Reference**: measured needs a check whose output is recorded, and layer `%s` records no output yet (supported: HTTP Ingestion, Error Path, Rate Limiting and Permissions on the http engine, and chain `http` steps)", PrimaryLayer(s))
		}
	}
	if len(r.Steps) > 0 {
		steps, stepsOK := chainStepsOf(s)
		switch {
		case !contains(s.Tags, ChainTag):
			add(keyLine("Steps"), "**Steps** names chain steps, and this scenario is not a chain")
		case stepsOK:
			byName := map[string]ChainStep{}
			var all []string
			for _, st := range steps {
				byName[st.Name] = st
				all = append(all, st.Name)
			}
			for _, n := range r.Steps {
				st, ok := byName[n]
				switch {
				case !ok:
					add(keyLine("Steps"), "**Steps** names step `%s`, which the chain does not declare (steps: %s)", n, strings.Join(all, ", "))
				case st.Type != "http":
					add(keyLine("Steps"), "**Steps** names step `%s`, which is not an `http` step: only `http` steps record an output yet", n)
				}
			}
		}
	}
	if len(r.NotWorseThan) > 0 {
		switch {
		case isAMQPLoad(s):
			add(keyLine("Not Worse Than"), "**Not Worse Than** is not read on an `AMQP Load` scenario: its ramp has its own verdict")
		case IsHTTPLoad(s):
			add(keyLine("Not Worse Than"), "**Not Worse Than** is not read on an `HTTP Load` scenario: its ramp has its own verdict")
		case !s.LoadDeclared:
			add(keyLine("Not Worse Than"), "**Not Worse Than** needs a `## LOAD` section: there are no load numbers to compare without one")
		}
	}
	return errs
}

func chainStepsOf(s *Scenario) ([]ChainStep, bool) {
	if !contains(s.Tags, ChainTag) {
		return nil, false
	}
	steps, err := ParseChainSteps(s.Trigger.Payload)
	return steps, err == nil
}

func anyHTTPStep(steps []ChainStep) bool {
	for _, st := range steps {
		if st.Type == "http" {
			return true
		}
	}
	return false
}

// compareWarnings are the advisory lints of a valid section.
func compareWarnings(s *Scenario) []string {
	if s == nil || s.Compare == nil {
		return nil
	}
	return compare.Warnings(s.Compare)
}
