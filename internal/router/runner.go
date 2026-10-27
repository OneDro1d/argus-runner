package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// THE PASS-THROUGH ROUTER, PART 2 — THE RUNNER TOOLS (AC-D70, design #678 A4, A5, A6, B4).
//
// The router used to offer every folder a fixed list of 6 runner tools, spelled runner__X, each with one shared input
// list — which hid inputs the executors really have (use_local_scenarios on the run tool). It keeps no list of runner
// tools any more: when a session starts, it reads each instance's executor, with the folder's OWN executor token, and
// passes the tools on unchanged. The only change is the spelling of an old executor's names (A4: runner__X is shown as
// runner_X — a fixed rule, not a list). Calls go to the target instance's executor under the name THAT executor listed,
// and its answers come back unchanged (decision 14).
//
// Several instances in one folder (A5): the session lists the union of what their executors offer, with the newest
// executor's definition where two differ; a call is checked against the TARGET instance's own offer, so an older executor
// is never refused a call it would accept.
//
// What a session keeps about the runner plane is a runnerSnapshot: per instance, the cause that stopped it (or none), its
// version and its names. Never a URL and never a token — every call finds the folder, the address and the token again
// from the router token (B4 step 1), so a reload, a rotated token or a removed instance takes effect on the next call.
//
// No text an agent gets holds an address or a token: the session-start texts below do not, and a call that brings back
// no usable answer gets the router's own sentence (passBack, AC-D70.2) — the client's text, which quotes the URL or the
// executor's error page, goes to the router's log only.

// The texts an agent gets about one instance's executor (design A6 — the control plane's texts, for an executor). One per
// cause; only the first says "could not reach". None holds a token or a URL.
const (
	textExecUnreachableFmt = "Argus could not reach the executor of instance %s when it read the tools for this session, so its runner tools are not listed. Make sure the instance is running, then reconnect (in Claude Code: /mcp)."
	textExecRefusedFmt     = "The executor of instance %s refused this folder's executor token, so its runner tools are not listed. Onboard instance %s again (onboarding step 7/8 writes this folder's executor token), then reconnect."
	textExecErrorFmt       = "The executor of instance %s answered with an error (%s) when Argus read the tools for this session. Try again later, then reconnect."
	textExecNoAddressFmt   = "This folder holds no address for the executor of instance %s, so its runner tools are not listed — onboard instance %s again, then reconnect (in Claude Code: /mcp)."
	textExecNoTokenFmt     = "This machine holds no executor token for instance %s in this folder, so its runner tools are not listed — onboard instance %s again, then reconnect (in Claude Code: /mcp)."
	textExecBreachFmt      = "The executor of instance %s listed a tool that is not a runner tool (%s). That should not happen, so none of its tools are shown in this session. Report it to the Argus team."
	textExecNotPassedOnFmt = "The executor of instance %s listed %s in a form Argus cannot pass on (%s), so Argus does not use it for that instance in this session. Report it to the Argus team."
	textExecUsableNowFmt   = "The executor of instance %s became usable after this session started. Reconnect (in Claude Code: /mcp) to use it."
	// The A5 texts: a tool the target instance's executor does not offer.
	textNotOfferedUpdateFmt = "%s is not offered by the executor of instance %s. Update that instance's executor to use it."
	textNotOfferedOlderFmt  = "%s is not offered by the executor of instance %s. Only older executors in this folder still offer it."
	// textInstanceIDRequired is the executor's OWN answer to a runner call with no instance_id (internal/mcpserver/tools.go),
	// so the router gives the same bad call the same answer (A9 #9).
	textInstanceIDRequired = "instance_id is required"
)

// maxQuotedName bounds an executor-chosen name quoted in a session-start text or a log line: the name comes from a list
// the router does not control, and it reaches the model through the instructions.
const maxQuotedName = 64

// listTools reads one server's tool list. A variable only so a test can make a read fail in ways a real server cannot.
var listTools = func(c *mcp.Client) mcp.ListResult { return c.ListTools() }

// runnerDef is one tool's definition as one executor listed it, already respelled (B4).
type runnerDef struct {
	description string
	schema      map[string]any
}

// runnerInstance is one instance's runner plane as ONE read of its executor's list found it: a cause, a version and names.
type runnerInstance struct {
	id         string
	cause      string // "" = usable; else the sentence a session start and a call give
	stateCause bool   // the cause is the folder's own record (no address, no token): a re-onboarding can fix it
	failed     bool   // the read itself failed — an outage, so a first-use set asks to be built again
	version    string
	offers     map[string]string // shown name → the name this executor listed
	order      []string          // shown names, in this executor's order
	defs       map[string]runnerDef
	notPassed  map[string]string // shown name → the sentence for a tool listed in a form Argus cannot pass on
	texts      []string          // session-start sentences about single tools (not passed on)
	digest     string            // over everything the executor listed, and its version
}

// runnerSnapshot is a session's runner plane: every instance of the folder as read when the session started. It is
// immutable once built, and shared by every session whose reads were the same (the ToolSet cache keys on its digest).
type runnerSnapshot struct {
	ids       []string
	instances map[string]*runnerInstance
}

// offerers lists the instances of the snapshot, other than except, whose executor offers shown.
func (sn *runnerSnapshot) offerers(shown, except string) []*runnerInstance {
	var out []*runnerInstance
	if sn == nil {
		return nil
	}
	for _, id := range sn.ids {
		if inst := sn.instances[id]; id != except && inst.cause == "" {
			if _, ok := inst.offers[shown]; ok {
				out = append(out, inst)
			}
		}
	}
	return out
}

// ── names ───────────────────────────────────────────────────────────────────────────────────────────────────────

// shownRunnerName is the fixed spelling rule (A4, decision 7): runner__X is shown as runner_X, runner_X stays. A name
// whose rest is empty or starts with "_" (runner_, runner__, runner___x) cannot be respelled without making another
// old-style name, so it is not shown at all.
func shownRunnerName(listed string) (string, bool) {
	rest, ok := strings.CutPrefix(listed, "runner__")
	if !ok {
		rest, ok = strings.CutPrefix(listed, "runner_")
	}
	if !ok || rest == "" || strings.HasPrefix(rest, "_") {
		return "", false
	}
	return "runner_" + rest, true
}

// legacyRunnerAlias is the old spelling of a shown name: the router accepts it from agents for one release (A4, B8).
func legacyRunnerAlias(shown string) string { return "runner__" + strings.TrimPrefix(shown, "runner_") }

// oldToolName is an old-style tool name where a name STARTS (AC-D70.2): runner__ or author__ at the start of the text or
// after a character that cannot be part of a name, followed by the first character of the rest. So xrunner__y and
// my_runner__y (inside a longer word) and runner___x (no name can start with "_") are not tool names.
var oldToolName = regexp.MustCompile(`(^|[^A-Za-z0-9_])(runner|author)__([a-z0-9])`)

// respellText applies the spelling rule to the tool names an executor quotes in its texts (B4): runner__X → runner_X,
// author__X → author_X, whole names only. A match ends on a name character, so two names next to each other cannot
// overlap: the second one starts after a separator, which the regexp sees.
func respellText(s string) string {
	return oldToolName.ReplaceAllString(s, "${1}${2}_${3}")
}

// respellSchema applies respellText to every "description" STRING inside a decoded input schema, at any depth. Nothing
// else is rewritten: names, enums, defaults and titles are the executor's contract and stay as listed.
func respellSchema(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "description" {
				x[k] = respellText(s)
				continue
			}
			respellSchema(e)
		}
	case []any:
		for _, e := range x {
			respellSchema(e)
		}
	}
}

// cappedName quotes a name an executor chose, cut to maxQuotedName bytes.
func cappedName(n string) string {
	if n == "" {
		return "a tool with no name"
	}
	if len(n) <= maxQuotedName {
		return fmt.Sprintf("%q", n)
	}
	cut := n[:maxQuotedName]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return fmt.Sprintf("%q…", cut)
}

func cappedNames(names []string) string {
	s := cappedName(names[0])
	if len(names) > 1 {
		s += fmt.Sprintf(" and %d more", len(names)-1)
	}
	return s
}

// ── versions ────────────────────────────────────────────────────────────────────────────────────────────────────

// olderVersion orders executor versions (A5) in three ranks, oldest first:
//
//  1. a version updatecmd cannot read that is not a developer's build — the old fixed "M2.5", "" (no version), a tag;
//  2. every version updatecmd can read, ordered by updatecmd.SemverLess (the codebase's one comparator for them);
//  3. a developer's own build — buildinfo's 0.0.0-dev and 0.0.0-src+<rev> sentinels (devBuild).
//
// Two versions in rank 1, or two in rank 3, are equal. AC-D70.2 (#699, proposed) moved the sentinels from rank 1 to
// rank 3: an executor a developer built is typically newer than every release, so its tool definitions are the ones a
// folder shows. Only the order of SHOWN definitions and the A5 refusal text depend on it — every call is still checked
// against the target executor's own tools — and updatecmd.ValidVersion, which guards updates, is unchanged.
func olderVersion(a, b string) bool {
	ra, rb := versionRank(a), versionRank(b)
	if ra != rb {
		return ra < rb
	}
	return ra == 2 && updatecmd.SemverLess(a, b)
}

// versionRank is the rank of v in olderVersion's order: 1 unreadable, 2 readable, 3 a developer's own build.
func versionRank(v string) int {
	switch {
	case devBuild(v):
		return 3
	case updatecmd.ValidVersion(v):
		return 2
	}
	return 1
}

// devBuild reports whether v is one of buildinfo's "this build has no release identity" sentinels: 0.0.0-dev for an
// unstamped build, 0.0.0-src+<rev> for one built from a checkout. The same rule as control.isNoReleaseIdentity and
// federation's version floors.
func devBuild(v string) bool {
	return strings.HasPrefix(v, "0.0.0-dev") || strings.HasPrefix(v, "0.0.0-src")
}

// notOfferedText is the A5 refusal for target, which does not offer shown: "only older executors still offer it" when
// EVERY instance that offers it is older than the target, otherwise "update that instance's executor".
func notOfferedText(shown string, target *runnerInstance, offerers []*runnerInstance) string {
	for _, o := range offerers {
		if !olderVersion(o.version, target.version) {
			return fmt.Sprintf(textNotOfferedUpdateFmt, shown, target.id)
		}
	}
	return fmt.Sprintf(textNotOfferedOlderFmt, shown, target.id)
}

// ── reading one executor ────────────────────────────────────────────────────────────────────────────────────────

// executorRead is what the network part of a read brought back. It is split from the processing so a session start can
// run every read in parallel and do all the processing in one goroutine.
type executorRead struct {
	id    string
	state *runnerInstance // set when the folder's own record stopped the read before any call
	res   mcp.ListResult
}

// readExecutor is the network part: the folder's record first (no call), then one list read with the list timeout.
func (s *sessions) readExecutor(id string, up Upstream) executorRead {
	switch {
	case strings.TrimSpace(up.URL) == "":
		return executorRead{id: id, state: &runnerInstance{id: id, cause: fmt.Sprintf(textExecNoAddressFmt, id, id), stateCause: true}}
	case strings.TrimSpace(up.Token) == "":
		return executorRead{id: id, state: &runnerInstance{id: id, cause: fmt.Sprintf(textExecNoTokenFmt, id, id), stateCause: true}}
	}
	c := &mcp.Client{ServerURL: ResolveFromVantage(up.URL, s.opts.HostAlias), Transport: s.opts.Transport, Token: up.Token,
		Timeout: s.opts.ListTimeout}
	return executorRead{id: id, res: listTools(c)}
}

// readInstance reads one executor now — an instance onboarded after the session started, the re-read after an executor
// said it does not know a name, the re-read before an A5 refusal. The same rules as at session start (B4, decision 13).
func (s *sessions) readInstance(where, id string, up Upstream) *runnerInstance {
	return s.processExecutor(where, s.readExecutor(id, up))
}

// processExecutor turns one read into the instance's runner plane: decision 13 first, over every listed name; then each
// tool by the spelling rule and a strict client's schema rules; descriptions respelled.
func (s *sessions) processExecutor(where string, r executorRead) *runnerInstance {
	if r.state != nil {
		return r.state
	}
	inst := &runnerInstance{id: r.id, offers: map[string]string{}, defs: map[string]runnerDef{}, notPassed: map[string]string{}}
	if f := r.res.Failure; f != nil {
		inst.cause, inst.failed = execFailureText(r.id, f), true
		s.logf("router: the tool list of the executor of instance %s for folder %s could not be read: %s", r.id, where, f.Error())
		return inst
	}
	inst.version = r.res.Server.Version

	var breach []string
	for _, lt := range r.res.Tools {
		if !strings.HasPrefix(lt.Name, "runner_") {
			breach = append(breach, lt.Name)
		}
	}
	if len(breach) > 0 {
		inst.cause = fmt.Sprintf(textExecBreachFmt, r.id, cappedNames(breach))
		s.logf("router: decision 13 — the executor of instance %s for folder %s listed tool(s) that are not runner tools (%s); none of its tools is shown in that session", r.id, where, cappedNames(breach))
		return inst
	}

	h := sha256.New()
	fmt.Fprintf(h, "%q\x00", inst.version)
	for _, lt := range r.res.Tools {
		fmt.Fprintf(h, "%q\x00%q\x00", lt.Name, lt.Description)
		h.Write(lt.InputSchema)
		h.Write([]byte{0})
		shown, ok := shownRunnerName(lt.Name)
		if !ok {
			text := fmt.Sprintf(textExecNotPassedOnFmt, r.id, cappedName(lt.Name), "its name cannot be spelled with one underscore")
			inst.texts = append(inst.texts, text)
			s.logf("router: the executor of instance %s for folder %s listed %s, which is not passed on: its name cannot be spelled with one underscore", r.id, where, cappedName(lt.Name))
			continue
		}
		schema, why := decodeSchema(lt.InputSchema)
		if why != "" {
			text := fmt.Sprintf(textExecNotPassedOnFmt, r.id, cappedName(lt.Name), why)
			inst.texts = append(inst.texts, text)
			inst.notPassed[shown] = text
			s.logf("router: the executor of instance %s for folder %s listed %s, which is not passed on: %s", r.id, where, cappedName(lt.Name), why)
			continue
		}
		if kept, dup := inst.offers[shown]; dup {
			// Listed twice (AC-D70.2): the entry already spelled with one underscore is kept, whatever the order; between
			// two entries of the same spelling, the first. The shown name keeps its first position.
			if lt.Name != shown || kept == shown {
				s.logf("router: the executor of instance %s for folder %s listed %s twice (as %s and %s); %s is kept", r.id, where,
					cappedName(shown), cappedName(kept), cappedName(lt.Name), cappedName(kept))
				continue
			}
			s.logf("router: the executor of instance %s for folder %s listed %s twice (as %s and %s); %s is kept", r.id, where,
				cappedName(shown), cappedName(kept), cappedName(lt.Name), cappedName(lt.Name))
		} else {
			inst.order = append(inst.order, shown)
		}
		respellSchema(schema)
		inst.offers[shown] = lt.Name
		inst.defs[shown] = runnerDef{description: respellText(lt.Description), schema: schema}
	}
	for shown := range inst.offers {
		delete(inst.notPassed, shown) // a well-formed entry of the same name wins over a malformed one
	}
	inst.digest = hex.EncodeToString(h.Sum(nil))
	return inst
}

// execFailureText is the A6 text for an executor list read that failed.
func execFailureText(id string, f *mcp.ListFailure) string {
	switch {
	case f.Kind == mcp.ListUnreachable || f.Kind == mcp.ListTimedOut:
		return fmt.Sprintf(textExecUnreachableFmt, id)
	case f.AuthRefused():
		return fmt.Sprintf(textExecRefusedFmt, id, id)
	case f.Kind == mcp.ListHTTPStatus:
		return fmt.Sprintf(textExecErrorFmt, id, fmt.Sprintf("HTTP %d", f.HTTPStatus))
	case f.Kind == mcp.ListRPCError:
		return fmt.Sprintf(textExecErrorFmt, id, fmt.Sprintf("JSON-RPC error %d", f.RPCCode))
	}
	return fmt.Sprintf(textExecErrorFmt, id, "an answer that is not a tool list")
}

// ── the session's runner plane ──────────────────────────────────────────────────────────────────────────────────

// runnerPlane merges the reads of one folder's executors (ids sorted) into the session's snapshot and tools (A5).
func (s *sessions) runnerPlane(f *Folder, ids []string, reads []executorRead) (snap *runnerSnapshot, tools []mcpserver.Tool, texts []string, digest string, retry bool) {
	snap = &runnerSnapshot{ids: ids, instances: make(map[string]*runnerInstance, len(ids))}
	h := sha256.New()
	var order []string
	best := map[string]*runnerInstance{}
	for i, id := range ids {
		inst := s.processExecutor(f.Path, reads[i])
		snap.instances[id] = inst
		fmt.Fprintf(h, "%q\x00%q\x00%q\x00%t\x00%s\x00", id, inst.cause, inst.version, inst.stateCause, inst.digest)
		if inst.cause != "" {
			texts = append(texts, inst.cause)
			retry = retry || inst.failed || inst.stateCause
			continue
		}
		texts = append(texts, inst.texts...)
		for _, shown := range inst.order {
			cur, seen := best[shown]
			if !seen {
				order = append(order, shown)
			}
			// The newest executor's definition; a tie keeps the earlier — the lowest instance id (ids are sorted).
			if !seen || olderVersion(cur.version, inst.version) {
				best[shown] = inst
			}
		}
	}
	for _, shown := range order {
		def := best[shown].defs[shown]
		tools = append(tools, mcpserver.Tool{Name: shown, Namespace: mcpserver.NSRunner, Description: def.description,
			InputSchema: def.schema, Aliases: []string{legacyRunnerAlias(shown)}, Handler: s.runnerHandler(shown, snap)})
	}
	return snap, tools, texts, hex.EncodeToString(h.Sum(nil)), retry
}

// ── the runner call path (B4 steps 1, 2, 4-8) ───────────────────────────────────────────────────────────────────

func (s *sessions) runnerHandler(shown string, snap *runnerSnapshot) mcpserver.Handler {
	return func(args json.RawMessage, prin mcpserver.Principal) mcpserver.Outcome {
		return s.runnerCall(shown, snap, nil, args, prin)
	}
}

func toolErrText(text string) mcpserver.Outcome {
	return mcpserver.ToolErr(map[string]any{"error": text})
}

// runnerCall serves one runner call. pre, when set, is a read of the target made a moment ago for THIS call (UnknownTool
// read an instance onboarded after the session started), so it is not read twice.
func (s *sessions) runnerCall(shown string, snap *runnerSnapshot, pre *runnerInstance, args json.RawMessage, prin mcpserver.Principal) mcpserver.Outcome {
	// 1. The folder, from the router token, on EVERY call — the address and the token come from it, live.
	f, err := s.tbl.Resolve(prin.Subject)
	if err != nil {
		return toolErrText(err.Error())
	}
	// 4. instance_id, before any network call.
	id, absent, refusal := checkRunnerArgs(f, args)
	if refusal != nil {
		return *refusal
	}
	if absent {
		return toolErrText(textInstanceIDRequired)
	}
	up := f.Upstreams[id]
	// 2 and 5. The target instance: its state at session start, and its own offer.
	inst := pre
	if inst == nil || inst.id != id {
		inst = nil
		if known := snap.instances[id]; known != nil {
			if known.cause != "" {
				return toolErrText(unusableText(f, known)) // the session-start sentence; no request to that executor
			}
			inst = known
		} else {
			// Onboarded into the folder after the session started: read it now (A5 "works at once").
			inst = s.readInstance(f.Path, id, up)
			if inst.cause != "" {
				return toolErrText(inst.cause)
			}
		}
	}
	listed, ok := inst.offers[shown]
	if !ok {
		if text, np := inst.notPassed[shown]; np {
			return toolErrText(text)
		}
		// Before refusing, read the target once more: it may have been updated since the session started.
		if inst == snap.instances[id] {
			if fresh, usable := s.reReadInstance(f.Path, id, up); usable {
				if l, now := fresh.offers[shown]; now {
					return s.forwardRunner(f.Path, id, up, shown, l, snap, args)
				}
				if text, np := fresh.notPassed[shown]; np {
					return toolErrText(text)
				}
				inst = fresh
			}
		}
		return toolErrText(notOfferedText(shown, inst, snap.offerers(shown, id)))
	}
	return s.forwardRunner(f.Path, id, up, shown, listed, snap, args)
}

// reReadInstance reads, during a call, an instance the session already knows — only ever to avoid a WRONG answer (the
// target may have been updated or rolled back). Its result counts only when it is a usable list: a read that failed, or
// a list that breaks decision 13, changes nothing the session knew, so the caller answers as it would have without it.
// (A session-start sentence would be false here — "… when it read the tools for this session … reconnect", "none of its
// tools are shown in this session" — while that instance's tools ARE listed in this session.) The cause is logged by
// processExecutor as at session start.
func (s *sessions) reReadInstance(where, id string, up Upstream) (*runnerInstance, bool) {
	fresh := s.readInstance(where, id, up)
	return fresh, fresh.cause == ""
}

// forwardRunner sends the call to the target's executor (step 6), retries once under the name it lists now if it no
// longer knows the one used (step 7), and passes its answer back unchanged (step 8).
func (s *sessions) forwardRunner(where, id string, up Upstream, shown, listed string, snap *runnerSnapshot, args json.RawMessage) mcpserver.Outcome {
	localised := json.RawMessage(localiseRaw(args))
	call := func(name string) mcp.CallResult {
		c := &mcp.Client{ServerURL: ResolveFromVantage(up.URL, s.opts.HostAlias), Transport: s.opts.Transport, Token: up.Token,
			Timeout: s.opts.RunnerCallTimeout}
		return c.Call(mcp.CallInput{Tool: name, Args: localised})
	}
	res := call(listed)
	if res.JSONRPCError != nil && res.JSONRPCError.Code == mcpserver.CodeMethodNotFound {
		// The executor was rolled back or updated while the session was open. -32601 is answered before any tool runs,
		// so the retry can never run anything twice; there is exactly one. A re-read that is not a usable list (it
		// failed, or it breaks decision 13) is not used: the executor's own answer stands.
		if fresh, usable := s.reReadInstance(where, id, up); usable {
			if l2, now := fresh.offers[shown]; now && l2 != listed {
				res = call(l2)
			} else if !now {
				if text, np := fresh.notPassed[shown]; np {
					return toolErrText(text)
				}
				if others := snap.offerers(shown, id); len(others) > 0 {
					return toolErrText(notOfferedText(shown, fresh, others))
				}
			}
		}
	}
	return s.passBack(res, "runner", argString(args, "instance_id"), where)
}

// unusableText is what a call gets for an instance that could not be used when the session started: the same sentence
// (A6) — unless that sentence was about the folder's own record and the live folder no longer has the gap, which the
// agent can only take up by reconnecting.
func unusableText(f *Folder, inst *runnerInstance) string {
	if inst.stateCause {
		if up := f.Upstreams[inst.id]; strings.TrimSpace(up.URL) != "" && strings.TrimSpace(up.Token) != "" {
			return fmt.Sprintf(textExecUsableNowFmt, inst.id)
		}
	}
	return inst.cause
}

// checkRunnerArgs applies B4 step 4 to a runner call. The router's own rules — a key that only folds to instance_id, a
// repeated instance_id, an instance outside the folder or a joined list — are refused with the router's texts. An
// instance_id that is absent, empty or not a string, or arguments that are not an object, are reported as absent: the
// executor itself answers those "instance_id is required", and so does the router. id is the trimmed instance id.
func checkRunnerArgs(f *Folder, args json.RawMessage) (id string, absent bool, refusal *mcpserver.Outcome) {
	idRaw, present, isObject, o := exactInstanceKey(args)
	if o != nil {
		return "", false, o
	}
	if !isObject || !present {
		return "", true, nil
	}
	var v string
	if json.Unmarshal(idRaw, &v) != nil || strings.TrimSpace(v) == "" {
		return "", true, nil
	}
	if err := f.checkInstance(v); err != nil {
		o := toolErrText(err.Error())
		return "", false, &o
	}
	return strings.TrimSpace(v), false, nil
}

// exactInstanceKey reads instance_id from an arguments object by part 1's key rules (session.go checkAuthorArgs): a key
// that only folds to instance_id, or a second instance_id, is refused — Go's decoder on the other side would take a
// different one than the router checked.
func exactInstanceKey(args json.RawMessage) (idRaw json.RawMessage, present, isObject bool, refusal *mcpserver.Outcome) {
	keys, vals, isObject := topLevelKeys(args)
	if !isObject {
		return nil, false, false, nil
	}
	n := 0
	for i, k := range keys {
		switch {
		case k == "instance_id":
			n++
			idRaw = vals[i]
		case strings.EqualFold(k, "instance_id"):
			o := toolErrText(fmt.Sprintf("router: the argument %q is not instance_id — spell it instance_id exactly", k))
			return nil, false, true, &o
		}
	}
	if n > 1 {
		o := toolErrText("router: instance_id is given more than once — give it once")
		return nil, false, true, &o
	}
	return idRaw, n == 1, true, nil
}

// localiseRaw replaces the value of the top-level instance_id with "local" — the name the executor gives itself
// (InEnvInstanceID) — and leaves every other byte of the object as the agent sent it: nothing is decoded into Go values,
// so numbers and key order reach the executor unchanged. It is called only after exactInstanceKey found exactly one.
func localiseRaw(args json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(args)
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return args
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return args
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return args
		}
		if k, _ := tok.(string); k == "instance_id" {
			end := int(dec.InputOffset())
			start := end - len(v)
			out := make([]byte, 0, len(trimmed)-len(v)+len(InEnvInstanceID)+2)
			out = append(out, trimmed[:start]...)
			out = append(out, '"')
			out = append(out, InEnvInstanceID...)
			out = append(out, '"')
			return append(out, trimmed[end:]...)
		}
	}
	return args
}

// ── names not in the session's set ──────────────────────────────────────────────────────────────────────────────

// unknownRunnerTool answers a runner name the session does not list (B4 step 2).
func (s *sessions) unknownRunnerTool(name string, args json.RawMessage, prin mcpserver.Principal, st *sessionState) (*mcpserver.Tool, *mcpserver.Outcome) {
	errOut := func(text string) (*mcpserver.Tool, *mcpserver.Outcome) {
		o := toolErrText(text)
		return nil, &o
	}
	f, err := s.tbl.Resolve(prin.Subject)
	if err != nil {
		return errOut(err.Error())
	}
	shown, ok := shownRunnerName(name)
	if !ok {
		return nil, nil
	}
	snap := &runnerSnapshot{instances: map[string]*runnerInstance{}}
	if st != nil && st.runner != nil {
		snap = st.runner
	}
	id, absent, refusal := checkRunnerArgs(f, args)
	if refusal != nil {
		return nil, refusal
	}
	if absent {
		// No instance named: the tool may be missing because an instance could not be used — say which (A6, A9 #12).
		var lines []string
		for _, iid := range snap.ids {
			if inst := snap.instances[iid]; inst.cause != "" {
				if _, live := f.Upstreams[iid]; live {
					lines = append(lines, unusableText(f, inst))
				}
			}
		}
		if len(lines) == 0 {
			return nil, nil
		}
		return errOut(strings.Join(lines, "\n"))
	}
	if inst := snap.instances[id]; inst != nil {
		if inst.cause != "" {
			return errOut(unusableText(f, inst))
		}
		if text, np := inst.notPassed[shown]; np {
			return errOut(text)
		}
		return s.unlistedOnAKnownInstance(f, id, name, shown, snap, inst)
	}
	// Onboarded after the session started: read it now, ONCE, and hand that read to the call.
	inst := s.readInstance(f.Path, id, f.Upstreams[id])
	if inst.cause != "" {
		return errOut(inst.cause)
	}
	if _, offers := inst.offers[shown]; !offers {
		if text, np := inst.notPassed[shown]; np {
			return errOut(text)
		}
		return nil, nil
	}
	return &mcpserver.Tool{Name: name, Namespace: mcpserver.NSRunner,
		Handler: func(a json.RawMessage, p mcpserver.Principal) mcpserver.Outcome {
			return s.runnerCall(shown, snap, inst, a, p)
		}}, nil
}

// unlistedOnAKnownInstance answers a runner name the session does not list, called on an instance that was usable when
// the session started (B4 step 2). No instance of the session's offers the name, so: the target may offer it NOW (it was
// updated while the session was open) → a Tool that forwards with that read (R6); otherwise an instance onboarded into
// the folder after the session started may offer it → the A5 text, which is then true; otherwise nothing in this folder
// offers it → nil, the executor's own "unknown tool" (D-11). These reads happen only on this path, never for a listed name.
func (s *sessions) unlistedOnAKnownInstance(f *Folder, id, name, shown string, snap *runnerSnapshot, inst *runnerInstance) (*mcpserver.Tool, *mcpserver.Outcome) {
	target := inst
	if fresh, usable := s.reReadInstance(f.Path, id, f.Upstreams[id]); usable {
		if _, now := fresh.offers[shown]; now {
			return &mcpserver.Tool{Name: name, Namespace: mcpserver.NSRunner,
				Handler: func(a json.RawMessage, p mcpserver.Principal) mcpserver.Outcome {
					return s.runnerCall(shown, snap, fresh, a, p)
				}}, nil
		}
		if text, np := fresh.notPassed[shown]; np {
			o := toolErrText(text)
			return nil, &o
		}
		target = fresh
	}
	var late []*runnerInstance
	for _, lid := range sortedIDs(f) {
		if _, known := snap.instances[lid]; known {
			continue
		}
		if li := s.readInstance(f.Path, lid, f.Upstreams[lid]); li.cause == "" {
			if _, offers := li.offers[shown]; offers {
				late = append(late, li)
			}
		}
	}
	if len(late) == 0 {
		return nil, nil
	}
	o := toolErrText(notOfferedText(shown, target, late))
	return nil, &o
}

// sortedIDs is the folder's instance ids, sorted (B4 "for each instance in f.Upstreams (sorted)").
func sortedIDs(f *Folder) []string {
	ids := make([]string, 0, len(f.Upstreams))
	for id := range f.Upstreams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
