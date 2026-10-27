package config

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneywrites.go — the SUT config's OPTIONAL `money_writes` block, valid only beside
// `money_handling: true` (T5.4 follow-up, 2026-09-26; widened to every engine 2026-10-11).
//
// Argus is system-agnostic: it has no opinion about what a system's checks may do, and it does not
// care whether a system moves money. `money_handling: true` is a switch the system's OWNER sets in the
// owner's own config, and it means "Argus refuses what my list does not name". This block IS that list:
// every entry is the owner's decision. See internal/scenario/moneywrites.go for the pure allowlist/spend
// core this block is parsed INTO, moneyengines.go for the non-HTTP matchers, and moneyguard.go for the
// rule an entry exempts a request or step from.
//
//	money_writes:
//	  allow:
//	    - method: POST                       # an HTTP entry: no `engine`, or `engine: http`
//	      path: /api/v1/pay/quote
//	      spends: false
//	    - method: POST
//	      path: /api/v1/pay/order
//	      spends: true                       # bounded, per request and per run
//	      amount_field: pay_amount
//	      max_amount: 25
//	      max_per_run: 4
//	    - method: GET                        # a GET of a money-word path (spends must be false)
//	      path: /api/v1/orders/{id}          # {id} = exactly one non-empty segment
//	      spends: false
//	    - engine: amqp                       # one op of an amqp chain step, and the names it uses
//	      op: consume
//	      queue: reply.inbox
//	    - engine: mcp                        # one tool of one target (omit target = targets.mcp)
//	      target: ledger
//	      tool: get_balances
//	    - engine: db                         # a chain `db` step (omit target = targets.database)
//	      target: ledger
//	    - engine: logs                       # a chain `logs` step
//	    - engine: cleanup                    # a runnable ## CLEANUP block of this form (sql | bash)
//	      form: sql
//	    - engine: ui                         # a ui scenario / ui chain step that names this spec
//	      spec: tests/live/argus-declared.spec.ts
//	    - engine: fault                      # a fault chain step (fault_allowed_targets still bounds it)

// MoneyWriteAllowEntry is one parsed `money_writes.allow` entry, before conversion to
// scenario.MoneyWriteAllow. Spends is a *bool (not bool) so validate can tell "spends: false" (an
// explicit, valid declaration) apart from "spends" simply absent (a required field left out) — the
// same required-vs-zero-value distinction MoneyHandling's own doc discusses for money_handling.
// Exchange, RoutingKey and Queue are *string for the same reason: an amqp publish to the default
// exchange is written `exchange: ""`, which is a different statement from leaving the key out.
type MoneyWriteAllowEntry struct {
	Method      string  `yaml:"method"`
	Path        string  `yaml:"path"`
	Spends      *bool   `yaml:"spends"`
	AmountField string  `yaml:"amount_field,omitempty"`
	MaxAmount   float64 `yaml:"max_amount,omitempty"`
	MaxPerRun   int     `yaml:"max_per_run,omitempty"`

	// Engine is absent or "http" for an HTTP entry, else "amqp" | "mcp" | "db" | "logs" | "cleanup" | "ui" | "fault".
	Engine     string  `yaml:"engine,omitempty"`
	Op         string  `yaml:"op,omitempty"`
	Exchange   *string `yaml:"exchange,omitempty"`
	RoutingKey *string `yaml:"routing_key,omitempty"`
	Queue      *string `yaml:"queue,omitempty"`
	// Stream names a bus step's `stream`: `engine: amqp, op: listen, stream: <name>`. *string so that
	// `stream: ""` is refused rather than read as absent.
	Stream *string `yaml:"stream,omitempty"`
	Target string  `yaml:"target,omitempty"`
	Tool   string  `yaml:"tool,omitempty"`
	// Form is a cleanup entry's (sql | bash), Spec a ui entry's (the spec path).
	Form string `yaml:"form,omitempty"`
	Spec string `yaml:"spec,omitempty"`
}

// MoneyWritesConfig is the whole parsed `money_writes` block.
type MoneyWritesConfig struct {
	Allow []MoneyWriteAllowEntry `yaml:"allow"`
}

// engineOf reads an entry's engine; the second result is false for one this code does not know.
func (e MoneyWriteAllowEntry) engineOf() (string, bool) {
	eng := strings.TrimSpace(e.Engine)
	if eng == "" {
		return scenario.AllowEngineHTTP, true
	}
	for _, k := range scenario.AllowEngines() {
		if eng == k {
			return eng, true
		}
	}
	return eng, false
}

// validate refuses a money_writes block that is present without money_handling: true, empty, or
// carrying a malformed entry — named by index and field, exactly as RateLimit.validate/Package.validate
// refuse their own blocks at LOAD time (parseConfig), so validate-config (and onboarding, which dies
// on it) reports it, never a run. targets is the config's own targets block: an mcp or db entry may
// only name a target the config declares.
func (m *MoneyWritesConfig) validate(moneyHandling bool, targets *Targets) error {
	if m == nil {
		return nil
	}
	if !moneyHandling {
		return fmt.Errorf("money_writes is declared but money_handling is not true — money_writes only " +
			"relaxes the money-path guard on a SUT that has turned it on; set money_handling: true or " +
			"remove the money_writes block")
	}
	if len(m.Allow) == 0 {
		return fmt.Errorf("money_writes.allow is empty — declare at least one entry or omit the money_writes block entirely")
	}
	seen := map[string]int{}
	for i, e := range m.Allow {
		where := fmt.Sprintf("money_writes.allow[%d]", i)
		eng, known := e.engineOf()
		if !known {
			return fmt.Errorf("%s: engine %q is not one of %s", where, eng, strings.Join(scenario.AllowEngines(), ", "))
		}
		var key string
		var err error
		if eng == scenario.AllowEngineHTTP {
			key, err = e.validateHTTP(where)
		} else {
			key, err = e.validateEngine(where, eng, targets)
		}
		if err != nil {
			return err
		}
		if first, dup := seen[key]; dup {
			return fmt.Errorf("%s: duplicates money_writes.allow[%d] — an entry is listed once", where, first)
		}
		seen[key] = i
	}
	return nil
}

// validateHTTP checks an HTTP entry and returns its duplicate key (method + the path with every
// placeholder name erased, since `/orders/{id}` and `/orders/{order}` cover the same requests).
func (e MoneyWriteAllowEntry) validateHTTP(where string) (string, error) {
	if fs := e.engineFieldsSet(); len(fs) > 0 {
		return "", fmt.Errorf("%s: %s is a field of an engine entry (amqp, mcp, db, logs, cleanup, ui, fault), and this is an HTTP entry "+
			"(engine absent or http) — set `engine:` to the engine you mean, or remove %s", where, fs[0], fs[0])
	}
	method := strings.ToUpper(strings.TrimSpace(e.Method))
	if method == "" {
		return "", fmt.Errorf("%s: method is required", where)
	}
	if strings.TrimSpace(e.Path) == "" {
		return "", fmt.Errorf("%s: path is required", where)
	}
	if strings.ContainsAny(e.Path, "*?") {
		return "", fmt.Errorf("%s: path %q must be an exact path — no wildcards (\"*\") and no query string (\"?\"); "+
			"a whole segment may be a placeholder written {name}", where, e.Path)
	}
	if !strings.HasPrefix(e.Path, "/") {
		return "", fmt.Errorf("%s: path %q must start with \"/\"", where, e.Path)
	}
	shape := make([]string, 0, 8)
	for _, seg := range strings.Split(e.Path, "/") {
		switch {
		case scenario.IsPlaceholderSegment(seg):
			shape = append(shape, "{}")
		case strings.Contains(seg, "${"):
			return "", fmt.Errorf("%s: path %q: a path built at run time (${…}) cannot be listed — write the path as it is, "+
				"with {name} for a segment that varies", where, e.Path)
		case strings.ContainsAny(seg, "{}"):
			return "", fmt.Errorf("%s: path %q segment %q: a placeholder must be a whole segment written {name} "+
				"(letters, digits and underscores, not starting with a digit) — a partial segment such as x{id} is not allowed",
				where, e.Path, seg)
		default:
			shape = append(shape, seg)
		}
	}
	if e.Spends == nil {
		return "", fmt.Errorf("%s: spends is required (true or false)", where)
	}
	if method == "GET" && *e.Spends {
		return "", fmt.Errorf("%s: a GET entry must be spends: false — a GET is a read, and Argus bounds an amount only in the "+
			"JSON body of a write", where)
	}
	if *e.Spends {
		if strings.TrimSpace(e.AmountField) == "" {
			return "", fmt.Errorf("%s: spends: true requires amount_field", where)
		}
		if e.MaxAmount <= 0 {
			return "", fmt.Errorf("%s: spends: true requires max_amount > 0", where)
		}
		if e.MaxPerRun < 1 {
			return "", fmt.Errorf("%s: spends: true requires max_per_run >= 1", where)
		}
	} else if e.AmountField != "" || e.MaxAmount != 0 || e.MaxPerRun != 0 {
		return "", fmt.Errorf("%s: spends: false must not set amount_field/max_amount/max_per_run", where)
	}
	return "http " + method + " " + strings.Join(shape, "/"), nil
}

// engineFieldsSet lists, in a stable order, the engine-only fields this entry sets.
func (e MoneyWriteAllowEntry) engineFieldsSet() []string {
	var out []string
	if e.Op != "" {
		out = append(out, "op")
	}
	if e.Exchange != nil {
		out = append(out, "exchange")
	}
	if e.RoutingKey != nil {
		out = append(out, "routing_key")
	}
	if e.Queue != nil {
		out = append(out, "queue")
	}
	if e.Stream != nil {
		out = append(out, "stream")
	}
	if e.Target != "" {
		out = append(out, "target")
	}
	if e.Tool != "" {
		out = append(out, "tool")
	}
	if e.Form != "" {
		out = append(out, "form")
	}
	if e.Spec != "" {
		out = append(out, "spec")
	}
	return out
}

// validateEngine checks an amqp | mcp | db | logs entry and returns its duplicate key.
func (e MoneyWriteAllowEntry) validateEngine(where, eng string, targets *Targets) (string, error) {
	// the HTTP half of the entry has no meaning here
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"method", strings.TrimSpace(e.Method) != ""},
		{"path", e.Path != ""},
	} {
		if f.set {
			return "", fmt.Errorf("%s: %s is a field of an HTTP entry, and this entry is engine: %s — remove %s", where, f.name, eng, f.name)
		}
	}
	if e.Spends != nil || e.AmountField != "" || e.MaxAmount != 0 || e.MaxPerRun != 0 {
		return "", fmt.Errorf("%s: spends, amount_field, max_amount and max_per_run apply to HTTP entries only — an engine: %s "+
			"entry is all-or-nothing for what it names; Argus cannot cap it the way max_amount caps an HTTP body", where, eng)
	}
	for _, v := range []string{e.Op, e.Target, e.Tool, e.Form, e.Spec} {
		if strings.Contains(v, "${") {
			return "", fmt.Errorf("%s: %q: a name built at run time (${…}) cannot be listed", where, v)
		}
	}
	for _, p := range []*string{e.Exchange, e.RoutingKey, e.Queue, e.Stream} {
		if p != nil && strings.Contains(*p, "${") {
			return "", fmt.Errorf("%s: %q: a name built at run time (${…}) cannot be listed", where, *p)
		}
	}
	set := map[string]bool{}
	for _, f := range e.engineFieldsSet() {
		set[f] = true
	}
	allowed := map[string]bool{}
	switch eng {
	case scenario.AllowEngineAMQP:
		if e.Op == "" {
			return "", fmt.Errorf("%s: an amqp entry requires op (one of %s)", where, strings.Join(scenario.AMQPOps(), ", "))
		}
		names, ok := scenario.AMQPAllowNameFields(e.Op)
		if !ok {
			return "", fmt.Errorf("%s: op %q is not an amqp op (one of %s)", where, e.Op, strings.Join(scenario.AMQPOps(), ", "))
		}
		allowed["op"] = true
		if e.Stream != nil {
			// a stream entry names a bus step bound by `stream`, as a listen, and nothing else
			if e.Op != "listen" {
				return "", fmt.Errorf("%s: stream is valid only with op: listen (op: %s does not use it) — remove it", where, e.Op)
			}
			if e.Exchange != nil || e.RoutingKey != nil {
				return "", fmt.Errorf("%s: stream together with exchange or routing_key is ambiguous — a stream entry names a bus step "+
					"bound by `stream`, an exchange + routing_key entry names one bound by those; write two entries or keep one", where)
			}
			if strings.TrimSpace(*e.Stream) == "" {
				return "", fmt.Errorf("%s: op: listen with stream requires a non-empty stream", where)
			}
			allowed["stream"] = true
			names = nil // the exchange and routing key are the stream's, resolved only by the executor
		}
		for _, n := range names {
			allowed[n] = true
			if !set[n] {
				return "", fmt.Errorf("%s: op: %s requires %s — %s names %s", where, e.Op, n, e.Op, strings.Join(names, ", "))
			}
		}
		// a name the op uses is non-empty, except a publish to the default exchange, written exchange: ""
		for n, p := range map[string]*string{"exchange": e.Exchange, "routing_key": e.RoutingKey, "queue": e.Queue} {
			if p != nil && *p == "" && allowed[n] && !(e.Op == "publish" && n == "exchange") {
				return "", fmt.Errorf("%s: op: %s requires a non-empty %s", where, e.Op, n)
			}
		}
	case scenario.AllowEngineMCP:
		allowed["target"], allowed["tool"] = true, true
		if strings.TrimSpace(e.Tool) == "" {
			return "", fmt.Errorf("%s: an mcp entry requires tool — the exact tool name", where)
		}
		if e.Target != "" && (targets == nil || targets.MCPTargets[e.Target] == nil) {
			return "", fmt.Errorf("%s: target %q is not declared under targets.mcp_targets", where, e.Target)
		}
	case scenario.AllowEngineDB:
		allowed["target"] = true
		if e.Target != "" && (targets == nil || targets.DatabaseTargets[e.Target] == nil) {
			return "", fmt.Errorf("%s: target %q is not declared under targets.database_targets", where, e.Target)
		}
	case scenario.AllowEngineLogs:
		// nothing else to name
	case scenario.AllowEngineCleanup:
		allowed["form"] = true
		if strings.TrimSpace(e.Form) == "" {
			return "", fmt.Errorf("%s: a cleanup entry requires form (one of %s)", where, strings.Join(scenario.CleanupForms(), ", "))
		}
		known := false
		for _, f := range scenario.CleanupForms() {
			if e.Form == f {
				known = true
			}
		}
		if !known {
			return "", fmt.Errorf("%s: form %q is not a runnable cleanup form (one of %s)", where, e.Form, strings.Join(scenario.CleanupForms(), ", "))
		}
	case scenario.AllowEngineUI:
		allowed["spec"] = true
		if strings.TrimSpace(e.Spec) == "" {
			return "", fmt.Errorf("%s: a ui entry requires spec — the spec path exactly as the scenario or chain step names it", where)
		}
	case scenario.AllowEngineFault:
		// nothing else to name: what a fault may touch is fault_allowed_targets
	}
	for _, f := range e.engineFieldsSet() {
		if !allowed[f] {
			if eng == scenario.AllowEngineAMQP {
				return "", fmt.Errorf("%s: op: %s does not use %s — remove it", where, e.Op, f)
			}
			return "", fmt.Errorf("%s: %s is not a field of an engine: %s entry — remove it", where, f, eng)
		}
	}
	return "engine " + eng + " " + e.Op + "|" + deref(e.Exchange) + "|" + deref(e.RoutingKey) + "|" + deref(e.Queue) + "|stream=" + deref(e.Stream) + "|" +
		e.Target + "|" + e.Tool + "|" + e.Form + "|" + e.Spec, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// allowlist converts the parsed block into the pure type moneyguard.go / argus / chain consult. nil
// (no block declared) converts to nil — MoneyGuardViolations/Match already treat that as "exempt
// nothing", today's behaviour exactly.
func (m *MoneyWritesConfig) allowlist() scenario.MoneyWriteAllowlist {
	if m == nil || len(m.Allow) == 0 {
		return nil
	}
	out := make(scenario.MoneyWriteAllowlist, len(m.Allow))
	for i, e := range m.Allow {
		eng, _ := e.engineOf()
		if eng == scenario.AllowEngineHTTP {
			// an HTTP entry carries no engine fields: it serialises exactly as it did before they existed
			out[i] = scenario.MoneyWriteAllow{
				Method:      strings.ToUpper(strings.TrimSpace(e.Method)),
				Path:        e.Path,
				Spends:      e.Spends != nil && *e.Spends,
				AmountField: e.AmountField,
				MaxAmount:   e.MaxAmount,
				MaxPerRun:   e.MaxPerRun,
			}
			continue
		}
		out[i] = scenario.MoneyWriteAllow{
			Engine:     eng,
			Op:         e.Op,
			Exchange:   deref(e.Exchange),
			RoutingKey: deref(e.RoutingKey),
			Queue:      deref(e.Queue),
			Stream:     deref(e.Stream),
			Target:     e.Target,
			Tool:       e.Tool,
			Form:       e.Form,
			Spec:       e.Spec,
		}
	}
	return out
}

// MoneyWriteAllowlist is the exported accessor every door outside this package (argus, control,
// reporoute) uses to reach c's money_writes allowlist — never c.MoneyWrites directly, so the
// conversion stays in ONE place.
func (c *Config) MoneyWriteAllowlist() scenario.MoneyWriteAllowlist {
	return c.MoneyWrites.allowlist()
}
