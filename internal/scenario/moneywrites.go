package scenario

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// moneywrites.go — "money writes" (T5.4 follow-up, 2026-09-26 product decision; widened 2026-10-11):
// Argus is a monitoring kit and is agnostic about what a system's checks may do.
// `money_handling: true` is a switch the system's OWNER sets in the owner's own config, meaning "refuse
// what I did not list", and `money_writes.allow` is the owner's list. This file is the PURE core the
// money-path guard (moneyguard.go) and all three of its doors share: the allowlist matchers (an HTTP
// method+path, and an entry per engine — moneyengines.go), the resolved-body spend check, and the
// per-run spend ledger.
//
// Nothing here knows about YAML or config — internal/config parses `money_writes` into this
// package's types (config.MoneyWritesConfig, config/moneywrites.go) precisely because config already
// imports scenario and the reverse would cycle.

// MoneyWriteAllow is one entry of a SUT's `money_writes.allow` block. An entry with no `engine` (or
// `engine: http`) is an HTTP entry: an exemption from the money-path guard's GET-only / money-verb-path
// rules for ONE route, bounded or not by the owner. Every other engine (amqp, mcp, db, logs) names what
// the owner allows a chain step or scenario of that engine to do; the fields for those kinds are
// `omitempty`, so a list of HTTP entries serialises BYTE-IDENTICALLY to what it did before they existed
// (the executor reports this JSON and an older control plane decodes it —
// TestMoneyWriteAllow_TodayShapedListSerialisesByteIdentically).
//
// ⛔ FAIL CLOSED ON THE WIRE. An entry that is not a well-formed entry of a kind this code knows
// matches NOTHING: an unknown engine, an HTTP entry with an empty method or path, an HTTP entry that
// carries an engine's fields. The reverse direction holds too — a control plane that predates the engine
// fields decodes a new-kind entry as an entry with an empty method and path, which its exact matcher can
// never equal (TestMoneyWriteAllow_OldControlPlaneNeverMatchesANewKind).
type MoneyWriteAllow struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Spends bool   `json:"spends"`
	// AmountField is a dotted path into the resolved JSON request body (e.g. "source_amount",
	// "order.amount") — required, and consulted, only when Spends is true.
	AmountField string `json:"amount_field,omitempty"`
	// MaxAmount is the per-request ceiling, in the field's own units. Required (> 0) when Spends.
	MaxAmount float64 `json:"max_amount,omitempty"`
	// MaxPerRun caps how many spends: true requests THIS entry may send in one run, summed across
	// every scenario the run executes (MoneySpendLedger). Required (>= 1) when Spends.
	MaxPerRun int `json:"max_per_run,omitempty"`

	// ── the entry kinds that are not HTTP (moneyengines.go) ──
	// Engine is "" or "http" for an HTTP entry, else "amqp" | "mcp" | "db" | "logs" | "cleanup" | "ui" | "fault".
	Engine string `json:"engine,omitempty"`
	// Op, Exchange, RoutingKey, Queue: an amqp entry — one op of the step and the names that op uses,
	// compared EXACTLY with the step's fields as written.
	Op         string `json:"op,omitempty"`
	Exchange   string `json:"exchange,omitempty"`
	RoutingKey string `json:"routing_key,omitempty"`
	Queue      string `json:"queue,omitempty"`
	// Stream: an `amqp` `listen` entry that names a bus step bound by `stream`. Compared EXACTLY with the
	// step's `stream`; never together with exchange / routing_key, and never matches a step bound by those.
	Stream string `json:"stream,omitempty"`
	// Target, Tool: an mcp entry (a key of targets.mcp_targets, or "" for the unnamed targets.mcp, and
	// one exact tool name); a db entry (a key of targets.database_targets, or "" for targets.database).
	Target string `json:"target,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Form: a cleanup entry ("sql" | "bash"). Spec: a ui entry (the spec path, exactly as the scenario or
	// the chain step names it). An engine: fault entry carries neither..
	Form string `json:"form,omitempty"`
	Spec string `json:"spec,omitempty"`
}

// Allow-entry engines. An entry with no engine is an HTTP entry.
const (
	AllowEngineHTTP = "http"
	AllowEngineAMQP = "amqp"
	AllowEngineMCP  = "mcp"
	AllowEngineDB   = "db"
	AllowEngineLogs = "logs"
	// a runnable ## CLEANUP block, a ui scenario / chain step, a fault chain step.
	AllowEngineCleanup = "cleanup"
	AllowEngineUI      = "ui"
	AllowEngineFault   = "fault"
)

// AllowEngines lists the engines an entry may name, in a stable order (for messages and docs).
func AllowEngines() []string {
	return []string{AllowEngineHTTP, AllowEngineAMQP, AllowEngineMCP, AllowEngineDB, AllowEngineLogs,
		AllowEngineCleanup, AllowEngineUI, AllowEngineFault}
}

// kind is the entry's engine with an absent one read as http.
func (e *MoneyWriteAllow) kind() string {
	if e.Engine == "" {
		return AllowEngineHTTP
	}
	return e.Engine
}

// hasEngineFields reports whether the entry carries any field only a non-HTTP entry has.
func (e *MoneyWriteAllow) hasEngineFields() bool {
	return e.Op != "" || e.Exchange != "" || e.RoutingKey != "" || e.Queue != "" || e.Stream != "" || e.Target != "" || e.Tool != "" ||
		e.Form != "" || e.Spec != ""
}

// isHTTP is true only for a WELL-FORMED HTTP entry: kind http, a method and a path, no engine fields.
// Anything else is not consulted by the HTTP matcher, so a malformed or foreign entry exempts nothing.
func (e *MoneyWriteAllow) isHTTP() bool {
	return e.kind() == AllowEngineHTTP && strings.TrimSpace(e.Method) != "" && e.Path != "" && !e.hasEngineFields()
}

// MoneyWriteAllowlist is the SUT's whole `money_writes.allow` block. nil/empty is today's behaviour
// EXACTLY: Match never returns a hit, so every non-GET / money-verb request is refused precisely as
// it was before this file existed.
type MoneyWriteAllowlist []MoneyWriteAllow

// IsPlaceholderSegment reports whether one path segment of an entry's `path` is a placeholder:
// `{name}` with name = a letter or underscore, then letters, digits and underscores. A placeholder is
// always a WHOLE segment; `x{id}` and `{id}x` are not placeholders (config refuses them by name).
func IsPlaceholderSegment(seg string) bool {
	if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' {
		return false
	}
	name := seg[1 : len(seg)-1]
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		if !(letter || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// PathHasPlaceholder reports whether any segment of an entry's path is a placeholder.
func PathHasPlaceholder(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if IsPlaceholderSegment(seg) {
			return true
		}
	}
	return false
}

// Match returns the HTTP entry that covers (method, rawURL) — or nil when nothing does. The method is
// compared case-insensitively; the path is taken from rawURL after stripping a leading "${VAR}"
// placeholder, "scheme://host[:port]", and any query string, and compared CASE-SENSITIVELY.
//
// ⛔ EXACT, EXCEPT FOR A WHOLE-SEGMENT PLACEHOLDER. An entry path is compared byte-for-byte with the
// request path, with one widening: a segment written `{name}` in the ENTRY matches exactly ONE
// non-empty segment of the request path (never `.` or `..`, never anything with a `/` in it). No
// prefix, no suffix, no wildcard, no trailing-slash normalization: a near-miss path is refused exactly
// like an unlisted one. An exact entry wins over a placeholder entry when both match.
// (Guarded by TestMoneyWriteAllowlist_ExactMatchOnly's HasPrefix mutation case and the placeholder
// tests in moneyallow_http_test.go.)
//
// A rawURL is the text of a FILE here: a segment that is still `${saved.id}` is one non-empty segment,
// so `/orders/${saved.id}` is covered by `/orders/{id}`. That is a judgement about the file only —
// what is SENT is judged again, on the resolved path, by MatchResolved.
func (al MoneyWriteAllowlist) Match(method, rawURL string) *MoneyWriteAllow {
	if len(al) == 0 {
		return nil
	}
	return al.match(method, requestPathOnly(rawURL), "")
}

// MatchResolved is Match for a request that is about to be SENT: u is the URL after every
// `${saved.<var>}` is bound. It consults the DECODED path (u.Path) exactly as the exact matcher always
// did at this door, and — because a placeholder segment is the one place a value that came from the SUT
// can change which route a request addresses — additionally refuses a placeholder segment whose ESCAPED
// form hides a slash (`%2F`, `%5C`) or is, once decoded, empty, `.` or `..`.
//
// ⛔ It does NOT pass u.Path through requestPathOnly: that strips a "?" and a "://", and a decoded
// `%3F` is data here, not a query. An empty u.Path is "/".
func (al MoneyWriteAllowlist) MatchResolved(method string, u *url.URL) *MoneyWriteAllow {
	if len(al) == 0 || u == nil {
		return nil
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return al.match(method, p, u.EscapedPath())
}

// match is the one matcher. esc, when non-empty, is the escaped form of p and is checked for the
// placeholder segments of a placeholder entry.
func (al MoneyWriteAllowlist) match(method, p, esc string) *MoneyWriteAllow {
	m := strings.ToUpper(strings.TrimSpace(method))
	// pass 1 — an exact entry wins.
	for i := range al {
		e := &al[i]
		if !e.isHTTP() || PathHasPlaceholder(e.Path) {
			continue
		}
		if entryCoversMethod(e, m) && e.Path == p {
			return e
		}
	}
	// pass 2 — a placeholder entry.
	for i := range al {
		e := &al[i]
		if !e.isHTTP() || !PathHasPlaceholder(e.Path) {
			continue
		}
		if entryCoversMethod(e, m) && placeholderPathMatches(e.Path, p, esc) {
			return e
		}
	}
	return nil
}

// entryCoversMethod: an entry covers exactly the one method it names. An entry that names none covers
// none (isHTTP already refuses it; this is the same fact held at the comparison).
func entryCoversMethod(e *MoneyWriteAllow, upperMethod string) bool {
	em := strings.ToUpper(strings.TrimSpace(e.Method))
	return em != "" && em == upperMethod
}

// placeholderPathMatches compares an entry path that holds placeholder segments with a request path,
// segment by segment: same number of segments, every literal segment byte-equal, every placeholder
// segment one non-empty segment that is not `.` or `..`. esc, when given, is the request's escaped path
// and must hold the same number of segments, with no hidden slash in a placeholder segment.
func placeholderPathMatches(entryPath, p, esc string) bool {
	es := strings.Split(entryPath, "/")
	ps := strings.Split(p, "/")
	if len(es) != len(ps) {
		return false
	}
	var raw []string
	if esc != "" {
		raw = strings.Split(esc, "/")
		if len(raw) != len(ps) {
			return false
		}
	}
	for i := range es {
		if !IsPlaceholderSegment(es[i]) {
			if es[i] != ps[i] {
				return false
			}
			continue
		}
		if ps[i] == "" || ps[i] == "." || ps[i] == ".." || strings.ContainsAny(ps[i], `/\`) {
			return false
		}
		if raw != nil {
			low := strings.ToLower(raw[i])
			if strings.Contains(low, "%2f") || strings.Contains(low, "%5c") {
				return false
			}
			dec, err := url.PathUnescape(raw[i])
			if err != nil || dec == "" || dec == "." || dec == ".." || strings.ContainsAny(dec, `/\`) {
				return false
			}
		}
	}
	return true
}

// requestPathOnly extracts the bare path a money_writes entry's `path` is compared against: strip a
// leading "${VAR}" base-URL placeholder, strip "scheme://host[:port]", and cut off any query string.
//
// ⚠ A DELIBERATE, SMALLER, SEPARATE COPY of config.PathFromURL's first two steps — internal/scenario
// cannot import internal/config (config already imports scenario; the reverse would cycle). Kept
// intentionally tiny, and it must be kept in step with PathFromURL if that stripping rule ever
// changes. Given an already-clean path (e.g. an execution door's *url.URL.Path) every step here is a
// deliberate no-op, so ONE function serves both the raw-text authoring doors and the resolved
// execution doors.
func requestPathOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "${") {
		if i := strings.Index(raw, "}"); i >= 0 {
			raw = raw[i+1:]
		}
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			raw = rest[j:]
		} else {
			raw = "/"
		}
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return raw
}

// EvaluateSpendAmount checks a RESOLVED request body against a spends:true entry, returning "" when
// the request may proceed or a sentence naming the entry and the value when it may not.
//
// ⛔ FAILS CLOSED on any ambiguity (item 3): a missing field, a non-numeric field (including a
// numeric-LOOKING string — JSON numbers are unquoted, and a quoted amount is not this field's
// declared type), or an unparseable body is refused, never treated as zero or as within budget.
func EvaluateSpendAmount(entry *MoneyWriteAllow, body string) string {
	if entry == nil || !entry.Spends {
		return ""
	}
	v, ok := numberAtDottedPath(body, entry.AmountField)
	if !ok {
		return fmt.Sprintf("%s %s: amount_field %q is missing or not a JSON number in the request body — refused (fail-closed)",
			entry.Method, entry.Path, entry.AmountField)
	}
	if v > entry.MaxAmount {
		return fmt.Sprintf("%s %s: amount_field %q is %v, over max_amount %v",
			entry.Method, entry.Path, entry.AmountField, v, entry.MaxAmount)
	}
	return ""
}

// numberAtDottedPath reads a JSON number at a dotted path ("a.b.c") inside a JSON object body. Any
// failure along the way (bad JSON, a missing segment, a non-object intermediate, a non-number leaf)
// answers ok=false — the caller (EvaluateSpendAmount) treats that as "refuse", never as zero.
func numberAtDottedPath(body, dotted string) (float64, bool) {
	dotted = strings.TrimSpace(dotted)
	if strings.TrimSpace(body) == "" || dotted == "" {
		return 0, false
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return 0, false
	}
	cur := root
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = m[seg]
		if !ok {
			return 0, false
		}
	}
	n, ok := cur.(float64)
	return n, ok
}

// MoneySpendLedger counts spends against money_writes allow entries, SHARED across every scenario in
// ONE run (item 3: "across all scenarios") — the zero value is ready to use. A nil *MoneySpendLedger
// always reserves successfully, so an execution door with no ledger to share (a door that does not
// run a whole RunAll, e.g. a single ad-hoc call) never has to nil-check before calling Reserve.
type MoneySpendLedger struct {
	mu    sync.Mutex
	spent map[string]int
}

// Reserve claims one spend against entry's max_per_run for this run. ok=false means this exact
// (method, path) entry has already spent max_per_run requests THIS RUN — the caller must not send
// the request that asked for the reservation.
func (l *MoneySpendLedger) Reserve(entry *MoneyWriteAllow) (ok bool, spentAfter int) {
	if l == nil || entry == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spent == nil {
		l.spent = map[string]int{}
	}
	key := strings.ToUpper(strings.TrimSpace(entry.Method)) + " " + entry.Path
	if l.spent[key] >= entry.MaxPerRun {
		return false, l.spent[key]
	}
	l.spent[key]++
	return true, l.spent[key]
}

// MoneyWriteAmountViolations is the VALIDATION-time half of item 3: a literal (no "${…}" anywhere in
// its body) spends:true request whose declared amount is ALREADY known to be missing, non-numeric,
// or over max_amount is refused at `argus validate-config`, exactly as MoneyGuardViolations refuses a
// structural violation — nothing here waits for a run to discover a bound-to-fail literal.
//
// A body carrying ANY "${…}" placeholder is left entirely to execution (the door that sees the
// resolved value): this function reports nothing for it, deliberately — a false "violation" computed
// from an unresolved placeholder would refuse configs that are perfectly fine at run time.
func MoneyWriteAmountViolations(s *Scenario, allow MoneyWriteAllowlist) []string {
	if len(allow) == 0 {
		return nil
	}
	var out []string
	check := func(where, method, url, body string) {
		entry := allow.Match(method, url)
		if entry == nil || !entry.Spends {
			return
		}
		if strings.Contains(body, "${") {
			return // not literal — execution enforces it against the resolved value
		}
		if msg := EvaluateSpendAmount(entry, body); msg != "" {
			out = append(out, fmt.Sprintf("%s: %s", where, msg))
		}
	}
	// The runner the scenario will get decides which requests it sends (RunnerKindOf, the dispatch's own
	// function): a `ui, chain` file is a chain, so its spending POST is checked here like any chain's.
	switch RunnerKindOf(s) {
	case RunnerMCP, RunnerUI, RunnerAMQPLoad:
		return nil
	case RunnerChain:
		steps, err := ParseChainSteps(s.Trigger.Payload)
		if err != nil {
			return nil // MoneyGuardViolations already reports the unreadable chain
		}
		for i, st := range steps {
			if st.Type != "http" {
				continue
			}
			name := st.Name
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			check("chain step "+name, st.Method, st.URL, string(st.Body))
		}
	default:
		check("the TRIGGER", s.Trigger.Method, s.Trigger.URL, s.Trigger.Payload)
	}
	return out
}
