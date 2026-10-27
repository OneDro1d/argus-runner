package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// WHAT AN `mcp` CHAIN STEP CAN SAY ABOUT AN ANSWER A GATEWAY WRAPS: `extract` (the part of the
// answer the claims and `save` read), `unavailable` (an answer that means "could not check"), `poll` (re-send while
// the system is unavailable or the claims miss) and `idempotent` (the author's statement that sending the call again
// is harmless, which `poll` requires). Door A is the validator (mcpStepErrors); door B is the executor's preflight
// (argus.mcpPreflight). Both ask MCPStepProblems, so a file that reached a run without the validator is refused with
// the same sentence.
//
// An older executor does not know `extract`: it ignores the key. A claim on the extracted document is therefore WRITTEN
// `body extracted <form>` (and a save from it `"from": "extracted"`), which the older executor has no form for: it
// refuses the chain by name at preflight, so the check is never judged on the raw answer (TestExtractedSpelling_…).
// `unavailable` and `poll` need no such door: an older executor ignores them, so an outage answer is judged on its text
// (a step whose body claims are all negative can PASS there) and a poll does not retry. Pair `unavailable` with a
// `body extracted …` claim or a `from: extracted` save (refused by an older executor) or with a positive data claim.
// The docs say "needs executor 0.3.70".

// MCPExtract is an mcp step's `extract` block: {"regex": "<one capture group>"}.
type MCPExtract struct {
	Regex string `json:"regex"`
	// Path (optional, a pointer so `"path": ""` is told apart from no path) is the dot path of a JSON STRING inside the
	// answer's content text; the regex then runs on that string's decoded value. For a gateway whose answer is a JSON
	// object holding the data inside a string field, where a regex over the raw text sees escaped quotes.
	Path *string `json:"path"`
}

// MCPUnavailable is an mcp step's `unavailable` block: {"matching": "<regex>"}.
type MCPUnavailable struct {
	Matching string `json:"matching"`
}

// MCPExtractMaxBytes caps what `extract` captures: the claims are judged on it and `save` reads it, as an http body
// and a db step's document are capped at the same size.
const MCPExtractMaxBytes = 1 << 20

// mcpAllowedKeys are the wire keys an mcp step may carry. Anything else is stray and refused by name. `expect` is
// not listed and not reported here either: validate.go's W1 already refuses it by name, with the way out.
var mcpAllowedKeys = map[string]bool{
	"type": true, "name": true, "transport": true, "target": true, "server_url": true, "tool": true, "args": true,
	"save": true, "always": true, "extract": true, "unavailable": true, "poll": true, "idempotent": true, "sign": true,
}

// mcpAllowedList is the allowed set in the order the docs list it.
const mcpAllowedList = "type, name, transport, target, server_url, tool, args, save, always, extract, unavailable, poll, idempotent, sign"

// MCPExtractProblem is the ONE judgement of an `extract` block: "" when it is usable. The regex follows the rules of a
// `save` regex: it compiles (Go RE2, no flags added - the author writes `(?s)` when the data spans lines) and has
// exactly one capture group, the part to keep.
func MCPExtractProblem(e *MCPExtract) string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Regex) == "" {
		return "declares `extract` without a `regex` - write {\"regex\": \"<one capture group>\"}"
	}
	if e.Path != nil {
		if strings.TrimSpace(*e.Path) == "" {
			return "declares an `extract` with an empty `path` - write the dot path of the string field that holds the data " +
				"(for example {\"path\": \"result\", \"regex\": \"…\"}), or leave `path` out to read the answer's text as it is"
		}
		if strings.Contains(*e.Path, "${saved.") {
			return "declares an `extract` `path` that names a ${saved.…} variable - the path is fixed text, not a value from an earlier step"
		}
	}
	re, err := regexp.Compile(e.Regex)
	if err != nil {
		return fmt.Sprintf("declares an `extract` regex that does not compile (%v)", err)
	}
	if n := re.NumSubexp(); n != 1 {
		return fmt.Sprintf("declares an `extract` regex with %d capture group(s) - it must have exactly one, the part of "+
			"the answer to keep (use `(?:…)` for grouping that should not capture)", n)
	}
	return ""
}

// MCPUnavailableProblem is the ONE judgement of an `unavailable` block: "" when it is usable.
func MCPUnavailableProblem(u *MCPUnavailable) string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.Matching) == "" {
		return "declares `unavailable` without a `matching` - write {\"matching\": \"<regex>\"}; there is no default pattern"
	}
	if _, err := regexp.Compile(u.Matching); err != nil {
		return fmt.Sprintf("declares an `unavailable` regex that does not compile (%v)", err)
	}
	return ""
}

// MCPPollIntervalSentence is the sentence for a poll interval under a second ("" when it is fine).
func mcpPollIntervalSentence(p *ChainPoll) string {
	_, why := stepPollInterval(p, "an MCP tool")
	return why
}

// MCPStepPollInterval is the interval an mcp step's poll sleeps between attempts: the declared one, one second when none is.
func MCPStepPollInterval(p *ChainPoll) time.Duration {
	d, _ := stepPollInterval(p, "an MCP tool")
	return d
}

// MCPStepProblems lists everything wrong with ONE mcp step's NEW shape, as short sentences without the step name:
// the stray fields, `extract`, `unavailable`, and `poll` (its bounds, a claim to poll on, and `idempotent`). raw is
// the step's wire keys (nil skips the stray rule); claims are the step's `## EXPECT` claims.
func MCPStepProblems(st ChainStep, raw map[string]json.RawMessage, claims []string) []string {
	var out []string
	if raw != nil {
		var extra []string
		for k := range raw {
			if !mcpAllowedKeys[k] && k != "expect" {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, fmt.Sprintf("this mcp step carries the field %q, which an mcp step does not accept "+
				"(an mcp step takes: %s)", k, mcpAllowedList))
		}
	}
	for _, why := range MCPSignProblems(st.Sign) {
		out = append(out, "this mcp step: "+why)
	}
	if why := MCPExtractProblem(st.Extract); why != "" {
		out = append(out, "this mcp step "+why)
	}
	// the document `extract` cuts out is read ONLY by a claim written `body extracted …` or a save with
	// "from": "extracted". Both directions are refused by name, so an `extract` is never declared and ignored, and a
	// claim never names a document the step does not have.
	readers := 0
	for _, c := range claims {
		if ClaimIsExtracted(c) {
			readers++
		}
	}
	for _, sp := range st.Save {
		if sp.From == SaveFromExtracted {
			readers++
		}
	}
	if st.Extract == nil && readers > 0 {
		out = append(out, "this mcp step reads the extracted document (a `body extracted …` claim or a save with \"from\": \"extracted\") "+
			"but declares no `extract`, so there is no such document - add an `extract` block, or write the claim as `body …` / drop `from`")
	}
	if st.Extract != nil && readers == 0 {
		out = append(out, "this mcp step declares `extract` but nothing reads it - write a claim as `body extracted …` or a save with "+
			"\"from\": \"extracted\" (a plain `body …` claim and a save without `from` read the answer itself)")
	}
	if why := MCPUnavailableProblem(st.Unavailable); why != "" {
		out = append(out, "this mcp step "+why)
	}
	if st.Poll != nil {
		if len(claims) == 0 {
			out = append(out, "this mcp step declares `poll` but has no claim - a poll re-sends the call until the step's "+
				"claims hold or the answer is no longer unavailable, so with none it would spin for the whole timeout")
		}
		for _, why := range ChainPollProblems(st.Poll) {
			out = append(out, "this mcp step "+why)
		}
		if why := mcpPollIntervalSentence(st.Poll); why != "" {
			out = append(out, "this mcp step "+why)
		}
		if !st.Idempotent {
			out = append(out, "this mcp step declares `poll`, which SENDS THE TOOL CALL AGAIN on every attempt - set "+
				"\"idempotent\": true to state that calling this tool again is harmless (it is your statement, and the "+
				"money-handling allow entry for the tool still decides whether the call may be sent at all)")
		}
	}
	return out
}

// SaveFromProblems refuses a save's "from" on a step that is not an mcp step: only an mcp step has an extracted
// document to read. Shared by the validator and the executor's second door.
func SaveFromProblems(st ChainStep) []string {
	if st.Type == "mcp" {
		return nil
	}
	var out []string
	names := make([]string, 0, len(st.Save))
	for k, sp := range st.Save {
		if sp.From != "" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, fmt.Sprintf("save for variable %q declares \"from\", which only a save of an mcp step that declares `extract` accepts", k))
	}
	return out
}

// mcpStepErrors is the validator's door for mcp steps (door A).
func mcpStepErrors(steps []ChainStep, rawSteps []map[string]json.RawMessage, claims map[string][]string, trigLine int) []Error {
	var errs []Error
	for i, st := range steps {
		if st.Type != "mcp" {
			if st.Sign != nil {
				errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: `sign` is for an mcp step only - only an mcp step signs its request", st.Name)})
			}
			continue
		}
		var raw map[string]json.RawMessage
		if i < len(rawSteps) {
			raw = rawSteps[i]
		}
		if rs, ok := raw["sign"]; ok {
			for _, p := range MCPSignRawProblems(rs) {
				errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: this mcp step: %s", st.Name, p)})
			}
		}
		for _, p := range MCPStepProblems(st, raw, claims[st.Name]) {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q: %s", st.Name, p)})
		}
	}
	return errs
}
