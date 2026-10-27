package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
)

// moneyengines.go — the `money_writes.allow` entry kinds that are not HTTP.
//
// Argus is system-agnostic: it has no opinion about what a system's checks may do. With
// `money_handling: true` the system's OWNER tells Argus to refuse what the owner's list does not name,
// and these matchers are how the owner names a bus operation, a tool call, a database step or a logs
// step. They are consulted from ONE place, MoneyGuardViolations (moneyguard.go), which every door
// calls with the same list — so config validate, the run door and the control plane agree by
// construction (TestMoneyGuard_ThreeDoorsAgree).
//
// ⛔ NOTHING an entry allows is bounded by Argus beyond what the entry names. There is no amount to cap
// on a bus publish or a tool call: an amqp or mcp entry is all-or-nothing for that op / tool. (`spends`
// and its three limit fields are refused on such an entry at load, by name.)
//
// ⛔ A NAME BUILT AT RUN TIME CANNOT BE LISTED. A step field that still holds `${` in the file (a
// `${saved.x}`, a `${cid}`, an environment variable) never matches an entry: the owner listed a name,
// and the name this step will use is not known until it runs.

// amqpAllowFields is, per amqp op, the name fields a `money_writes.allow` amqp entry must carry and
// compares EXACTLY with the step's fields as written. A field an op does not use is refused in the
// entry. (The table follows chain/amqp.go: the fields each op actually hands the broker.)
var amqpAllowFields = map[string][]string{
	"publish":          {"exchange", "routing_key"},
	"consume":          {"queue"},
	"declare_queue":    {"queue"},
	"declare_exchange": {"exchange"},
	"queue_delete":     {"queue"},
	"queue_unbind":     {"queue", "exchange", "routing_key"},
	"exchange_delete":  {"exchange"},
	// the queue a listen makes is named by Argus, so the entry names only what the owner can know.
	"listen":     {"exchange", "routing_key"},
	"queue_bind": {"queue", "exchange", "routing_key"},
}

// chainStepAllowedInChain is chainStepAllowed for a step judged WITH its chain (the ONE guard function).
// An amqp consume or queue_delete whose queue is exactly a `${saved.<var>}` that only `listen` steps of this
// chain save is allowed when a listen step of this chain that saves it is itself allowed: the queue's name is
// Argus's, so the owner cannot list it. A `${saved.<var>}` anything else saved (an http answer, an mcp result,
// a variable no step saves) never matches. FAIL CLOSED without an entry.
func (al MoneyWriteAllowlist) chainStepAllowedInChain(st ChainStep, steps []ChainStep) (bool, string) {
	if st.Type == "amqp" && (st.Op == "consume" || st.Op == "queue_delete") && listenQueueRef(st.Queue, steps) {
		v, _ := SavedWholeRef(st.Queue)
		for _, l := range steps {
			if l.Type != "amqp" || l.Op != "listen" {
				continue
			}
			if _, saves := l.Save[v]; saves && al.MatchAMQP(l) != nil {
				return true, ""
			}
		}
		return false, " — its queue is made by a `listen` step of this chain, and no money_writes.allow entry allows that listen step"
	}
	return al.chainStepAllowed(st)
}

// AMQPAllowNameFields returns the name fields an amqp entry for op must carry, and whether op is an
// amqp op at all.
func AMQPAllowNameFields(op string) ([]string, bool) {
	f, ok := amqpAllowFields[op]
	return f, ok
}

// stepRunTimeName reports whether a step field is built at run time (it still holds `${` in the file).
func stepRunTimeName(v string) bool { return strings.Contains(v, "${") }

// MatchAMQP returns the amqp entry that covers the step, or nil. The op must be the entry's op, and
// every name field that op uses must equal the entry's, byte for byte. The default exchange (a publish
// to exchange "") is a name like any other: an entry written `exchange: ""` covers it.
func (al MoneyWriteAllowlist) MatchAMQP(st ChainStep) *MoneyWriteAllow {
	fields, ok := amqpAllowFields[st.Op]
	if !ok {
		return nil
	}
	have := map[string]string{"exchange": st.Exchange, "routing_key": st.RoutingKey, "queue": st.Queue}
	for _, f := range fields {
		if stepRunTimeName(have[f]) {
			return nil
		}
	}
	for i := range al {
		e := &al[i]
		if e.kind() != AllowEngineAMQP || e.Op != st.Op || e.Stream != "" || e.Tool != "" || e.Target != "" || e.Form != "" || e.Spec != "" || e.Spends {
			continue
		}
		want := map[string]string{"exchange": e.Exchange, "routing_key": e.RoutingKey, "queue": e.Queue}
		same := true
		for _, f := range []string{"exchange", "routing_key", "queue"} {
			used := false
			for _, u := range fields {
				if u == f {
					used = true
				}
			}
			if used {
				// a name the op uses is compared exactly; the default exchange is the only name that may be ""
				if want[f] != have[f] || (want[f] == "" && !(st.Op == "publish" && f == "exchange")) {
					same = false
				}
			} else if want[f] != "" {
				same = false // the entry names a field this op does not use: it covers nothing
			}
		}
		if same {
			return e
		}
	}
	return nil
}

// MatchBusStream returns the `engine: amqp, op: listen, stream: <name>` entry that covers a bus step bound by
// `stream`, or nil. The name is compared exactly; a stream built at run time never matches. Only the
// executor's config can resolve a stream to an exchange and routing key, and the control plane and the validator cannot,
// so the entry names the stream itself. It never covers a step bound by exchange + routing_key (MatchAMQP skips every
// entry that names a stream), and an exchange / routing_key entry never covers a stream-bound step (it is not asked).
func (al MoneyWriteAllowlist) MatchBusStream(stream string) *MoneyWriteAllow {
	if stream == "" || stepRunTimeName(stream) {
		return nil
	}
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineAMQP && e.Op == "listen" && e.Stream == stream &&
			e.Exchange == "" && e.RoutingKey == "" && e.Queue == "" &&
			e.Target == "" && e.Tool == "" && e.Form == "" && e.Spec == "" && !e.Spends {
			return e
		}
	}
	return nil
}

// MatchMCP returns the mcp entry that covers a tool call, or nil. target is the name of a key of
// targets.mcp_targets, or "" for the unnamed targets.mcp; tool is the exact tool name.
//
// The CALLER decides whether the call's endpoint is the one `target` names. That rule (see the two
// callers, chainMCPAllowed and mcpScenarioAllowed): a call that sets its own `server_url` is NEVER
// matched, because the runner may send it to an address no entry names.
func (al MoneyWriteAllowlist) MatchMCP(target, tool string) *MoneyWriteAllow {
	if tool == "" || stepRunTimeName(tool) || stepRunTimeName(target) {
		return nil
	}
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineMCP && e.Tool == tool && e.Target == target &&
			e.Op == "" && e.Exchange == "" && e.RoutingKey == "" && e.Queue == "" && e.Stream == "" && e.Form == "" && e.Spec == "" && !e.Spends {
			return e
		}
	}
	return nil
}

// MatchDB returns the db entry that covers a chain `db` step against target ("" = targets.database).
func (al MoneyWriteAllowlist) MatchDB(target string) *MoneyWriteAllow {
	if stepRunTimeName(target) {
		return nil
	}
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineDB && e.Target == target &&
			e.Op == "" && e.Tool == "" && e.Exchange == "" && e.RoutingKey == "" && e.Queue == "" && e.Stream == "" && e.Form == "" && e.Spec == "" && !e.Spends {
			return e
		}
	}
	return nil
}

// MatchLogs returns the logs entry (any chain `logs` step is covered by it), or nil.
func (al MoneyWriteAllowlist) MatchLogs() *MoneyWriteAllow {
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineLogs && !e.hasEngineFields() && !e.Spends {
			return e
		}
	}
	return nil
}

// bareEntry is true for an entry that carries none of the fields of another kind, none of the HTTP ones
// and no spend limit. The three kinds of (cleanup, ui, fault) are matched only on an
// entry that is exactly what they name: a stray field makes the entry match nothing (fail closed).
func (e *MoneyWriteAllow) bareEntry(own ...string) bool {
	has := map[string]string{"op": e.Op, "exchange": e.Exchange, "routing_key": e.RoutingKey, "queue": e.Queue, "stream": e.Stream,
		"target": e.Target, "tool": e.Tool, "form": e.Form, "spec": e.Spec}
	for f, v := range has {
		keep := false
		for _, o := range own {
			if o == f {
				keep = true
			}
		}
		if v != "" && !keep {
			return false
		}
	}
	return strings.TrimSpace(e.Method) == "" && e.Path == "" && !e.Spends &&
		e.AmountField == "" && e.MaxAmount == 0 && e.MaxPerRun == 0
}

// CleanupForms are the forms a runnable `## CLEANUP` block (and so a cleanup entry) has.
func CleanupForms() []string { return []string{string(CleanupSQL), string(CleanupBash)} }

// MatchCleanup returns the cleanup entry that covers a runnable `## CLEANUP` block of the given form
// ("sql" | "bash"), or nil. Nothing finer than the form is checkable from what runs: a sql block is any
// statement against targets.database and a bash block is `sh -c` in the executor, so listing the form
// allows whatever the block does.
func (al MoneyWriteAllowlist) MatchCleanup(form string) *MoneyWriteAllow {
	if form == "" {
		return nil
	}
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineCleanup && e.Form == form && e.bareEntry("form") {
			return e
		}
	}
	return nil
}

// MatchUI returns the ui entry that covers a ui scenario or a ui chain step that names spec, or nil. The
// comparison is EXACT, on the spec as the file names it; a spec built at run time (it holds `${`) never
// matches, and an empty spec matches nothing.
func (al MoneyWriteAllowlist) MatchUI(spec string) *MoneyWriteAllow {
	if spec == "" || stepRunTimeName(spec) {
		return nil
	}
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineUI && e.Spec == spec && e.bareEntry("spec") {
			return e
		}
	}
	return nil
}

// MatchFault returns the fault entry (any chain `fault` step is covered by it), or nil. What a fault may
// touch stays bounded by the top-level fault_allowed_targets list and the run's fault preflight.
func (al MoneyWriteAllowlist) MatchFault() *MoneyWriteAllow {
	for i := range al {
		e := &al[i]
		if e.kind() == AllowEngineFault && e.bareEntry() {
			return e
		}
	}
	return nil
}

// UIScenarioSpec reads which spec a `ui`-tagged scenario drives, the way the driver picks it
// (internal/argus.runUIScenario): the `spec` field of the TRIGGER payload JSON as written, else the
// TRIGGER url with its backticks trimmed. ok is false when a non-blank payload is not JSON (the driver
// refuses such a scenario), so no entry can match it.
func UIScenarioSpec(s *Scenario) (spec string, ok bool) {
	if strings.TrimSpace(s.Trigger.Payload) != "" {
		var p struct {
			Spec string `json:"spec"`
		}
		if err := json.Unmarshal([]byte(s.Trigger.Payload), &p); err != nil {
			return "", false
		}
		spec = p.Spec
	}
	if spec == "" {
		spec = strings.Trim(s.Trigger.URL, "`")
	}
	return spec, true
}

// uiScenarioAllowed reports whether the list covers a `ui`-tagged scenario, and the clause telling the
// owner which entry would.
func (al MoneyWriteAllowlist) uiScenarioAllowed(s *Scenario) (bool, string) {
	spec, ok := UIScenarioSpec(s)
	if !ok {
		return false, " — its TRIGGER payload is not valid JSON, so no money_writes.allow entry can match it"
	}
	if al.MatchUI(spec) != nil {
		return true, ""
	}
	return false, uiHint(spec)
}

func uiHint(spec string) string {
	if spec == "" || stepRunTimeName(spec) {
		return " — a spec named at run time (or not at all) cannot be listed in money_writes.allow"
	}
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: ui, spec: %q` "+
		"(the entry allows whatever that spec does in the browser)", spec)
}

const faultHint = " — the system's owner may allow fault steps with a money_writes.allow entry `engine: fault` " +
	"(what a fault may touch stays limited by fault_allowed_targets)"

func cleanupHint(form string) string {
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: cleanup, form: %s` "+
		"(the entry allows whatever a %s block does)", form, form)
}

// chainStepAllowed reports whether the list covers a chain step of a type other than http, and — when
// it does not — the clause that tells the owner which entry would allow it ("" for a step type no entry
// kind exists for: anything unknown).
func (al MoneyWriteAllowlist) chainStepAllowed(st ChainStep) (bool, string) {
	switch st.Type {
	case "amqp":
		if al.MatchAMQP(st) != nil {
			return true, ""
		}
		return false, amqpHint(st)
	case "mcp":
		if strings.TrimSpace(st.ServerURL) == "" && al.MatchMCP(st.Target, st.Tool) != nil {
			return true, ""
		}
		return false, mcpHint(st.Target, st.Tool, strings.TrimSpace(st.ServerURL) != "")
	case "db":
		if al.MatchDB(st.Target) != nil {
			return true, ""
		}
		return false, dbHint(st.Target)
	case "logs":
		if al.MatchLogs() != nil {
			return true, ""
		}
		return false, " — the system's owner may allow logs steps with a money_writes.allow entry `engine: logs`"
	case "bus":
		// a bus step subscribes with a queue of its own, as an amqp `listen` on the same exchange and
		// routing key, and is allowed by that entry. A step bound by `stream` names neither in the file (only the executor's
		// config resolves a stream), so it is allowed by an `engine: amqp, op: listen, stream: <name>` entry.
		if st.Stream == "" {
			l := BusStepAsListen(st)
			if al.MatchAMQP(l) != nil {
				return true, ""
			}
			return false, amqpHint(l)
		}
		if al.MatchBusStream(st.Stream) != nil {
			return true, ""
		}
		return false, busStreamHint(st.Stream)
	case "ui":
		if al.MatchUI(st.Spec) != nil {
			return true, ""
		}
		return false, uiHint(st.Spec)
	case "fault":
		if al.MatchFault() != nil {
			return true, ""
		}
		return false, faultHint
	}
	return false, ""
}

func busStreamHint(stream string) string {
	if stepRunTimeName(stream) {
		return " — a stream built at run time (stream " + fmt.Sprintf("%q", stream) + ") cannot be listed in money_writes.allow"
	}
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: amqp, op: listen, stream: %q`", stream)
}

func amqpHint(st ChainStep) string {
	fields, ok := amqpAllowFields[st.Op]
	if !ok {
		return ""
	}
	have := map[string]string{"exchange": st.Exchange, "routing_key": st.RoutingKey, "queue": st.Queue}
	var parts []string
	for _, f := range fields {
		if stepRunTimeName(have[f]) {
			return " — a name built at run time (" + f + " " + fmt.Sprintf("%q", have[f]) + ") cannot be listed in money_writes.allow"
		}
		parts = append(parts, fmt.Sprintf("%s: %q", f, have[f]))
	}
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: amqp, op: %s, %s`",
		st.Op, strings.Join(parts, ", "))
}

func mcpHint(target, tool string, ownEndpoint bool) string {
	if ownEndpoint {
		return " — a call that sets its own endpoint (a server_url, or a TRIGGER url on a scenario that names no **Target**) is never matched by a money_writes.allow entry; name a target from targets.mcp_targets instead"
	}
	if tool == "" || stepRunTimeName(tool) || stepRunTimeName(target) {
		return " — a tool or target built at run time cannot be listed in money_writes.allow"
	}
	if target == "" {
		return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: mcp, tool: %q`", tool)
	}
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: mcp, target: %q, tool: %q`", target, tool)
}

func dbHint(target string) string {
	if stepRunTimeName(target) {
		return " — a target built at run time cannot be listed in money_writes.allow"
	}
	if target == "" {
		return " — the system's owner may allow it with a money_writes.allow entry `engine: db`"
	}
	return fmt.Sprintf(" — the system's owner may allow it with a money_writes.allow entry `engine: db, target: %q`", target)
}

// mcpScenarioCall reads what an `mcp`-tagged scenario calls: the target from its Metadata **Target**
// and the tool from its TRIGGER payload, both AS WRITTEN. ok is false when the payload cannot be read.
//
// THE ENDPOINT RULE, in one sentence: a scenario that names a **Target** is matched by that name alone
// (the runner ignores its TRIGGER url then, and refuses a payload server_url beside it); one that names
// none is matched as the unnamed target only when it sets neither a TRIGGER url nor a payload
// server_url, because either may send it to an address no entry names. So the in-tree `POST ${MCP_URL}`
// shape is not matched as the unnamed target — an owner who wants it allowed names a target.
func mcpScenarioCall(s *Scenario) (target, tool string, endpointOK, ok bool) {
	var raw struct {
		Tool      string `json:"tool"`
		ServerURL string `json:"server_url"`
	}
	if err := json.Unmarshal([]byte(s.Trigger.Payload), &raw); err != nil {
		return "", "", false, false
	}
	target = strings.TrimSpace(s.Target)
	if strings.TrimSpace(raw.ServerURL) != "" {
		return target, raw.Tool, false, true
	}
	if target == "" && strings.TrimSpace(strings.Trim(s.Trigger.URL, "` ")) != "" {
		return target, raw.Tool, false, true
	}
	return target, raw.Tool, true, true
}

// mcpScenarioAllowed reports whether the list covers an `mcp`-tagged scenario, and the clause telling
// the owner which entry would.
func (al MoneyWriteAllowlist) mcpScenarioAllowed(s *Scenario) (bool, string) {
	target, tool, endpointOK, ok := mcpScenarioCall(s)
	if !ok {
		return false, " — its TRIGGER payload could not be read as a tool call, so no money_writes.allow entry can match it"
	}
	if endpointOK && al.MatchMCP(target, tool) != nil {
		return true, ""
	}
	return false, mcpHint(target, tool, !endpointOK)
}
