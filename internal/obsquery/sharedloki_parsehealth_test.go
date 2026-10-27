package obsquery_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/OneDro1d/argus-runner/internal/obsquery"
)

// Live check 2026-10-19 - the SUT logs PLAIN TEXT, so `| json` parsed 0 of 9,521 lines and a
// `level=error` claim with `count == 0` would have passed vacuously. These tests hold: (1) the parse-health probe,
// (2) the __error__ filter on every metric query with a parser, (3) the plain-text (`| regexp`) format.

// The real line shape, measured against the shared Loki (content shortened; no value of a real line is kept).
const (
	plainWarn  = `10:06:24.852569 main-engine WARN: [PollTick] skipping duplicate ack`
	plainInfo  = `10:06:25.101113 main-engine INFO: [PollTick] tick complete`
	plainError = `10:06:26.000001 main-engine ERROR: [Settle] order rejected`
	// the RE2 expression a plain-text instance declares as observability.loki.log_regex
	plainRegex = `^(?P<time>\S+) (?P<service>\S+) (?P<level>[A-Z]+): `
)

// simShared is a shared Loki that EVALUATES what it is asked: log queries (query_range) and the probe's
// `sum(count_over_time(...))` (query) both go through the simulator. Like the real one it REJECTS a metric query
// with a json/logfmt parser and no `__error__` filter (measured: HTTP 400 `pipeline error: 'JSONParserErr' ...
// Use a label filter to intentionally skip this error`).
type simShared struct {
	mu      sync.Mutex
	streams []simStream
	log     []string // log queries
	metric  []string // metric queries
}

var rangeSuffixRe = regexp.MustCompile(` \[(\d+)s\]\)\)$`)

func (s *simShared) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		q := r.URL.Query().Get("query")
		if strings.HasSuffix(r.URL.Path, "/query") {
			s.metric = append(s.metric, q)
			m := rangeSuffixRe.FindStringSubmatchIndex(q)
			if !strings.HasPrefix(q, "sum(count_over_time(") || m == nil {
				http.Error(w, "parse error: not a shape the simulator knows", 400)
				return
			}
			inner := q[len("sum(count_over_time("):m[0]]
			if (strings.Contains(inner, " | json") || strings.Contains(inner, " | logfmt")) && !strings.Contains(inner, `| __error__=""`) {
				http.Error(w, `pipeline error: 'JSONParserErr' for series '{}'. Use a label filter to intentionally skip this error. (e.g | __error__!="JSONParserErr")`, 400)
				return
			}
			n := len(simQuery(t, inner, s.streams))
			var result string
			if n > 0 {
				result = fmt.Sprintf(`{"metric":{},"value":[1760868000.0,"%d"]}`, n)
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, result)
			return
		}
		s.log = append(s.log, q)
		var vals []string
		for i, l := range simQuery(t, q, s.streams) {
			vals = append(vals, fmt.Sprintf(`["%d",%s]`, 1000+i, strconv.Quote(l)))
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[`+strings.Join(vals, ",")+`]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *simShared) metricQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.metric...)
}

func arbStream(lines ...string) []simStream {
	return []simStream{{labels: map[string]string{"cluster": "cluster-a", "namespace": "sut-ns", "container": "main-engine", "pod": "main-engine-0"}, lines: lines}}
}

func plainSource() *SharedLogSource {
	s := shSource()
	s.Regexp = plainRegex
	return s
}

var (
	pwFrom = time.Unix(1760864400, 0)
	pwTo   = time.Unix(1760868000, 0) // one hour
)

func levelQuery(t *testing.T, src *SharedLogSource, level string) string {
	t.Helper()
	q, err := src.SourceLogQL(map[string]string{"service": "main-engine", "level": level}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// ── (1) the parse-health probe ───────────────────────────────────────────────────────────────────────

// THE LIVE FINDING, reproduced: lines of the real plain-text shape, a json-format instance, `level=error`.
func TestParseHealth_PlainTextLinesAgainstTheJSONFormatIsUnparsed(t *testing.T) {
	sim := &simShared{streams: arbStream(plainWarn, plainInfo, plainError)}
	l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()} // Format json
	q := levelQuery(t, shSource(), "error")
	t.Logf("shared-mode LogQL, level=error claim on the json format:\n%s", q)

	read := l.ReadLogs(q, pwFrom, pwTo)
	if read.Err != nil {
		t.Fatal(read.Err)
	}
	if len(read.Lines) != 0 {
		t.Fatalf("json parses none of these lines, so level=error selects none: %v", read.Lines)
	}
	if read.Unparsed == nil || read.Unparsed.Format != "json" || read.Unparsed.Lines != 3 {
		t.Fatalf("want Unparsed{json, 3 lines} (lines exist, none parse), got %+v", read.Unparsed)
	}
	if read.NoStream {
		t.Fatal("the stream exists")
	}
	// the probe is two metric queries over the SAME selector and the window, the parsed one with __error__
	mq := sim.metricQueries()
	sel := `{cluster="cluster-a",container="main-engine",namespace="sut-ns"}`
	wantTotal := `sum(count_over_time(` + sel + ` [3600s]))`
	wantParsed := `sum(count_over_time(` + sel + ` | json level="level" | __error__="" [3600s]))`
	if !reflect.DeepEqual(mq, []string{wantTotal, wantParsed}) {
		t.Fatalf("probe queries:\n got %q\nwant %q", mq, []string{wantTotal, wantParsed})
	}
}

// Some lines parse: the zero is a real zero and is kept.
func TestParseHealth_SomeLinesParseKeepsTheZero(t *testing.T) {
	good := `{"level":"info","msg":"fine"}`
	sim := &simShared{streams: arbStream(plainWarn, good, plainInfo)}
	l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()}
	read := l.ReadLogs(levelQuery(t, shSource(), "error"), pwFrom, pwTo)
	if read.Err != nil || read.Unparsed != nil || read.NoStream || len(read.Lines) != 0 {
		t.Fatalf("one JSON line parses, so the empty answer is real: %+v", read)
	}
	if n := len(sim.metricQueries()); n != 2 {
		t.Fatalf("the probe counts total and parsed, got %d queries", n)
	}
}

// No probe when it cannot matter: a non-empty read, a query with no parser stage, an own Loki.
func TestParseHealth_NoProbeWhenItCannotMatter(t *testing.T) {
	t.Run("lines were found", func(t *testing.T) {
		sim := &simShared{streams: arbStream(`{"level":"error"}`)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()}
		read := l.ReadLogs(levelQuery(t, shSource(), "error"), pwFrom, pwTo)
		if len(read.Lines) != 1 || len(sim.metricQueries()) != 0 {
			t.Fatalf("got %+v, %d metric queries", read, len(sim.metricQueries()))
		}
	})
	t.Run("a correlation query has no parser stage", func(t *testing.T) {
		sim := &simShared{streams: arbStream(plainInfo)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()}
		q, _ := l.CorrelationQuery(shCID, "", "")
		read := l.ReadLogs(q, pwFrom, pwTo)
		if read.Err != nil || read.Unparsed != nil || len(sim.metricQueries()) != 0 {
			t.Fatalf("got %+v, %d metric queries", read, len(sim.metricQueries()))
		}
	})
	t.Run("a source query with no derived label has no parser stage", func(t *testing.T) {
		sim := &simShared{streams: arbStream(plainInfo)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()}
		q, _ := l.SourceQuery(map[string]string{"service": "main-engine"}, "no-such-text", "")
		read := l.ReadLogs(q, pwFrom, pwTo)
		if read.Err != nil || read.Unparsed != nil || len(sim.metricQueries()) != 0 {
			t.Fatalf("got %+v, %d metric queries", read, len(sim.metricQueries()))
		}
	})
	t.Run("an own Loki is never probed", func(t *testing.T) {
		sim := &simShared{streams: []simStream{{labels: map[string]string{"service": "main-engine", "level": "info"}, lines: []string{plainInfo}}}}
		l := &Loki{BaseURL: sim.server(t).URL}
		q, _ := SourceLogQL(map[string]string{"service": "main-engine", "level": "error"}, "", "")
		read := l.ReadLogs(q, pwFrom, pwTo)
		if read.Err != nil || read.Unparsed != nil || len(sim.metricQueries()) != 0 {
			t.Fatalf("got %+v, %d metric queries", read, len(sim.metricQueries()))
		}
	})
	t.Run("no stream is still NoStream, and is not probed for parsing", func(t *testing.T) {
		sim := &simShared{}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: shSource()}
		read := l.ReadLogs(levelQuery(t, shSource(), "error"), pwFrom, pwTo)
		if !read.NoStream || read.Unparsed != nil || len(sim.metricQueries()) != 0 {
			t.Fatalf("got %+v, %d metric queries", read, len(sim.metricQueries()))
		}
	})
}

// A probe that fails is an error, never "parsed fine".
func TestParseHealth_AFailingProbeIsAnError(t *testing.T) {
	// the bare-selector probe (limit 1) finds a line, the filtered read is empty, the count fails:
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/query") {
			http.Error(w, "boom SECRET-BODY", 500)
			return
		}
		if r.URL.Query().Get("limit") == "1" {
			fmt.Fprint(w, `{"status":"success","data":{"result":[{"values":[["1","x"]]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"result":[]}}`)
	}))
	defer srv2.Close()
	l := &Loki{BaseURL: srv2.URL, Shared: shSource()}
	read := l.ReadLogs(levelQuery(t, shSource(), "error"), pwFrom, pwTo)
	if read.Err == nil || read.Unparsed != nil || strings.Contains(read.Err.Error(), "SECRET-BODY") {
		t.Fatalf("want an error without the body, got %+v", read)
	}
}

// ── (2) every metric query with a parser carries __error__ ───────────────────────────────────────────

func TestParseHealth_EveryMetricQueryWithAParserFiltersErrors(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"json", `{cluster="cluster-a",namespace="sut-ns"} |= "boom" | json level="level" | level="error"`},
		{"logfmt", `{cluster="cluster-a",namespace="sut-ns"} | logfmt level="level" | level="error"`},
		{"json + event_type", `{cluster="cluster-a"} | json saga_marker="event" | label_format event_type="x" | event_type="saga"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			total, parsed, _, ok := ParseHealthQueries(c.src, 900)
			if !ok {
				t.Fatal("a query with a parser stage must be probed")
			}
			if strings.Contains(total, "| json") || strings.Contains(total, "| logfmt") {
				t.Errorf("the total count has no parser: %s", total)
			}
			if !strings.Contains(parsed, `| __error__=""`) {
				t.Errorf("a metric query with a parser must carry `| __error__=\"\"` (real Loki rejects it otherwise): %s", parsed)
			}
			// the range is the last thing, after the pipeline
			if !strings.HasSuffix(parsed, ` | __error__="" [900s]))`) {
				t.Errorf("the range selector follows the pipeline: %s", parsed)
			}
		})
	}
	if _, _, _, ok := ParseHealthQueries(`{cluster="cluster-a"} |= "x"`, 60); ok {
		t.Error("no parser stage, nothing to probe")
	}
	// a line filter that CONTAINS the text " | json " is not a parser stage
	if _, _, _, ok := ParseHealthQueries(`{cluster="cluster-a"} |= "a | json b"`, 60); ok {
		t.Error("a quoted ' | json ' is not a stage")
	}
}

// ── (3) the plain-text format ────────────────────────────────────────────────────────────────────────

// The expression extracts what the issue asked for from the real line shape.
func TestPlainTextRegex_ExtractsLevelAndServiceFromTheRealLineShape(t *testing.T) {
	re := regexp.MustCompile(plainRegex)
	m := re.FindStringSubmatch(plainWarn)
	got := map[string]string{}
	for i, n := range re.SubexpNames() {
		if n != "" {
			got[n] = m[i]
		}
	}
	if got["level"] != "WARN" || got["service"] != "main-engine" || got["time"] != "10:06:24.852569" {
		t.Fatalf("extracted %v", got)
	}
	// and through LogQL: the generated `level=WARN` query selects the line, `| service=` selects it by the extracted group
	streams := arbStream(plainWarn, plainInfo, plainError)
	q := levelQuery(t, plainSource(), "WARN")
	if got := simQuery(t, q, streams); !reflect.DeepEqual(got, []string{plainWarn}) {
		t.Fatalf("level=WARN must select exactly the WARN line, got %v\nquery %s", got, q)
	}
	hand := `{cluster="cluster-a",namespace="sut-ns"} | regexp ` + strconv.Quote(plainRegex) + ` | service="main-engine" | level="WARN"`
	if got := simQuery(t, hand, streams); !reflect.DeepEqual(got, []string{plainWarn}) {
		t.Fatalf("extracted service=main-engine and level=WARN must select the WARN line, got %v", got)
	}
}

// (b) the LogQL generated for a level=error claim, json and plain text.
func TestPlainTextRegex_GeneratedLogQL(t *testing.T) {
	js, err := shSource().SourceLogQL(map[string]string{"service": "main-engine", "level": "error"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := plainSource().SourceLogQL(map[string]string{"service": "main-engine", "level": "ERROR"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("(a) json format:       %s", js)
	t.Logf("(b) plain-text format: %s", pt)
	if want := `{cluster="cluster-a",container="main-engine",namespace="sut-ns"} | json level="level" | level="error"`; js != want {
		t.Errorf("json:\n got %s\nwant %s", js, want)
	}
	if want := `{cluster="cluster-a",container="main-engine",namespace="sut-ns"} | regexp ` + strconv.Quote(plainRegex) + ` | level="ERROR"`; pt != want {
		t.Errorf("plain text:\n got %s\nwant %s", pt, want)
	}
}

// A level claim is judged: a plain-text SUT with its format declared selects ERROR lines, and a quiet window is a real zero.
func TestPlainTextRegex_ReadsAndJudgesLevel(t *testing.T) {
	t.Run("an ERROR line is found", func(t *testing.T) {
		sim := &simShared{streams: arbStream(plainWarn, plainError)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: plainSource()}
		read := l.ReadLogs(levelQuery(t, plainSource(), "ERROR"), pwFrom, pwTo)
		if read.Err != nil || len(read.Lines) != 1 || read.Lines[0].Line != plainError || read.Unparsed != nil {
			t.Fatalf("got %+v", read)
		}
	})
	t.Run("no ERROR line: the expression reads the others, so the zero is real", func(t *testing.T) {
		sim := &simShared{streams: arbStream(plainWarn, plainInfo)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: plainSource()}
		read := l.ReadLogs(levelQuery(t, plainSource(), "ERROR"), pwFrom, pwTo)
		if read.Err != nil || len(read.Lines) != 0 || read.Unparsed != nil || read.NoStream {
			t.Fatalf("got %+v", read)
		}
		mq := sim.metricQueries()
		if len(mq) != 2 || !strings.Contains(mq[1], ` |~ `+strconv.Quote(plainRegex)) {
			t.Fatalf("a regexp format is counted with the expression as a line filter: %q", mq)
		}
	})
	t.Run("an expression that matches none of the lines is Unparsed (format regexp)", func(t *testing.T) {
		src := shSource()
		src.Regexp = `^level=(?P<level>[a-z]+) ` // a logfmt-ish expression on these plain lines
		sim := &simShared{streams: arbStream(plainWarn, plainInfo)}
		l := &Loki{BaseURL: sim.server(t).URL, Shared: src}
		read := l.ReadLogs(levelQuery(t, src, "error"), pwFrom, pwTo)
		if read.Err != nil || read.Unparsed == nil || read.Unparsed.Format != "regexp" || read.Unparsed.Lines != 2 {
			t.Fatalf("got %+v", read)
		}
	})
}

// A claim on a label the expression has no group for is refused, never invented.
func TestPlainTextRegex_NeverInventsAField(t *testing.T) {
	src := plainSource() // groups: time, service, level
	for _, labels := range []map[string]string{
		{"service": "main-engine", "correlation_id": shCID},
		{"service": "main-engine", "event_type": "saga"},
	} {
		_, err := src.SourceLogQL(labels, "", "")
		if err == nil || !strings.Contains(err.Error(), "log_regex has no (?P<") {
			t.Errorf("%v: want a refusal naming the missing group, got %v", labels, err)
		}
	}
	src.Regexp = `(?P<level>` // not RE2
	if _, err := src.SourceLogQL(map[string]string{"service": "x", "level": "ERROR"}, "", ""); err == nil || !strings.Contains(err.Error(), "RE2") {
		t.Errorf("an invalid expression builds no query, got %v", err)
	}
	// with the groups present they are derived
	src.Regexp = plainRegex + `(?P<correlation_id>tr-\S+)? ?(?P<saga_marker>\w+)?`
	if q, err := src.SourceLogQL(map[string]string{"service": "x", "event_type": "saga"}, "", ""); err != nil || !strings.Contains(q, `| label_format event_type=`) {
		t.Errorf("event_type from a saga_marker group: %q %v", q, err)
	}
}
