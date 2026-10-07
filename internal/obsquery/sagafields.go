package obsquery

import (
	"encoding/json"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// ─────────────────────────────────────────────────────────────────────────────────────────
// The SUT-agnostic line reader (2026-07-22, GAP-1/GAP-2).
//
// Two defects, both found by EVALUATING two independent failure-triage documents that had each
// concluded "our SUT's instrumentation is broken". It was ours:
//
//   GAP-2  every Loki line was json.Unmarshal'd and DROPPED on error, so a logfmt SUT (Memstore
//          logs via Go's stdlib logger) was invisible end-to-end — no saga, no parsed fields.
//   GAP-1  the saga node was built from HARDCODED the operator key names (saga_id/step/step_name/
//          step_status/ts/...). The per-SUT translation table declared only 4 fields and never
//          covered these. Social marks its saga lines correctly (event=tool_dispatch) but names
//          its step fields msg/time/level — of the 10 keys we wanted, only `service` intersected,
//          so we produced available:true with every semantic field empty.
//
// The third element here is the one that actually protects the user: an empty-content timeline is
// now reported DEGRADED, never as a clean available:true. A blind panel that says "healthy" is
// worse than an absent one — it defeats exactly the automated checks meant to catch it.
// ─────────────────────────────────────────────────────────────────────────────────────────

// parseKV parses a log line into a field map. JSON first (the canonical the operator form, unchanged);
// then logfmt (`k=v`, `k="quoted v"`). ok=false when the line yields no pairs at all, so a prose
// line is reported unparsed rather than as a junk map.
//
// prefix returns logfmt's LEADING non-pair text — Go's stdlib logger writes
// `2026/07/16 19:28:43 INFO telemetry.event event_type=… service=…`, where the human-readable part
// is not a k=v pair at all. Returning it separately lets the log view use it as the message
// WITHOUT changing the JSON path's contract (a JSON line has no prefix, so `msg` stays exactly as
// before — including staying EMPTY for a non-saga line with no msg key, which is a tested rule).
func parseKV(line string) (m map[string]any, prefix string, ok bool) {
	return parseKVFormat(line, true)
}

// parseKVFormat is parseKV under the SUT's DECLARED log encoding (#422): JSON first always; the
// logfmt fallback runs only when logfmt is declared. On a JSON-declared SUT a non-JSON line yields
// ok=false, so a plain-text line that merely ECHOES `correlation_id=<cid> event_type=saga ...`
// (a stdlib log.Printf of a query string) is not read as a saga step. This is exactly the rule
// templates/saga-presence.jmx applies to the verdict (parseLine), so reader and verdict agree.
func parseKVFormat(line string, logfmt bool) (m map[string]any, prefix string, ok bool) {
	if json.Unmarshal([]byte(line), &m) == nil && m != nil {
		return m, "", true
	}
	if !logfmt {
		return nil, "", false
	}
	m = map[string]any{}
	pairs, pre := splitLogfmt(line)
	for _, k := range pairs {
		m[k.key] = k.val
	}
	return m, pre, len(m) > 0
}

type kvPair struct{ key, val string }

// splitLogfmt scans key=value pairs, honouring double-quoted values (with \" escapes). Anything
// that is not a well-formed pair is skipped rather than guessed at. prefix is the leading text
// before the FIRST pair (a stdlib-logger date/level/message header), trimmed.
func splitLogfmt(s string) (pairs []kvPair, prefix string) {
	var out []kvPair
	firstKeyStart := -1
	for i := 0; i < len(s); {
		// find the next '=' that has a plausible bare key to its left
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		eq += i
		// walk back over the key
		ks := eq
		for ks > i && !isSpace(s[ks-1]) {
			ks--
		}
		key := s[ks:eq]
		j := eq + 1
		var val string
		if j < len(s) && s[j] == '"' {
			j++
			var b strings.Builder
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					j++
					break
				}
				b.WriteByte(s[j])
				j++
			}
			val = b.String()
		} else {
			vs := j
			for j < len(s) && !isSpace(s[j]) {
				j++
			}
			val = s[vs:j]
		}
		if key != "" && validKey(key) {
			if firstKeyStart < 0 {
				firstKeyStart = ks
			}
			out = append(out, kvPair{key, val})
		}
		i = j
	}
	if firstKeyStart > 0 {
		prefix = strings.TrimSpace(s[:firstKeyStart])
	}
	return out, prefix
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// validKey keeps the scanner from inventing keys out of prose or timestamps: a logfmt key is
// letters/digits/_/-/. and must start with a letter or underscore.
func validKey(k string) bool {
	c := k[0]
	if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && c != '_' {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// stepFieldFallbacks are tried IN ORDER when a step field is not declared and the canonical the operator
// name is absent — so a SUT gets a readable saga BEFORE anyone edits its argus-config. Declaring
// saga_step_fields is still the exact contract; this is the graceful default.
var stepFieldFallbacks = map[string][]string{
	"saga_id":     {"saga_id", "trace_id"},
	"step":        {"step", "step_index"},
	"step_name":   {"step_name", "msg", "message", "event", "name"},
	"step_status": {"step_status", "status", "outcome"},
	"timestamp":   {"ts", "time", "timestamp", "@timestamp"},
	"error":       {"error", "err"},
	"service":     {"service", "service_name", "component"},
}

// stepFieldNames is the ORDERED list of keys a logical step field is read from: the SUT's declared
// name alone, else the fallback chain. It is the single source for stepField (the Go reader) and
// for SagaJudgeFields (the names handed to saga-presence.jmx), so the two cannot drift.
func (l *Loki) stepFieldNames(logical string) []string {
	if name := l.SagaStepFields[logical]; name != "" {
		return []string{name}
	}
	return stepFieldFallbacks[logical]
}

// stepField resolves ONE logical step field: the SUT's declared name wins; else the fallback
// chain; else "". The first candidate with a NON-EMPTY value counts.
func (l *Loki) stepField(m map[string]any, logical string) string {
	for _, cand := range l.stepFieldNames(logical) {
		if v, ok := m[cand]; ok {
			if s := str(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// SagaJudgeFields is everything saga-presence.jmx needs to judge ONE parsed log line the way the
// Go reader would: which key carries the correlation id, which key tags a saga line and with what
// value(s), and which key(s) carry the step name. Resolved with the reader's own defaults.
type SagaJudgeFields struct {
	CorrelationField string
	SagaField        string
	SagaValues       []string
	StepNameFields   []string // tried in order; the first non-empty value is the step name
}

// SagaJudgeFields resolves the declared names (and the defaults/fallbacks) for the JMX template.
func (l *Loki) SagaJudgeFields() SagaJudgeFields {
	return SagaJudgeFields{
		CorrelationField: l.corrField(),
		SagaField:        l.sagaField(),
		SagaValues:       l.sagaValues(),
		StepNameFields:   l.stepFieldNames("step_name"),
	}
}

// logfmtDeclared reports whether the SUT declared observability.loki.log_format: logfmt. Anything
// else (unset, json, a typo) is json, the default; config.LogFormat applies the same rule.
func (l *Loki) logfmtDeclared() bool {
	return strings.EqualFold(strings.TrimSpace(l.LogFormat), "logfmt")
}

// parse is the Loki reader's one line parser: parseKV under the declared log_format.
func (l *Loki) parse(line string) (map[string]any, string, bool) {
	return parseKVFormat(line, l.logfmtDeclared())
}

// sagaStep parses one line and, if it carries the DECLARED saga marker, builds the step from the
// declared/fallback field names. ok=false when the line is unparseable or is not a saga line.
func (l *Loki) sagaStep(line string) (failcontext.SagaStep, bool) {
	m, _, ok := l.parse(line)
	if !ok {
		return failcontext.SagaStep{}, false
	}
	if !l.isSagaMarker(str(m[l.sagaField()])) {
		return failcontext.SagaStep{}, false
	}
	return failcontext.SagaStep{
		SagaID:     l.stepField(m, "saga_id"),
		Step:       toInt(m[orDefaultKey(l.SagaStepFields["step"], "step")]),
		StepName:   l.stepField(m, "step_name"),
		Service:    l.stepField(m, "service"),
		Timestamp:  l.stepField(m, "timestamp"),
		StepStatus: l.stepField(m, "step_status"),
		Error:      l.stepField(m, "error"),
		Fields: failcontext.SagaFields{
			What: strPtr(m["what"]), Why: strPtr(m["why"]), ByWhom: strPtr(m["by_whom"]),
		},
	}, true
}

func orDefaultKey(declared, def string) string {
	if declared != "" {
		return declared
	}
	return def
}

// markDegradedIfContentFree is the honesty gate. A timeline whose nodes carry no step name, no
// status and no timestamp is not evidence of a healthy saga — it is evidence that we could not
// READ the SUT's saga lines. Reporting available:true there is the "green No-data panel is blind,
// not all-clear" failure mode, and it is what made an entire triage round fall back to raw logs
// without knowing why. Say so, and name the remedy.
func markDegradedIfContentFree(saga *failcontext.Saga, l *Loki) {
	if len(saga.Timeline) == 0 {
		return
	}
	for _, s := range saga.Timeline {
		if s.StepName != "" || s.StepStatus != "" || s.Timestamp != "" {
			return // at least one node is readable — nothing to report
		}
	}
	saga.Available = false
	saga.Note = "saga lines WERE found for this correlation_id (" + itoa(len(saga.Timeline)) +
		" matched the declared marker " + l.sagaField() + "=" + l.sagaValue() +
		") but none of their step fields could be read — every step_name, step_status and timestamp is empty. " +
		"This SUT names its saga step fields differently: declare them in argus-config under " +
		"observability.loki.saga_step_fields (step_name / step_status / timestamp / saga_id / step / error). " +
		"Treat this as UNREADABLE, not as a healthy saga."
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
