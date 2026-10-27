package obsquery

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The LogQL a logs step sends, and the one call that sends it. An httptest Loki; no real Loki.

func TestCorrelationLogQL_IsExactlyTheQueryLinesQuery(t *testing.T) {
	want := `{service=~".+",service!~"` + ObsInfraExclusion + `"} |= "tr-1"`
	if got := CorrelationLogQL("tr-1", "", ""); got != want {
		t.Fatalf("with no filter it is the correlation-id query queryLines has always sent:\n got %s\nwant %s", got, want)
	}
	if got := CorrelationLogQL("tr-1", "placed", ""); got != want+` |= "placed"` {
		t.Fatalf("correlation + contains: %s", got)
	}
	if got := CorrelationLogQL("tr-1", "placed", "level=(warn|error)"); got != want+` |= "placed" |~ "level=(warn|error)"` {
		t.Fatalf("correlation + contains + matching: %s", got)
	}
}

func TestSourceLogQL_LabelsAreSortedAndQuoted(t *testing.T) {
	got, err := SourceLogQL(map[string]string{"namespace": "prod", "app": "orders"}, "", `ERROR .*`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{app="orders",namespace="prod"} |~ "ERROR .*"`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	for i := 0; i < 20; i++ { // a map's order is random: the query must not be
		again, _ := SourceLogQL(map[string]string{"namespace": "prod", "app": "orders"}, "", `ERROR .*`)
		if again != got {
			t.Fatalf("the query is not deterministic: %s vs %s", again, got)
		}
	}
}

func TestSourceLogQL_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"no labels", nil},
		{"a name outside the grammar", map[string]string{`a="x",b`: "y"}},
		{"a regex-matcher spelling", map[string]string{`app=~".*"`: "y"}},
		{"an empty value", map[string]string{"app": ""}},
	}
	for _, c := range cases {
		if q, err := SourceLogQL(c.labels, "x", ""); err == nil {
			t.Errorf("%s: want an error, got the query %s", c.name, q)
		}
	}
}

// A value holding a quote, a backslash, a backtick and a newline stays inside its literal: the decoded query is ONE
// selector plus filters, and the decoded literal is the value.
func TestLogQL_HostileValuesStayInsideTheirLiterals(t *testing.T) {
	hostile := "a\"b\\c`d\ne}{x=\"y\""
	q, err := SourceLogQL(map[string]string{"app": hostile}, hostile, hostile)
	if err != nil {
		t.Fatal(err)
	}
	want := `{app=` + logqlString(hostile) + `} |= ` + logqlString(hostile) + ` |~ ` + logqlString(hostile)
	if q != want {
		t.Fatalf("got %s want %s", q, want)
	}
	if strings.Contains(q, "\n") {
		t.Fatalf("a raw newline reached the query: %q", q)
	}
	// walk the query: outside literals there are exactly one selector and the two filter operators
	var outside strings.Builder
	in, esc := false, false
	for _, r := range q {
		switch {
		case in && esc:
			esc = false
		case in && r == '\\':
			esc = true
		case r == '"':
			in = !in
			outside.WriteRune('"')
		case !in:
			outside.WriteRune(r)
		}
	}
	if got, wantOutside := outside.String(), `{app=""} |= "" |~ ""`; got != wantOutside {
		t.Fatalf("outside its literals the query must be one selector and two filters, got %q", got)
	}
	c := CorrelationLogQL("tr-1", hostile, hostile)
	if !strings.HasSuffix(c, ` |= `+logqlString(hostile)+` |~ `+logqlString(hostile)) {
		t.Fatalf("correlation filters: %s", c)
	}
}

func lokiAnswer(values ...[2]string) string {
	var vs []string
	for _, v := range values {
		vs = append(vs, fmt.Sprintf("[%q,%q]", v[0], v[1]))
	}
	return `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[` + strings.Join(vs, ",") + `]}]}}`
}

func TestReadLogs_SendsOneQueryRangeWithTheLimitAndSortsByTime(t *testing.T) {
	var gotPath, gotQuery, gotLimit, gotDir, gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotPath, gotQuery, gotLimit, gotDir, gotStart, gotEnd = r.URL.Path, q.Get("query"), q.Get("limit"), q.Get("direction"), q.Get("start"), q.Get("end")
		// two streams: Loki groups by stream, so the second holds an EARLIER line
		fmt.Fprint(w, `{"status":"success","data":{"result":[`+
			`{"values":[["300","c"],["400","d"]]},{"values":[["100","a"],["200","b"]]}]}}`)
	}))
	defer srv.Close()
	from, to := time.Unix(10, 0), time.Unix(20, 0)
	read := (&Loki{BaseURL: srv.URL}).ReadLogs(`{app="x"} |= "y"`, from, to)
	if read.Err != nil {
		t.Fatal(read.Err)
	}
	if gotPath != "/loki/api/v1/query_range" || gotQuery != `{app="x"} |= "y"` || gotLimit != "2000" || gotDir != "forward" {
		t.Fatalf("path %q query %q limit %q direction %q", gotPath, gotQuery, gotLimit, gotDir)
	}
	if gotStart != fmt.Sprint(from.UnixNano()) || gotEnd != fmt.Sprint(to.UnixNano()) {
		t.Fatalf("range %s..%s", gotStart, gotEnd)
	}
	if read.Limit != LogsLineLimit || LogsLineLimit != 2000 {
		t.Fatalf("limit %d", read.Limit)
	}
	var order []string
	for _, l := range read.Lines {
		order = append(order, l.Line)
	}
	if strings.Join(order, "") != "abcd" {
		t.Fatalf("lines must be in time order across streams, got %v", order)
	}
}

func TestReadLogs_TenantAndCredentialAreSentWhenConfigured(t *testing.T) {
	var tenant, user, pass string
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = r.Header.Get(TenantHeader)
		user, pass, hasAuth = r.BasicAuth()
		fmt.Fprint(w, lokiAnswer())
	}))
	defer srv.Close()
	if read := (&Loki{BaseURL: srv.URL, Tenant: "inst-a", Credential: "u:p"}).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now()); read.Err != nil {
		t.Fatal(read.Err)
	}
	if tenant != "inst-a" || !hasAuth || user != "u" || pass != "p" {
		t.Fatalf("tenant %q auth %v %q:%q", tenant, hasAuth, user, pass)
	}
	hasAuth, tenant = true, "x"
	if read := (&Loki{BaseURL: srv.URL}).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now()); read.Err != nil {
		t.Fatal(read.Err)
	}
	if hasAuth || tenant != "" {
		t.Fatalf("with neither configured no Authorization and no %s header may be sent: auth %v tenant %q", TenantHeader, hasAuth, tenant)
	}
}

// FAIL CLOSED: every way of not getting an answer is an error, never an empty line list.
func TestReadLogs_ErrorsAreNeverZeroLines(t *testing.T) {
	answers := map[string]func(w http.ResponseWriter, r *http.Request){
		"HTTP 500": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom with the free text SECRET-BODY", 500)
		},
		"HTTP 401":              func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) },
		"garbage":               func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>not json") },
		"an empty object":       func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) },
		"null":                  func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `null`) },
		"status error with 200": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"error","data":{"result":[]}}`) },
	}
	for name, h := range answers {
		srv := httptest.NewServer(http.HandlerFunc(h))
		read := (&Loki{BaseURL: srv.URL}).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now())
		srv.Close()
		if read.Err == nil {
			t.Errorf("%s: want an error, got %d lines", name, len(read.Lines))
		}
		if read.Err != nil && (strings.Contains(read.Err.Error(), "SECRET-BODY") || strings.Contains(read.Err.Error(), "query_range")) {
			t.Errorf("%s: the error carries the server's text or the URL: %v", name, read.Err)
		}
	}
	// unreachable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if read := (&Loki{BaseURL: url}).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now()); read.Err == nil || strings.Contains(read.Err.Error(), "query_range") {
		t.Errorf("down: want an error without the URL, got %v", read.Err)
	}
	// no URL at all
	if read := (&Loki{}).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now()); read.Err == nil {
		t.Error("no Loki URL must be an error")
	}
}
