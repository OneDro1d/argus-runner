package obsquery_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/sutlogs"
)

// The shared-Loki log source: the selector, the query-time parse, and parity with the
// ingest-time pipeline (internal/sutlogs/labels.go) that an instance's own promtail applied.

const (
	shCluster = "cluster-a"
	shNS      = "sut-ns"
	shCID     = "tr-20261019T090000000-SAGA-1-abcdef12"
)

func shSource() *SharedLogSource {
	return &SharedLogSource{
		Cluster: shCluster, Namespace: shNS,
		Constants:        map[string]string{"argus_instance": "inst-1", "project": "orders"},
		Format:           "json",
		LevelField:       "level",
		CorrelationField: "correlation_id",
		SagaField:        "event",
		SagaMarkers:      []string{"tool_dispatch", "ghost_dispatch"},
	}
}

// (a) the query carries the cluster AND the namespace selector. Exact strings: a query missing either differs.
func TestSharedLogQL_CorrelationQueryNamesClusterAndNamespace(t *testing.T) {
	excl := `container!~"` + ObsInfraExclusion + `"`
	got, err := shSource().CorrelationLogQL(shCID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := `{cluster="cluster-a",namespace="sut-ns",` + excl + `} |= "` + shCID + `"`
	if got != want {
		t.Fatalf("correlation query:\n got %s\nwant %s", got, want)
	}
	for _, need := range []string{`cluster="cluster-a"`, `namespace="sut-ns"`} {
		if !strings.Contains(got, need) {
			t.Errorf("the query must carry %s: %s", need, got)
		}
	}
	got, _ = shSource().CorrelationLogQL(shCID, "order placed", `level=(warn|error)`)
	if want := `{cluster="cluster-a",namespace="sut-ns",` + excl + `} |= "` + shCID + `" |= "order placed" |~ "level=(warn|error)"`; got != want {
		t.Fatalf("with filters:\n got %s\nwant %s", got, want)
	}
	// the saga-presence template selects with exactly this selector
	sel, _ := shSource().Selector()
	if want := `{cluster="cluster-a",namespace="sut-ns",` + excl + `}`; sel != want {
		t.Fatalf("selector %s, want %s", sel, want)
	}
}

func TestSharedLogQL_SourceQueryGolden(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"service is the container", map[string]string{"service": "orders"}, `{cluster="cluster-a",container="orders",namespace="sut-ns"} |= "x"`},
		{"stream labels pass through, sorted", map[string]string{"app": "orders", "pod": "orders-0"}, `{app="orders",cluster="cluster-a",namespace="sut-ns",pod="orders-0"} |= "x"`},
		{"level is parsed at query time", map[string]string{"service": "orders", "level": "error"},
			`{cluster="cluster-a",container="orders",namespace="sut-ns"} |= "x" | json level="level" | level="error"`},
		{"correlation_id is parsed at query time", map[string]string{"correlation_id": shCID},
			`{cluster="cluster-a",namespace="sut-ns"} |= "x" | json correlation_id="correlation_id" | correlation_id="` + shCID + `"`},
		{"event_type=saga carries the per-SUT marker normalisation", map[string]string{"event_type": "saga"},
			`{cluster="cluster-a",namespace="sut-ns"} |= "x" | json saga_marker="event" | label_format event_type=` +
				strconv.Quote(`{{ if or (eq .saga_marker "tool_dispatch") (eq .saga_marker "ghost_dispatch") }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}`) +
				` | event_type="saga"`},
		{"all three together", map[string]string{"level": "warn", "correlation_id": shCID, "event_type": "saga"},
			`{cluster="cluster-a",namespace="sut-ns"} |= "x" | json level="level", correlation_id="correlation_id", saga_marker="event" | label_format event_type=` +
				strconv.Quote(`{{ if or (eq .saga_marker "tool_dispatch") (eq .saga_marker "ghost_dispatch") }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}`) +
				` | level="warn" | correlation_id="` + shCID + `" | event_type="saga"`},
		{"this instance's own project is every line, so it drops out", map[string]string{"project": "orders", "app": "o"}, `{app="o",cluster="cluster-a",namespace="sut-ns"} |= "x"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := shSource().SourceLogQL(c.labels, "x", "")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestSharedLogQL_LogfmtSutUsesTheLogfmtParser(t *testing.T) {
	s := shSource()
	s.Format = "logfmt"
	got, err := s.SourceLogQL(map[string]string{"level": "error"}, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{cluster="cluster-a",namespace="sut-ns"} |= "x" | logfmt level="level" | level="error"`; got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

// What a shared Loki cannot honour is refused by name; nothing is turned into a selector that matches nothing.
func TestSharedLogQL_SourceQueryRefusals(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"cluster is fixed", map[string]string{"cluster": "cluster-b"}, "fixed by the instance"},
		{"namespace is fixed", map[string]string{"namespace": "other"}, "fixed by the instance"},
		{"another project selects nothing", map[string]string{"project": "someone-else"}, "does not exist in a shared Loki"},
		{"another instance selects nothing", map[string]string{"argus_instance": "someone-else"}, "does not exist in a shared Loki"},
		{"a non-Argus correlation_id was never a label", map[string]string{"correlation_id": "req-1"}, "only an Argus id"},
		{"service and container together", map[string]string{"service": "a", "container": "b"}, "both name the container"},
		{"empty value", map[string]string{"app": ""}, "empty value"},
		{"bad label name", map[string]string{"a-b": "x"}, "not a Loki label name"},
		{"no label", nil, "at least one label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := shSource().SourceLogQL(c.labels, "x", "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got q=%q err=%v", c.want, q, err)
			}
			if q != "" {
				t.Fatalf("a refused step must build no query, got %q", q)
			}
		})
	}
}

// ⛔ No fallback: a shared source without a cluster or a namespace builds NO query at all.
func TestSharedLogQL_NoClusterOrNamespaceBuildsNoQuery(t *testing.T) {
	for _, mod := range []func(*SharedLogSource){
		func(s *SharedLogSource) { s.Cluster = "" },
		func(s *SharedLogSource) { s.Namespace = "" },
		func(s *SharedLogSource) { s.Namespace = `a"b` },
	} {
		s := shSource()
		mod(s)
		if q, err := s.CorrelationLogQL(shCID, "", ""); err == nil || q != "" {
			t.Errorf("correlation: want a refusal, got %q / %v", q, err)
		}
		if q, err := s.SourceLogQL(map[string]string{"app": "a"}, "x", ""); err == nil || q != "" {
			t.Errorf("source: want a refusal, got %q / %v", q, err)
		}
		if q, err := s.Selector(); err == nil || q != "" {
			t.Errorf("selector: want a refusal, got %q / %v", q, err)
		}
	}
	var none *SharedLogSource
	if _, err := none.Selector(); err == nil {
		t.Error("a nil source must refuse, not panic")
	}
}

// (3) an instance with its OWN Loki gets exactly the queries it always got - literal golden strings.
func TestOwnLoki_QueriesAreUnchanged(t *testing.T) {
	l := &Loki{BaseURL: "http://x"}
	got, err := l.CorrelationQuery(shCID, "order placed", `l=(w|e)`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{service=~".+",service!~"loki|promtail|grafana|prometheus|pushgateway|jmeter"} |= "` + shCID + `" |= "order placed" |~ "l=(w|e)"`; got != want {
		t.Fatalf("own-Loki correlation query changed:\n got %s\nwant %s", got, want)
	}
	got, err = l.SourceQuery(map[string]string{"service": "orders", "level": "error", "event_type": "saga"}, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{event_type="saga",level="error",service="orders"} |= "x"`; got != want {
		t.Fatalf("own-Loki source query changed:\n got %s\nwant %s", got, want)
	}
	// and the helpers the rest of the code calls are the same strings
	if got != mustSource(t, map[string]string{"service": "orders", "level": "error", "event_type": "saga"}, "x") {
		t.Fatal("SourceQuery on an own Loki must be SourceLogQL")
	}
	if l.Shared != nil {
		t.Fatal("a Loki is not shared unless it is given a source")
	}
}

func mustSource(t *testing.T, labels map[string]string, contains string) string {
	t.Helper()
	q, err := SourceLogQL(labels, contains, "")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// A shared Loki's query builder is reached through the same Loki type.
func TestSharedLoki_BuildsThroughTheLokiType(t *testing.T) {
	l := &Loki{BaseURL: "http://x", Shared: shSource()}
	q, err := l.CorrelationQuery(shCID, "", "")
	if err != nil || !strings.HasPrefix(q, `{cluster="cluster-a",namespace="sut-ns",`) {
		t.Fatalf("got %q / %v", q, err)
	}
	var _ LogsQueryBuilder = l
}

// ── (b) a saga line and a non-saga line through the query-time mapping ──────────────────────────────

const shContainer = "orders"

// shStreams is a shared Loki holding the SUT's pods, a same-named namespace on ANOTHER cluster, another
// namespace of this cluster, and the observability plane's own container in the SUT namespace.
func shStreams(lines []string) []simStream {
	return []simStream{
		{labels: map[string]string{"cluster": "cluster-a", "namespace": "sut-ns", "container": shContainer, "pod": "orders-0"}, lines: lines},
		{labels: map[string]string{"cluster": "cluster-b", "namespace": "sut-ns", "container": shContainer, "pod": "orders-9"}, lines: lines},
		{labels: map[string]string{"cluster": "cluster-a", "namespace": "other-ns", "container": shContainer, "pod": "x-0"}, lines: lines},
		{labels: map[string]string{"cluster": "cluster-a", "namespace": "sut-ns", "container": "loki", "pod": "loki-0"}, lines: lines},
	}
}

func TestSharedLogQL_ASagaLineAndANonSagaLine(t *testing.T) {
	saga := `{"level":"info","correlation_id":"` + shCID + `","event":"tool_dispatch","step_name":"control_action"}`
	other := `{"level":"info","correlation_id":"` + shCID + `","event":"http_request"}`
	streams := shStreams([]string{saga, other})

	q, err := shSource().SourceLogQL(map[string]string{"service": shContainer, "event_type": "saga"}, shCID, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("shared-mode source query (logs step, labels service+event_type=saga):\n%s", q)
	// ONE line, from ONE stream: the saga line of THIS cluster's namespace. Not the same-named namespace of
	// another cluster, not another namespace, not the plane's container.
	if got := simQuery(t, q, streams); !reflect.DeepEqual(got, []string{saga}) {
		t.Fatalf("event_type=saga must select exactly the saga line once, got %v", got)
	}

	// the same line under the non-saga marker is NOT a saga, and the opposite filter finds it
	q, _ = shSource().SourceLogQL(map[string]string{"service": shContainer, "event_type": "http_request"}, shCID, "")
	if got := simQuery(t, q, streams); !reflect.DeepEqual(got, []string{other}) {
		t.Fatalf("event_type=http_request must select the non-saga line, got %v", got)
	}

	// the correlation scope reads both lines of this run, once each
	cq, _ := shSource().CorrelationLogQL(shCID, "", "")
	t.Logf("shared-mode correlation query (logs step, scope correlation):\n%s", cq)
	if got := simQuery(t, cq, streams); !reflect.DeepEqual(got, []string{saga, other}) {
		t.Fatalf("the correlation scope must read this cluster+namespace once, got %v", got)
	}
}

// The strongest form of "the same line gives the same result as the ingest-time pipeline": for every line of a
// corpus, and for every value of level / correlation_id / event_type either pipeline can produce, the query-time
// query selects the line exactly when sutlogs.Labels (the promtail pipeline in Go) gives it that label value.
func TestSharedLogQL_QueryTimeMappingEqualsTheIngestTimePipeline(t *testing.T) {
	type corpus struct {
		name   string
		src    *SharedLogSource
		rules  sutlogs.Rules
		lines  []string
		values map[string][]string // extra candidate values per label
	}
	jsonRules := sutlogs.Rules{Instance: "inst-1", Project: "orders", Cluster: "cluster-a", Format: "json",
		LevelField: "level", CorrelationField: "correlation_id", SagaEventField: "event", SagaMarkers: []string{"tool_dispatch", "ghost_dispatch"}}
	nested := jsonRules
	nested.LevelField = "log.level"
	nested.CorrelationField = "ctx.request_id"
	logfmtRules := sutlogs.Rules{Instance: "inst-1", Project: "orders", Cluster: "cluster-a", Format: "logfmt",
		LevelField: "level", CorrelationField: "correlation_id", SagaEventField: "event_type", SagaMarkers: []string{"saga"}}

	mk := func(r sutlogs.Rules) *SharedLogSource {
		s := shSource()
		s.Format, s.LevelField, s.CorrelationField, s.SagaField, s.SagaMarkers = r.Format, r.LevelField, r.CorrelationField, r.SagaEventField, r.SagaMarkers
		return s
	}
	corpora := []corpus{
		{"json, two declared markers", mk(jsonRules), jsonRules, []string{
			`{"level":"info","correlation_id":"` + shCID + `","event":"tool_dispatch"}`,
			`{"level":"error","correlation_id":"` + shCID + `","event":"ghost_dispatch"}`,
			`{"level":"error","correlation_id":"` + shCID + `","event":"http_request"}`,
			`{"level":"warn","correlation_id":"req-42","event":"saga"}`, // an undeclared marker that IS the word saga passes through as saga
			`{"level":"debug","correlation_id":"","event":""}`,
			`{"message":"no fields at all"}`,
			`plain prose, not JSON, mentioning ` + shCID,
			`{"level":"info","correlation_id":"tr-other-run","event":"Tool_Dispatch"}`, // case differs: not a marker
		}, map[string][]string{"event_type": {"saga", "http_request", "Tool_Dispatch", "tool_dispatch"}}},
		{"json, nested field paths", mk(nested), nested, []string{
			`{"log":{"level":"error"},"ctx":{"request_id":"` + shCID + `"},"event":"tool_dispatch"}`,
			`{"log":{"level":"info"},"ctx":{"request_id":"abc"},"event":"x"}`,
			`{"level":"error"}`,
		}, nil},
		{"logfmt", mk(logfmtRules), logfmtRules, []string{
			`level=error correlation_id=` + shCID + ` event_type=saga msg=hello`,
			`level=info correlation_id=` + shCID + ` event_type=request`,
			`level=info correlation_id=not-an-argus-id event_type=saga`,
			`just some text`,
		}, nil},
	}
	for _, c := range corpora {
		t.Run(c.name, func(t *testing.T) {
			selected := func(label, value string, line string) bool {
				q, err := c.src.SourceLogQL(map[string]string{"service": shContainer, label: value}, "", "")
				if err != nil {
					t.Fatalf("%s=%q: %v", label, value, err)
				}
				lines := simQuery(t, q, []simStream{{labels: map[string]string{"cluster": "cluster-a", "namespace": "sut-ns", "container": shContainer}, lines: []string{line}}})
				return len(lines) == 1
			}
			for _, line := range c.lines {
				ingest := sutlogs.Labels(c.rules, shContainer, line)
				for _, label := range []string{"level", "correlation_id", "event_type"} {
					cands := map[string]bool{}
					for _, l2 := range c.lines {
						if v := sutlogs.Labels(c.rules, shContainer, l2)[label]; v != "" {
							cands[v] = true
						}
					}
					for _, v := range c.values[label] {
						cands[v] = true
					}
					for v := range cands {
						if label == "correlation_id" && !strings.HasPrefix(v, "tr-") {
							continue // never a label at ingest; the builder refuses it (TestSharedLogQL_SourceQueryRefusals)
						}
						want := ingest[label] == v
						if got := selected(label, v, line); got != want {
							t.Errorf("line %q, %s=%q: ingest-time label %q says %v, the query-time query says %v", line, label, v, ingest[label], want, got)
						}
					}
				}
			}
		})
	}
}

// ── (c) the read: no stream is not zero lines ───────────────────────────────────────────────────────

type probeLoki struct {
	mu      sync.Mutex
	queries []string
	limits  []string
	answer  func(query string) []string // lines for a query
}

func (p *probeLoki) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query().Get("query")
		p.queries = append(p.queries, q)
		p.limits = append(p.limits, r.URL.Query().Get("limit"))
		var vals []string
		for i, l := range p.answer(q) {
			vals = append(vals, fmt.Sprintf(`["%d",%s]`, 1000+i, strconv.Quote(l)))
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[`+strings.Join(vals, ",")+`]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSharedReadLogs_NoStreamIsNotZeroLines(t *testing.T) {
	var from, to = time.Unix(1000, 0), time.Unix(2000, 0)

	t.Run("the selector sees nothing: NoStream, after one probe of the bare selector", func(t *testing.T) {
		p := &probeLoki{answer: func(string) []string { return nil }}
		l := &Loki{BaseURL: p.server(t).URL, Shared: shSource()}
		q, _ := l.CorrelationQuery(shCID, "boom", "")
		read := l.ReadLogs(q, from, to)
		if read.Err != nil || !read.NoStream || len(read.Lines) != 0 {
			t.Fatalf("want NoStream with no error, got %+v", read)
		}
		sel, _ := shSource().Selector()
		if len(p.queries) != 2 || p.queries[0] != q || p.queries[1] != sel || p.limits[1] != "1" {
			t.Fatalf("want the query, then the bare selector with limit 1; got %q limits %v", p.queries, p.limits)
		}
	})

	t.Run("lines exist but none passes the filter: genuinely zero, NoStream false", func(t *testing.T) {
		p := &probeLoki{answer: func(q string) []string {
			if strings.Contains(q, "|=") {
				return nil
			}
			return []string{"some other line"}
		}}
		l := &Loki{BaseURL: p.server(t).URL, Shared: shSource()}
		q, _ := l.CorrelationQuery(shCID, "boom", "")
		read := l.ReadLogs(q, from, to)
		if read.Err != nil || read.NoStream || len(read.Lines) != 0 {
			t.Fatalf("want a plain zero, got %+v", read)
		}
	})

	t.Run("lines found: no probe at all", func(t *testing.T) {
		p := &probeLoki{answer: func(string) []string { return []string{"a"} }}
		l := &Loki{BaseURL: p.server(t).URL, Shared: shSource()}
		q, _ := l.CorrelationQuery(shCID, "", "")
		if read := l.ReadLogs(q, from, to); read.NoStream || len(read.Lines) != 1 || len(p.queries) != 1 {
			t.Fatalf("got %+v after %d queries", read, len(p.queries))
		}
	})

	t.Run("a probe that fails is an error, not a zero", func(t *testing.T) {
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				fmt.Fprint(w, `{"status":"success","data":{"result":[]}}`)
				return
			}
			http.Error(w, "boom", 500)
		}))
		defer srv.Close()
		l := &Loki{BaseURL: srv.URL, Shared: shSource()}
		q, _ := l.CorrelationQuery(shCID, "", "")
		if read := l.ReadLogs(q, from, to); read.Err == nil || read.NoStream {
			t.Fatalf("want an error, got %+v", read)
		}
	})

	t.Run("an own Loki is never probed", func(t *testing.T) {
		p := &probeLoki{answer: func(string) []string { return nil }}
		l := &Loki{BaseURL: p.server(t).URL}
		read := l.ReadLogs(CorrelationLogQL(shCID, "", ""), from, to)
		if read.Err != nil || read.NoStream || len(p.queries) != 1 {
			t.Fatalf("got %+v after %d queries", read, len(p.queries))
		}
	})
}
