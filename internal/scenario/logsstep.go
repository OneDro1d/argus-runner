package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// THE "logs" CHAIN STEP: its authoring-time shape rules (door A), shared with the executor's
// preflight (door B).
//
// A logs step COUNTS the lines of the Loki this executor is already configured with and judges `body …` claims on
// the count, so a chain can say "after this request the service logged X" (`body has count >= 1`) and "nothing
// logged an ERROR for this run" (`body has count == 0`). What it may read is decided by the OPERATOR (the Loki the
// executor was started with); what this file refuses is a step whose SHAPE could not be run, or whose selector
// could be widened by a value the author did not write.
//
// ⛔ ONE FUNCTION PER RULE, TWO DOORS, exactly as dbstep.go: LogsStepProblems is called by Validate (authoring) and
// by the executor's logsPreflight with the same arguments, so a file that reached a run without the validator is
// refused with the same sentence. The claim grammar is dbstep.go's bodyOnlyStepClaims (one parser, two wordings)
// and the poll rules are ChainPollProblems and stepPollInterval (shared).

// Bounds of a logs step.
const (
	LogsSettleDefault = 2 * time.Second
	LogsSettleMax     = 60 * time.Second
	LogsWindowMax     = time.Hour
	// LogsLookBack is how far BEFORE the chain started the default window opens.
	LogsLookBack = time.Minute
	// LogsLinesKept is how many lines (in time order) the claims' document carries; `count` is always the full number.
	LogsLinesKept = 20
	// LogsLineMaxBytes cuts each kept line, on a rune boundary.
	LogsLineMaxBytes = 2048
)

// logsAllowedKeys are the wire keys a logs step may carry. Anything else (the fields of another step kind
// included: method, url, query, target, …) is stray and refused by name.
var logsAllowedKeys = map[string]bool{
	"type": true, "name": true, "scope": true, "labels": true, "contains": true, "matching": true,
	"window": true, "settle": true, "save": true, "poll": true, "always": true,
}

// LogsScopes are the two values of `scope`.
const (
	LogsScopeCorrelation = "correlation"
	LogsScopeSource      = "source"
)

// logsLabelNameRe is a Loki label name.
var logsLabelNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LogsStepLabels reads a logs step's `labels`: a JSON object of label name to string value (nil when absent).
// Anything else is refused, naming the offending label.
func LogsStepLabels(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("`labels` must be a JSON object of label name to value (e.g. {\"app\": \"orders\"})")
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, fmt.Errorf("`labels` value of %q is not a string - a label value is a literal, matched by equality", k)
		}
		out[k] = s
	}
	return out, nil
}

// LogsStepWindow reads `window`: 0 (and no sentence) when absent, which means "back to one minute before the chain
// started"; else a positive Go duration of at most LogsWindowMax.
func LogsStepWindow(s string) (time.Duration, string) {
	if strings.TrimSpace(s) == "" {
		return 0, ""
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Sprintf("`window` %q is not a positive Go duration (\"10m\", \"90s\")", s)
	}
	if d > LogsWindowMax {
		return 0, fmt.Sprintf("`window` of %s is more than the %s bound on a logs step", d, LogsWindowMax)
	}
	return d, ""
}

// LogsStepSettle reads `settle`: LogsSettleDefault when absent, else a Go duration within 0..LogsSettleMax.
func LogsStepSettle(s string) (time.Duration, string) {
	if strings.TrimSpace(s) == "" {
		return LogsSettleDefault, ""
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Sprintf("`settle` %q is not a Go duration of zero or more (\"2s\", \"500ms\")", s)
	}
	if d > LogsSettleMax {
		return 0, fmt.Sprintf("`settle` of %s is more than the %s bound on a logs step", d, LogsSettleMax)
	}
	return d, ""
}

// LogsStepPollInterval is the interval a logs step's poll sleeps between attempts (one second when none is
// declared); the sentence is non-empty when it is under a second.
func LogsStepPollInterval(p *ChainPoll) (time.Duration, string) {
	return stepPollInterval(p, "the log store")
}

// LogsStepContainsPlaceholderProblems judges the `${…}` in `contains`: the run's own id (${cid},
// ${correlation_id}) and ${saved.<var>} are filled in (always through the LogQL quoting function, never spliced
// raw); anything else - an environment variable included - is refused as one nothing fills in.
func LogsStepContainsPlaceholderProblems(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range checkPlaceholderRe.FindAllString(text, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		if contains(CorrelationIDPlaceholders, m) || savedRefWholeRe.MatchString(m) {
			continue
		}
		out = append(out, fmt.Sprintf("`contains` carries `%s`, which nothing fills in - only the run's own id "+
			"(${correlation_id}, ${cid}) and ${saved.<var>} are placed in a logs step's `contains`", m))
	}
	return out
}

// LogsStepProblems lists everything wrong with ONE logs step's shape, as short sentences without the step name.
// raw is the step's wire keys (the stray-field rule and "present but empty"; nil skips them). st is the step AS
// AUTHORED: at the executor's door that is the unsubstituted payload's, so no environment value is in it.
func LogsStepProblems(st ChainStep, raw map[string]json.RawMessage) []string {
	var out []string
	if raw != nil {
		var extra []string
		for k := range raw {
			if !logsAllowedKeys[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, fmt.Sprintf("this logs step carries the field %q, which a logs step does not accept "+
				"(a logs step takes: scope, labels, contains, matching, window, settle, save, poll, always)", k))
		}
	}
	present := func(k string) bool {
		if raw == nil {
			return false
		}
		_, ok := raw[k]
		return ok
	}
	hasLabels := len(st.Labels) > 0 && strings.TrimSpace(string(st.Labels)) != "null"
	switch st.Scope {
	case LogsScopeCorrelation:
		if hasLabels {
			out = append(out, "this logs step has scope \"correlation\" and carries `labels` - a correlation read selects its "+
				"streams itself (every stream but the observability plane's own); to select streams by label use scope \"source\"")
		}
	case LogsScopeSource:
		labels, lerr := LogsStepLabels(st.Labels)
		switch {
		case lerr != nil:
			out = append(out, lerr.Error())
		case len(labels) == 0:
			out = append(out, "this logs step has scope \"source\" and no `labels` - it needs at least one label to select the "+
				"streams to read (e.g. \"labels\": {\"app\": \"orders\"})")
		}
		names := make([]string, 0, len(labels))
		for k := range labels {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			v := labels[k]
			switch {
			case !logsLabelNameRe.MatchString(k):
				out = append(out, fmt.Sprintf("`labels` name %q is not a label name (letters, digits and _, not starting with a digit)", k))
			case v == "":
				out = append(out, fmt.Sprintf("`labels` value of %q is empty - an empty value matches every stream WITHOUT that label", k))
			case strings.Contains(v, "${"):
				out = append(out, fmt.Sprintf("`labels` value of %q carries `${…}` - a label is a literal; only `contains` takes "+
					"${correlation_id}, ${cid} and ${saved.<var>}", k))
			}
		}
		if st.Contains == "" && st.Matching == "" {
			out = append(out, "this logs step has scope \"source\" and neither `contains` nor `matching` - a step may not count "+
				"a whole stream; add the line text to look for")
		}
	case "":
		out = append(out, "this logs step declares no `scope` - it must be \"correlation\" (lines carrying this run's "+
			"correlation id) or \"source\" (the streams `labels` select)")
	default:
		out = append(out, fmt.Sprintf("this logs step has scope %q - it must be \"correlation\" or \"source\"", st.Scope))
	}
	if present("contains") && st.Contains == "" {
		out = append(out, "`contains` is empty - it would match every line; leave it out or write the text to look for")
	}
	out = append(out, LogsStepContainsPlaceholderProblems(st.Contains)...)
	if present("matching") && st.Matching == "" {
		out = append(out, "`matching` is empty - it would match every line; leave it out or write the pattern")
	}
	if st.Matching != "" {
		if strings.Contains(st.Matching, "${") {
			out = append(out, "`matching` carries `${…}` - a pattern is a literal RE2; only `contains` takes "+
				"${correlation_id}, ${cid} and ${saved.<var>}")
		} else if _, err := regexp.Compile(st.Matching); err != nil {
			out = append(out, fmt.Sprintf("`matching` %q does not compile as an RE2 pattern: %v", st.Matching, err))
		}
	}
	if _, why := LogsStepWindow(st.Window); why != "" {
		out = append(out, why)
	}
	if _, why := LogsStepSettle(st.Settle); why != "" {
		out = append(out, why)
	}
	for _, why := range ChainPollProblems(st.Poll) {
		out = append(out, "this logs step "+why)
	}
	if _, why := LogsStepPollInterval(st.Poll); why != "" {
		out = append(out, "this logs step "+why)
	}
	return out
}

// LogsStepClaims parses a logs step's claims: only the `body …` forms, by the existing parser, on the document
// {"count": N, "lines": ["…"]}. `status=…`, `elapsed`, `unreachable`, `broker …` and anything else are refused BY NAME.
func LogsStepClaims(claims []string) ([]BodyAssert, error) {
	return bodyOnlyStepClaims(bodyOnlyWording{
		step: "logs", judged: "the count", document: `{"count": N, "lines": ["…"]}`,
		statusEg: "body has count >= 1",
		examples: "`body has count >= 1`, `body has count == 0`, `body has lines.0 containing timeout`",
	}, claims)
}

// logsClaimsNotJudgedElsewhere are the claims of a logs step that another validator does not already refuse by
// name at authoring (elapsedStepErrors, unreachableStepErrors, amqpStepErrors' broker rule do), so door A does not
// say the same thing twice. Door B has no such validator and calls LogsStepClaims with everything.
func logsClaimsNotJudgedElsewhere(claims []string) []string {
	return dbClaimsNotJudgedElsewhere(claims)
}

// logsStepErrors is the validator's door for logs steps (door A): the shape (LogsStepProblems) and the claim
// grammar (LogsStepClaims).
func logsStepErrors(steps []ChainStep, rawSteps []map[string]json.RawMessage, claims map[string][]string, trigLine, expLine int) []Error {
	var errs []Error
	for i, st := range steps {
		if st.Type != "logs" {
			continue
		}
		var raw map[string]json.RawMessage
		if i < len(rawSteps) {
			raw = rawSteps[i]
		}
		for _, p := range LogsStepProblems(st, raw) {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, p)})
		}
		if _, err := LogsStepClaims(logsClaimsNotJudgedElsewhere(claims[st.Name])); err != nil {
			errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf("step %q: %s", st.Name, err.Error())})
		}
	}
	return errs
}

// HasLogsStep reports whether the chain carries any logs step.
func HasLogsStep(steps []ChainStep) bool {
	for _, st := range steps {
		if st.Type == "logs" {
			return true
		}
	}
	return false
}
