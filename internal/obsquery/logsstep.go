package obsquery

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// THE READ BEHIND A CHAIN `logs` STEP: the LogQL it sends and the one call that sends it.
//
// ⛔ A SCENARIO CANNOT NAME A URL. The step reads through *Loki, which the executor built from its own --loki,
// --loki-tenant and observability.loki.credential (toolcore.lokiFor); nothing in a step reaches BaseURL.
// ⛔ EVERY AUTHORED VALUE IS A QUOTED LITERAL. A label value, `contains` and `matching` go through logqlString
// (strconv.Quote), so a quote, a backslash, a backtick or a newline cannot end the literal or open a second
// selector. A label NAME is the only thing rendered bare, and it is checked against the Loki label grammar here
// as well as at authoring (the second lock, as did for the correlation id).

// LogsLineLimit is the most lines one logs-step read asks for. A read that returns this many is LOSSY (Loki
// stopped at the limit), and the step must not report that count as a number.
const LogsLineLimit = 2000

// LogsRead is the outcome of one logs-step read: the lines in time order, the limit the query used, or why it
// failed. The lines are SandboxLine, the same (timestamp, raw line) pair, because that is what Loki returns.
type LogsRead struct {
	Lines []SandboxLine
	Limit int   // the query's line limit; a read that returns this many is `lossy`
	Err   error // why the read failed (no URL, transport, HTTP status, decode); nil when Loki answered
	// NoStream is set only by a SHARED Loki, only when the read returned no line: the stream
	// selector ALONE (cluster, namespace and the labels the step named, no line filter, no parse stage) matched
	// nothing in the window either. Then the empty answer says nothing about the line the step looked for - the
	// cluster or namespace label may be wrong, or the shared promtail is not delivering that namespace - and a
	// `count == 0` claim must not be judged on it. Always false for an instance's own Loki and for a fake.
	NoStream bool
	// Unparsed (live check 2026-10-19) is set only by a SHARED Loki, only when the read returned no
	// line, the query filtered on a label its parser stage derives, the selector DOES hold lines, and the parser
	// stage reads NONE of them (see parsehealth.go). Then the empty answer says nothing about the line the step
	// looked for - no line of this SUT yields a level / correlation_id / event_type in the declared format - and a
	// `count == 0` claim must not be judged on it. Nil otherwise (some lines parse: the zero is real).
	Unparsed *UnparsedLines
}

// LogsReader is the logs step's one dependency. *Loki implements it; tests use a fake or an httptest Loki.
type LogsReader interface {
	ReadLogs(query string, from, to time.Time) LogsRead
}

var _ LogsReader = (*Loki)(nil)

// lokiLabelNameRe is a Loki label name.
var lokiLabelNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CorrelationLogQL is the correlation-id query: every stream but the observability plane's own (otherwise Loki
// matches its own log of this very query), lines holding the id, then the optional `contains` / `matching`
// filters. With both empty it is exactly the query queryLines has always sent.
func CorrelationLogQL(correlationID, contains, matching string) string {
	return `{service=~".+",service!~"` + ObsInfraExclusion + `"} |= ` + logqlString(correlationID) + lineFilters(contains, matching)
}

// SourceLogQL is the source query: the streams the labels select (equality only, names sorted so the query is
// deterministic), then the optional filters. No label, a label name outside the Loki grammar, or an empty value
// (which would match every stream WITHOUT that label) is an error and nothing is sent.
func SourceLogQL(labels map[string]string, contains, matching string) (string, error) {
	if len(labels) == 0 {
		return "", errors.New("a source query needs at least one label")
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		if !lokiLabelNameRe.MatchString(k) {
			return "", fmt.Errorf("label name %s is not a Loki label name", strconv.Quote(k))
		}
		if labels[k] == "" {
			return "", fmt.Errorf("label %s has an empty value, which would match every stream without it", k)
		}
		parts = append(parts, k+"="+logqlString(labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}" + lineFilters(contains, matching), nil
}

// lineFilters renders the optional `|= "<contains>"` and `|~ "<matching>"`, in that order.
func lineFilters(contains, matching string) string {
	var b strings.Builder
	if contains != "" {
		b.WriteString(" |= " + logqlString(contains))
	}
	if matching != "" {
		b.WriteString(" |~ " + logqlString(matching))
	}
	return b.String()
}

// ReadLogs sends one query_range for [from, to] with LogsLineLimit and returns the lines in time order (Loki
// groups them by stream). It is the only Loki call a logs step makes: the executor's own URL, tenant header and
// credential (queryRange), and an answer that does not say status "success" is an error, never zero lines.
func (l *Loki) ReadLogs(query string, from, to time.Time) LogsRead {
	read := LogsRead{Limit: LogsLineLimit}
	if strings.TrimSpace(l.BaseURL) == "" {
		read.Err = errors.New("no Loki URL for this executor (--loki is empty)")
		return read
	}
	entries, err := l.queryRangeChecked(query, from, to, LogsLineLimit, true)
	if err != nil {
		read.Err = err
		return read
	}
	if l.Shared != nil && len(entries) == 0 {
		// A shared Loki cannot tell "the line is not there" from "nothing of this namespace is there" by the count
		// alone, so ask once whether the selector sees any stream at all (one line is enough).
		sel, ok := leadingSelector(query)
		if !ok {
			read.Err = errors.New("the shared-Loki query has no stream selector to probe")
			return read
		}
		probe, perr := l.queryRangeChecked(sel, from, to, 1, true)
		if perr != nil {
			read.Err = perr
			return read
		}
		read.NoStream = len(probe) == 0
		if !read.NoStream {
			// The stream exists. If the query filters on a parsed label, ask whether the parser reads ANY of its lines.
			un, perr := l.parseHealth(query, from, to)
			if perr != nil {
				read.Err = perr
				return read
			}
			read.Unparsed = un
		}
	}
	ts := func(e lokiEntry) int64 {
		n, _ := strconv.ParseInt(e.TS, 10, 64)
		return n
	}
	sort.SliceStable(entries, func(i, j int) bool { return ts(entries[i]) < ts(entries[j]) })
	for _, e := range entries {
		read.Lines = append(read.Lines, SandboxLine{TS: e.TS, Line: e.Line})
	}
	return read
}
