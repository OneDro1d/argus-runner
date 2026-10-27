package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// THE "db" CHAIN STEP: its authoring-time shape rules (door A), shared with the executor's
// preflight (door B).
//
// A db step runs ONE read-only SELECT against a PostgreSQL database target declared in argus-config.yaml and
// judges `body …` claims on the rows, so a chain can "POST, then see the row". What it may read is decided by
// the OPERATOR (the target and its role); what this file refuses is a step whose SHAPE could not be run, or
// whose query could write, or would put a saved value into SQL text.
//
// ⛔ ONE FUNCTION PER RULE, TWO DOORS. DBStepProblems is called by Validate (authoring) and by the executor's
// dbPreflight with the same arguments, so a file that reached a run without the validator is refused with the
// same sentence. The SQL rules themselves are NOT here: they are verifysql.go's sqlCheck, the implementation a
// `## VERIFY` has always used.

// ChainStepTypes are the step types a chain may carry, in the order the docs list them. The ONE list: the
// authoring validator and the executor's preflight both refuse a type outside it (UnknownStepTypeProblem).
var ChainStepTypes = []string{"mcp", "ui", "http", "amqp", "fault", "db", "logs", "bus"}

// UnknownStepTypeProblem is the ONE sentence for a step whose `type` is none of ChainStepTypes. "" when the type
// is known. The executor's preflight has always refused an unknown type by this lead; door A learned to in
// With this function, so the two cannot word it differently.
func UnknownStepTypeProblem(typ string) string {
	if contains(ChainStepTypes, typ) {
		return ""
	}
	return "unknown chain step type " + strconv.Quote(typ) + " (one of " + strings.Join(ChainStepTypes, ", ") + ")"
}

// Bounds of a db step.
const (
	DBTimeoutDefault   = 5 * time.Second
	DBTimeoutMax       = 30 * time.Second
	DBMaxRowsDefault   = 100
	DBMaxRowsMin       = 1
	DBMaxRowsMax       = 1000
	DBPollIntervalMin  = time.Second
	DBPollIntervalBase = time.Second // used when a db step's poll names no interval
	// DBDocumentMaxBytes caps the one JSON document a db step's claims are judged against, as an http body is.
	DBDocumentMaxBytes = 1 << 20
)

// dbAllowedKeys are the wire keys a db step may carry. Anything else (the fields of another step kind
// included: method, url, tool, op, …) is stray and refused by name.
var dbAllowedKeys = map[string]bool{
	"type": true, "name": true, "query": true, "params": true, "target": true, "timeout": true,
	"max_rows": true, "save": true, "poll": true, "always": true,
}

var dbParamRefRe = regexp.MustCompile(`\$(\d+)`)

// DBStepParams reads a db step's `params`: a JSON array of strings (nil when absent). Anything else is refused,
// naming the offending element.
func DBStepParams(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("`params` must be a JSON array of strings (each is sent as bind parameter $1, $2, …)")
	}
	out := make([]string, 0, len(elems))
	for i, e := range elems {
		var s string
		if json.Unmarshal(e, &s) != nil {
			return nil, fmt.Errorf("`params` element %d is not a string - every bind parameter is sent as text and the "+
				"server casts it, so write \"42\" for the number 42", i+1)
		}
		out = append(out, s)
	}
	return out, nil
}

// DBStepTimeout reads `timeout`: the Go duration, DBTimeoutDefault when absent. ok false with the sentence when
// it is not a positive duration within DBTimeoutMax.
func DBStepTimeout(s string) (time.Duration, string) {
	if strings.TrimSpace(s) == "" {
		return DBTimeoutDefault, ""
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Sprintf("`timeout` %q is not a positive Go duration (\"5s\", \"500ms\")", s)
	}
	if d > DBTimeoutMax {
		return 0, fmt.Sprintf("`timeout` of %s is more than the %s bound on a db step", d, DBTimeoutMax)
	}
	return d, ""
}

// DBStepMaxRows reads `max_rows`: DBMaxRowsDefault when absent, else DBMaxRowsMin..DBMaxRowsMax.
func DBStepMaxRows(p *int) (int, string) {
	if p == nil {
		return DBMaxRowsDefault, ""
	}
	if *p < DBMaxRowsMin || *p > DBMaxRowsMax {
		return 0, fmt.Sprintf("`max_rows` is %d, outside %d..%d", *p, DBMaxRowsMin, DBMaxRowsMax)
	}
	return *p, ""
}

// ChainPollProblems judges a step's `poll` block, shared by the http step and the db step (the sentences are
// the http step's, unchanged): a timeout that is a Go duration of at most 10m, an interval that is one when
// given. Each sentence leads with a backquoted field and carries no step name.
func ChainPollProblems(p *ChainPoll) []string {
	var out []string
	if p == nil {
		return nil
	}
	if to, terr := time.ParseDuration(p.Timeout); terr != nil {
		out = append(out, fmt.Sprintf("declares `poll.timeout` %q, which is not a valid "+
			"duration (Go syntax: \"180s\", \"3m\") (AC-D20)", p.Timeout))
	} else if to > 10*time.Minute {
		out = append(out, fmt.Sprintf("declares `poll.timeout` of %s, which is more than the "+
			"10m bound on a chain step's poll (AC-D20)", to))
	}
	if p.Interval != "" {
		if _, ierr := time.ParseDuration(p.Interval); ierr != nil {
			out = append(out, fmt.Sprintf("declares `poll.interval` %q, which is not a valid "+
				"duration (Go syntax: \"5s\") (AC-D20)", p.Interval))
		}
	}
	return out
}

// DBStepPollInterval is the interval a db step's poll sleeps between attempts: the declared one, DBPollIntervalBase
// when none is declared. The sentence is non-empty when it is under DBPollIntervalMin.
func DBStepPollInterval(p *ChainPoll) (time.Duration, string) {
	return stepPollInterval(p, "a database")
}

// stepPollInterval is the poll-interval rule the db step and the logs step share: the declared interval, one
// second when none is declared, and a sentence when it is under a second. target names what must not be hammered
// ("a database", "the log store").
func stepPollInterval(p *ChainPoll, target string) (time.Duration, string) {
	if p == nil || p.Interval == "" {
		return DBPollIntervalBase, ""
	}
	d, err := time.ParseDuration(p.Interval)
	if err != nil {
		return 0, "" // ChainPollProblems says so
	}
	if d < DBPollIntervalMin {
		return d, fmt.Sprintf("declares `poll.interval` %q, which is under %s - a check must not hammer %s; "+
			"poll no faster than once a second", p.Interval, DBPollIntervalMin, target)
	}
	return d, ""
}

// DBStepProblems lists everything wrong with ONE db step's shape, as short sentences without the step name.
// raw is the step's wire keys (the stray-field rule; nil skips it). full true is the authoring door: every SQL
// rule of the shared implementation; false is the executor's preflight: only the read-only subset
// (DBStepReadOnlyViolations), because a MEASURING rule must not turn a live check into an error at run time.
// query is the text AS AUTHORED (door B passes the unsubstituted payload's, so no environment value is in it).
func DBStepProblems(st ChainStep, query string, raw map[string]json.RawMessage, full bool) []string {
	var out []string
	if raw != nil {
		var extra []string
		for k := range raw {
			if !dbAllowedKeys[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, fmt.Sprintf("this db step carries the field %q, which a db step does not accept "+
				"(a db step takes: query, params, target, timeout, max_rows, save, poll, always)", k))
		}
	}
	if strings.TrimSpace(query) == "" {
		out = append(out, "this db step declares no `query` - it needs one read-only SELECT")
	} else {
		if full {
			out = append(out, DBStepQueryErrors(query)...)
		} else {
			out = append(out, DBStepReadOnlyViolations(query)...)
		}
		out = append(out, DBStepQueryPlaceholderProblems(query)...)
	}
	params, perr := DBStepParams(st.Params)
	if perr != nil {
		out = append(out, perr.Error())
	}
	for i, p := range params {
		for _, ph := range UnfilledPlaceholders(p, true) {
			out = append(out, fmt.Sprintf("`params` element %d carries `%s`, which nothing fills in - a param may carry "+
				"${correlation_id}, ${cid}, ${cid8} and ${saved.<var>}", i+1, ph))
		}
	}
	if perr == nil && strings.TrimSpace(query) != "" {
		if why := DBStepParamCountProblem(query, len(params)); why != "" {
			out = append(out, why)
		}
	}
	if _, why := DBStepTimeout(st.Timeout); why != "" {
		out = append(out, why)
	}
	if _, why := DBStepMaxRows(st.MaxRows); why != "" {
		out = append(out, why)
	}
	for _, why := range ChainPollProblems(st.Poll) {
		out = append(out, "this db step "+why)
	}
	if _, why := DBStepPollInterval(st.Poll); why != "" {
		out = append(out, "this db step "+why)
	}
	return out
}

// DBStepParamCountProblem is the rule that the numbered placeholders of the (masked) query are exactly $1..$n
// for the n params the step passes: "" when they are, else the sentence naming both numbers.
func DBStepParamCountProblem(query string, nParams int) string {
	used := map[int]bool{}
	for _, m := range dbParamRefRe.FindAllStringSubmatch(maskSQL(strings.TrimSpace(query)), -1) {
		n, _ := strconv.Atoi(m[1])
		used[n] = true
	}
	highest := 0
	for n := range used {
		if n > highest {
			highest = n
		}
	}
	if highest == nParams && len(used) == nParams {
		return ""
	}
	return fmt.Sprintf("the db step's query uses %d numbered placeholder(s) (highest $%d) but `params` passes %d "+
		"value(s) - the query must use exactly $1..$%d, each at least once", len(used), highest, nParams, nParams)
}

// DBStepClaims parses a db step's claims: only the `body …` forms, by the existing parser. `status=…`,
// `elapsed`, `unreachable`, `broker …` and anything else are refused BY NAME - never read as "expect success".
func DBStepClaims(claims []string) ([]BodyAssert, error) {
	return bodyOnlyStepClaims(bodyOnlyWording{
		step: "db", judged: "the rows", document: `{"row_count": N, "rows": [{…}]}`,
		statusEg: "body has row_count >= 1",
		examples: "`body has row_count == 1`, `body has rows.0.status equals PAID`",
	}, claims)
}

// bodyOnlyWording is what differs between the steps whose ONLY claim grammar is the `body …` forms on one JSON
// document (the db step and the logs step): the step's name, what the claims judge, the document and examples.
type bodyOnlyWording struct {
	step, judged, document, statusEg, examples string
}

// bodyOnlyStepClaims is the ONE claim parser of such a step: `body …` forms by ParseBodyAsserts, every other
// claim family refused by name with a sentence that sends the author to the right form.
func bodyOnlyStepClaims(w bodyOnlyWording, claims []string) ([]BodyAssert, error) {
	for _, c := range claims {
		switch {
		case ClaimsToBeBodyAssert(c):
		case ClaimsToBeStatus(c):
			return nil, fmt.Errorf("EXPECT bullet %q is a status claim - a %s step has no HTTP status; judge %s with a "+
				"`body …` claim (e.g. `%s`)", c, w.step, w.judged, w.statusEg)
		case ClaimsToBeElapsed(c):
			return nil, fmt.Errorf("EXPECT bullet %q is an `elapsed` claim - that is a claim for an http step only; a %s step is "+
				"judged by `body …` claims on %s", c, w.step, w.judged)
		case ClaimsToBeUnreachable(c):
			return nil, fmt.Errorf("EXPECT bullet %q is an `unreachable` claim - that is a claim for an http step only; a %s step "+
				"is judged by `body …` claims on %s", c, w.step, w.judged)
		case ClaimsToBeBrokerClaim(c):
			return nil, fmt.Errorf("EXPECT bullet %q is a broker claim - that belongs to an `amqp` step; a %s step is judged by "+
				"`body …` claims on %s", c, w.step, w.judged)
		default:
			return nil, fmt.Errorf("EXPECT bullet %q is not a recognised %s-step assertion - write a `body …` claim on the "+
				"document %s (e.g. %s)", c, w.step, w.document, w.examples)
		}
	}
	asserts, berrs := ParseBodyAsserts(claims)
	if len(berrs) > 0 {
		return nil, berrs[0]
	}
	return asserts, nil
}

// dbClaimsJudgedElsewhere are the claims of a db step that another validator already refuses by name at
// authoring (elapsedStepErrors, unreachableStepErrors, amqpStepErrors' broker rule), so door A does not say
// the same thing twice. Door B has no such validator and calls DBStepClaims with everything.
func dbClaimsNotJudgedElsewhere(claims []string) []string {
	var out []string
	for _, c := range claims {
		if ClaimsToBeElapsed(c) || ClaimsToBeUnreachable(c) || ClaimsToBeBrokerClaim(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// dbStepErrors is the validator's door for db steps (door A): the shape (DBStepProblems, full), the claim
// grammar (DBStepClaims), and the unknown-type refusal for every step.
func dbStepErrors(steps []ChainStep, rawSteps []map[string]json.RawMessage, claims map[string][]string, trigLine, expLine int) []Error {
	var errs []Error
	for _, st := range steps {
		if why := UnknownStepTypeProblem(st.Type); why != "" {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, why)})
		}
	}
	for i, st := range steps {
		if st.Type != "db" {
			continue
		}
		var raw map[string]json.RawMessage
		if i < len(rawSteps) {
			raw = rawSteps[i]
		}
		for _, p := range DBStepProblems(st, st.Query, raw, true) {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, p)})
		}
		if _, err := DBStepClaims(dbClaimsNotJudgedElsewhere(claims[st.Name])); err != nil {
			errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf("step %q: %s", st.Name, err.Error())})
		}
	}
	return errs
}

// HasDBStep reports whether the chain carries any db step.
func HasDBStep(steps []ChainStep) bool {
	for _, st := range steps {
		if st.Type == "db" {
			return true
		}
	}
	return false
}
