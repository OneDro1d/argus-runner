package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// moneywrites.go — "money writes" (T5.4 follow-up, 2026-09-26 product decision): Argus is a
// monitoring kit and must be able to exercise REAL writes on a money-handling app, including real
// trades, when the SUT's TESTER declares exactly which writes and which limits. This file is the
// PURE core the money-path guard (moneyguard.go) and all three of its doors share: the (method,
// path) allowlist matcher, the resolved-body spend check, and the per-run spend ledger.
//
// Nothing here knows about YAML or config — internal/config parses `money_writes` into this
// package's types (config.MoneyWritesConfig, config/moneywrites.go) precisely because config already
// imports scenario and the reverse would cycle.

// MoneyWriteAllow is one entry of a SUT's `money_writes.allow` block: an EXACT (method, path)
// exemption from the money-path guard's GET-only / money-verb-path rules. A GET is never listed here
// (it needs no exemption); every listed method is a real write the SUT's own tester has bounded.
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
}

// MoneyWriteAllowlist is the SUT's whole `money_writes.allow` block. nil/empty is today's behaviour
// EXACTLY: Match never returns a hit, so every non-GET / money-verb request is refused precisely as
// it was before this file existed.
type MoneyWriteAllowlist []MoneyWriteAllow

// Match returns the entry whose method and path EXACTLY match (method compared case-insensitively;
// path compared byte-for-byte, case-SENSITIVE, after stripping a leading "${VAR}" placeholder,
// "scheme://host[:port]", and any query string from rawURL) — or nil when nothing matches.
//
// ⛔ EXACT ONLY. No prefix, no suffix, no trailing-slash normalization, no wildcards: a near-miss
// path is refused exactly like an unlisted one. (Guarded by TestMoneyWriteAllowlist_ExactMatchOnly's
// HasPrefix mutation case.)
func (al MoneyWriteAllowlist) Match(method, rawURL string) *MoneyWriteAllow {
	if len(al) == 0 {
		return nil
	}
	m := strings.ToUpper(strings.TrimSpace(method))
	p := requestPathOnly(rawURL)
	for i := range al {
		if strings.ToUpper(strings.TrimSpace(al[i].Method)) == m && al[i].Path == p {
			return &al[i]
		}
	}
	return nil
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
	switch {
	case contains(s.Tags, MCPTag), contains(s.Tags, UITag):
		return nil
	case contains(s.Tags, ChainTag):
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
