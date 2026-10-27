package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// THE "bus" CHAIN STEP: its authoring-time shape rules (door A), shared with the executor's
// preflight (door B).
//
// A bus step is an AMQP SUBSCRIPTION used as evidence, next to the Loki `logs` step. Before the chain's first step
// fires the executor binds a private queue to the step's binding (an exchange + routing key, or a named `stream` of
// the broker target) and keeps one consumer on it; the step is judged where it stands, by `body …` claims on ONE
// JSON document:
//
//	{"count":N,"correlated":N,"seen":N,"uncorrelated":N,"undecodable":N,"messages":[{…}]}
//
// What may be read is decided by the OPERATOR (the broker target, its grant, its `envelope` and `streams`); what this
// file refuses is a step whose SHAPE could not be run, a binding wide enough to carry the whole bus, and a claim set
// that could pass because nothing was heard.
//
// ⛔ ONE FUNCTION PER RULE, TWO DOORS, exactly as logsstep.go: BusStepProblems and BusStepClaims are called by Validate
// (authoring) and by the executor's busPreflight with the same arguments, so a file that reached a run without the
// validator is refused with the same sentence.

// Bounds of a bus step.
const (
	BusWaitDefault = 10 * time.Second
	BusWaitMin     = time.Second
	BusWaitMax     = 60 * time.Second
	// BusMessagesKept is how many matching messages (in arrival order) the claims' document carries; `count` is always
	// the full number.
	BusMessagesKept = 20
	// BusSeenMax is the number of deliveries one subscription may see before the step is `error` ("binding too wide"):
	// a count taken over a firehose is not a measurement of this run.
	BusSeenMax = 20000
	// BusCorrelatedMax is the number of correlated messages a subscription keeps to judge `where` and the claims
	// against; past it the step is `error`, never a count that is a silent lower bound.
	BusCorrelatedMax = 2000
	// BusMessageMaxBytes cuts each kept message's body when it is placed in the document, on a rune boundary.
	BusMessageMaxBytes = 4096
	// BusPrefetch is the consumer's Qos prefetch.
	BusPrefetch = 256
)

// busAllowedKeys are the wire keys a bus step may carry. `poll` and `always` get their own sentence; anything else
// (the fields of another step kind included: url, method, query, scope, …) is stray and refused by name.
var busAllowedKeys = map[string]bool{
	"type": true, "name": true, "target": true, "exchange": true, "routing_key": true, "stream": true,
	"where": true, "wait": true, "save": true,
}

var (
	busNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	busPathRe  = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	busCounter = map[string]bool{"count": true, "correlated": true, "seen": true, "uncorrelated": true, "undecodable": true}
)

// BusStepWait reads `wait`: BusWaitDefault when absent, else a Go duration within BusWaitMin..BusWaitMax.
func BusStepWait(s string) (time.Duration, string) {
	if strings.TrimSpace(s) == "" {
		return BusWaitDefault, ""
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Sprintf("`wait` %q is not a Go duration (\"10s\", \"500ms\")", s)
	}
	if d < BusWaitMin || d > BusWaitMax {
		return 0, fmt.Sprintf("`wait` of %s is outside the %s..%s bounds of a bus step", d, BusWaitMin, BusWaitMax)
	}
	return d, ""
}

// BusStepWhere reads a bus step's `where`: a JSON object of body path -> string or array of strings (any-of), nil
// when absent. Anything else is refused, naming the offending path.
func BusStepWhere(raw json.RawMessage) (map[string][]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("`where` must be a JSON object of body path to value (e.g. {\"level\": [\"error\", \"fatal\"]})")
	}
	out := make(map[string][]string, len(obj))
	for k, v := range obj {
		var one string
		if json.Unmarshal(v, &one) == nil {
			out[k] = []string{one}
			continue
		}
		var many []string
		if json.Unmarshal(v, &many) != nil {
			return nil, fmt.Errorf("`where` value of %q is neither a string nor an array of strings - a value is a literal, matched by equality", k)
		}
		out[k] = many
	}
	return out, nil
}

// BusStepWherePlaceholderProblems judges the `${…}` in a `where` value: the run's own id (${cid}, ${correlation_id}) and
// ${saved.<var>} are filled in; anything else - an environment variable included - is refused as one nothing fills in.
func BusStepWherePlaceholderProblems(path, text string) []string {
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
		out = append(out, fmt.Sprintf("`where` value of %q carries `%s`, which nothing fills in - only the run's own id "+
			"(${correlation_id}, ${cid}) and ${saved.<var>} are placed in a bus step's `where`", path, m))
	}
	return out
}

// busBareWildcard reports whether a routing key is only `#` words (`#`, `#.#`): it would bind the whole exchange.
func busBareWildcard(rk string) bool {
	for _, w := range strings.Split(rk, ".") {
		if w != "#" {
			return false
		}
	}
	return rk != ""
}

// BusStepPositionProblem is S7: a bus step may not be the chain's first step. It is judged where it stands, and at
// index 0 nothing has been triggered yet. "" when fine.
func BusStepPositionProblem(index int) string {
	if index == 0 {
		return "this bus step is the chain's first step - it is judged where it stands, and nothing has been triggered " +
			"before it; put the step that causes the traffic first and the bus step after it"
	}
	return ""
}

// BusStepProblems lists everything wrong with ONE bus step's shape, as short sentences without the step name.
// raw is the step's wire keys (the stray-field rule and "present but empty"; nil skips them). st is the step AS
// AUTHORED: at the executor's door that is the unsubstituted payload's, so no environment value is in it.
func BusStepProblems(st ChainStep, raw map[string]json.RawMessage) []string {
	var out []string
	present := func(k string) bool {
		if raw == nil {
			return false
		}
		_, ok := raw[k]
		return ok
	}
	if raw != nil {
		var extra []string
		for k := range raw {
			if !busAllowedKeys[k] && k != "poll" && k != "always" {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, fmt.Sprintf("this bus step carries the field %q, which a bus step does not accept "+
				"(a bus step takes: target, stream or exchange + routing_key, where, wait, save)", k))
		}
	}
	if st.Poll != nil || present("poll") || st.Always || present("always") {
		out = append(out, "this bus step may not carry `poll` or `always` - it waits by itself (`wait`) and creates nothing to clean up")
	}
	hasStream := st.Stream != "" || present("stream")
	hasBinding := st.Exchange != "" || st.RoutingKey != "" || present("exchange") || present("routing_key")
	switch {
	case hasStream && hasBinding:
		out = append(out, "this bus step carries both `stream` and `exchange`/`routing_key` - two answers to one question; "+
			"name a declared `stream`, or write the binding")
	case !hasStream && !hasBinding:
		out = append(out, "this bus step declares no binding - it needs a `stream` (a name under the broker target's `streams`) "+
			"or an `exchange` + `routing_key`")
	case hasStream:
		switch {
		case st.Stream == "":
			out = append(out, "`stream` is empty - name a stream declared under the broker target's `streams`")
		case strings.Contains(st.Stream, "${"):
			out = append(out, "`stream` carries `${…}` - a binding is a literal, not built at run time")
		case !busNameRe.MatchString(st.Stream):
			out = append(out, fmt.Sprintf("`stream` %q is not a name (letters, digits, `.`, `_`, `-`)", st.Stream))
		}
	default:
		switch {
		case st.Exchange == "":
			out = append(out, "`exchange` is empty - the default exchange cannot be subscribed to; name the exchange the traffic is published to")
		case strings.Contains(st.Exchange, "${"):
			out = append(out, "`exchange` carries `${…}` - a binding is a literal, not built at run time")
		}
		switch {
		case st.RoutingKey == "":
			out = append(out, "`routing_key` is missing or empty - a bus step must name a narrow binding (a bare `#` is refused too)")
		case strings.Contains(st.RoutingKey, "${"):
			out = append(out, "`routing_key` carries `${…}` - a binding is a literal, not built at run time")
		case busBareWildcard(st.RoutingKey):
			out = append(out, fmt.Sprintf("`routing_key` %q binds the whole exchange - a bus step must name a narrow binding "+
				"(the service or event it is about)", st.RoutingKey))
		}
	}
	if strings.Contains(st.Target, "${") {
		out = append(out, "`target` carries `${…}` - a target is a name, not built at run time")
	}
	if present("where") {
		where, err := BusStepWhere(st.Where)
		if err != nil {
			out = append(out, err.Error())
		}
		paths := make([]string, 0, len(where))
		for k := range where {
			paths = append(paths, k)
		}
		sort.Strings(paths)
		for _, k := range paths {
			vals := where[k]
			switch {
			case !busPathRe.MatchString(k):
				out = append(out, fmt.Sprintf("`where` path %q is not a body path (letters, digits, `.`, `_`, `-`)", k))
			case len(vals) == 0:
				out = append(out, fmt.Sprintf("`where` value of %q is an empty list - it would match nothing", k))
			}
			for _, v := range vals {
				if v == "" {
					out = append(out, fmt.Sprintf("`where` value of %q is empty - write the value to look for", k))
				}
				out = append(out, BusStepWherePlaceholderProblems(k, v)...)
			}
		}
		if err == nil && len(where) == 0 && present("where") && strings.TrimSpace(string(st.Where)) != "null" {
			out = append(out, "`where` is an empty object - leave it out, or write the paths to filter on")
		}
	}
	if _, why := BusStepWait(st.Wait); why != "" {
		out = append(out, why)
	}
	return out
}

// busSyntheticEmpty is the document of a subscription that heard nothing: every claim that HOLDS on it is an absence
// claim, and needs the anchor (C2).
const busSyntheticEmpty = `{"count":0,"correlated":0,"seen":0,"uncorrelated":0,"undecodable":0,"messages":[]}`

// busListField reports whether a claim's field reads the capped message list ("" = the whole document).
func busListField(f string) bool {
	return f == "" || f == "messages" || strings.HasPrefix(f, "messages.")
}

// busHoldsOnEmpty reports whether a claim holds when the subscription heard nothing. A numeric threshold that is still
// a `${saved.<var>}` cannot be told, so it counts as holding (fail closed).
func busHoldsOnEmpty(a BodyAssert) bool {
	if busCounter[a.Field] && strings.Contains(a.Value, "${") {
		return true
	}
	return !mcp.BodyAssertsMiss(busSyntheticEmpty, []BodyAssert{a})
}

// busIsAnchor reports whether a claim proves the subscription heard THIS run: `correlated >= n` (n >= 1),
// `correlated > n` (n >= 0) or `correlated == n` (n >= 1).
func busIsAnchor(a BodyAssert) bool {
	if a.Field != "correlated" {
		return false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(a.Value), 64)
	if err != nil {
		return false
	}
	switch a.Op {
	case BodyGTE, BodyEQ:
		return n >= 1
	case BodyGT:
		return n >= 0
	}
	return false
}

// BusStepClaims parses a bus step's claims: only the `body …` forms, by the existing parser, on the document
// {"count","correlated","seen","uncorrelated","undecodable","messages"}. `status=…`, `elapsed`, `unreachable`, `broker …`
// and anything else are refused BY NAME, and two bus rules on top:
//
//	C1  a universal or negative claim over the capped message list (`body every messages …`, `body has messages.… not …`,
//	    `body not contains`, `body has no messages.…`) would be judged on the first BusMessagesKept messages only: refused.
//	C2  a claim that holds when NOTHING was heard (`count == 0`, `count < 1`, `undecodable == 0`, …) needs an anchor
//	    claim `correlated >= n` (n >= 1) on the same step, so a wrong binding, or an app that does not publish over
//	    the bus, can never pass.
func BusStepClaims(claims []string) ([]BodyAssert, error) {
	asserts, err := bodyOnlyStepClaims(bodyOnlyWording{
		step: "bus", judged: "the counts and the first messages", document: `{"count": N, "correlated": N, "seen": N, "uncorrelated": N, "undecodable": N, "messages": [{…}]}`,
		statusEg: "body has count >= 1",
		examples: "`body has count >= 1`, `body has count == 0` (with `body has correlated >= 1`), `body has messages.0.body.status equals paid`",
	}, claims)
	if err != nil {
		return nil, err
	}
	for i, a := range asserts {
		c := claimAt(claims, i)
		if a.Every != "" {
			return nil, fmt.Errorf("EXPECT bullet %q is a claim over every message - the document holds only the first %d of `count`, so it "+
				"would be judged on part of the messages; narrow the question with the step's `where` and claim `body has count == 0` "+
				"(with `body has correlated >= 1`) instead", c, BusMessagesKept)
		}
		switch a.Op {
		case BodyNotContains, BodyNotMatches, BodyNotEquals, BodyAbsent:
			if busListField(a.Field) {
				return nil, fmt.Errorf("EXPECT bullet %q is a negative claim over the message list - the document holds only the first %d of "+
					"`count`, so it would be judged on part of the messages; narrow the question with the step's `where` and claim "+
					"`body has count == 0` (with `body has correlated >= 1`) instead", c, BusMessagesKept)
			}
		}
	}
	needsAnchor := false
	anchored := false
	for _, a := range asserts {
		if busHoldsOnEmpty(a) {
			needsAnchor = true
		}
		if busIsAnchor(a) {
			anchored = true
		}
	}
	if needsAnchor && !anchored {
		return nil, fmt.Errorf("a bus step has a claim that holds when nothing was heard (an absence, like `body has count == 0`) and no " +
			"anchor: add `body has correlated >= 1` to the same step - it proves the subscription heard this run's traffic at all, " +
			"so a wrong routing key, or an app that does not publish over the bus, cannot pass")
	}
	return asserts, nil
}

// claimAt returns the bullet behind the i-th parsed body assertion (the parser keeps order and one assertion per bullet
// for every form a bus step accepts); the empty string when it cannot be told.
func claimAt(claims []string, i int) string {
	n := 0
	for _, c := range claims {
		if ClaimsToBeBodyAssert(c) {
			if n == i {
				return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c), "-"))
			}
			n++
		}
	}
	return ""
}

// BusStepEarlyExit reports whether the step may pass BEFORE `wait` ends: only when every claim is monotone, i.e. once
// true it stays true as more messages arrive - a counter `>` / `>=` a threshold, or a positive claim on
// `messages.<i>.…` (message i of the first BusMessagesKept never changes once it is there). Anything else is judged once,
// when `wait` ends.
func BusStepEarlyExit(asserts []BodyAssert) bool {
	if len(asserts) == 0 {
		return false
	}
	for _, a := range asserts {
		if a.Every != "" || a.Extracted {
			return false
		}
		switch {
		case busCounter[a.Field]:
			if a.Op != BodyGT && a.Op != BodyGTE {
				return false
			}
		case strings.HasPrefix(a.Field, "messages."):
			rest := strings.TrimPrefix(a.Field, "messages.")
			idx := rest
			if i := strings.Index(rest, "."); i >= 0 {
				idx = rest[:i]
			}
			if _, err := strconv.Atoi(idx); err != nil {
				return false
			}
			switch a.Op {
			case BodyExists, BodyContains, BodyMatches, BodyEquals:
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}

// busClaimsNotJudgedElsewhere: see logsClaimsNotJudgedElsewhere.
func busClaimsNotJudgedElsewhere(claims []string) []string { return dbClaimsNotJudgedElsewhere(claims) }

// busStepErrors is the validator's door for bus steps (door A): the shape (BusStepProblems), the position (S7) and
// the claim grammar (BusStepClaims).
func busStepErrors(steps []ChainStep, rawSteps []map[string]json.RawMessage, claims map[string][]string, trigLine, expLine int) []Error {
	var errs []Error
	for i, st := range steps {
		if st.Type != "bus" {
			continue
		}
		var raw map[string]json.RawMessage
		if i < len(rawSteps) {
			raw = rawSteps[i]
		}
		for _, p := range BusStepProblems(st, raw) {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, p)})
		}
		if why := BusStepPositionProblem(i); why != "" {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, why)})
		}
		if _, err := BusStepClaims(busClaimsNotJudgedElsewhere(claims[st.Name])); err != nil {
			errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf("step %q: %s", st.Name, err.Error())})
		}
	}
	return errs
}

// HasBusStep reports whether the chain carries any bus step.
func HasBusStep(steps []ChainStep) bool {
	for _, st := range steps {
		if st.Type == "bus" {
			return true
		}
	}
	return false
}

// BusStepAsListen is the `amqp` `listen` step the money guard judges a bus step as: the same exchange and routing key.
// A step bound by `stream` has neither in the file (only the executor's config resolves a stream), so it is NOT judged
// through this: MatchBusStream asks for an `engine: amqp, op: listen, stream: <name>` entry instead.
func BusStepAsListen(st ChainStep) ChainStep {
	return ChainStep{Type: "amqp", Op: "listen", Name: st.Name, Exchange: st.Exchange, RoutingKey: st.RoutingKey}
}
