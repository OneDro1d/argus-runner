package obsquery

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// a Loki answer is read up to maxLokiAnswerBytes. A larger one is an ERROR, never a short answer:
// a logs check judged on a prefix could pass on lines it never saw.

// answerOf builds a Loki answer of exactly `size` bytes holding one line, padded inside the line.
func answerOf(size int) string {
	const head, tail = `{"status":"success","data":{"result":[{"values":[["1760000000000000000","`, `"]]}]}}`
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

func lokiServing(t *testing.T, body string) *Loki {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	t.Cleanup(srv.Close)
	return &Loki{BaseURL: srv.URL}
}

func TestReadLogs_AnAnswerOverTheCapIsAnErrorNotAShortRead(t *testing.T) {
	// positive control: an answer exactly at the cap is read, whole.
	read := lokiServing(t, answerOf(maxLokiAnswerBytes)).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now())
	if read.Err != nil || len(read.Lines) != 1 || len(read.Lines[0].Line) < maxLokiAnswerBytes-200 {
		t.Fatalf("at the cap: err=%v lines=%d, want the one line read whole", read.Err, len(read.Lines))
	}
	// one byte over: refused, with the words, and no lines.
	read = lokiServing(t, answerOf(maxLokiAnswerBytes+1)).ReadLogs(`{a="b"} |= "c"`, time.Now(), time.Now())
	if !errors.Is(read.Err, ErrLokiAnswerTooLarge) {
		t.Fatalf("over the cap: err = %v, want ErrLokiAnswerTooLarge", read.Err)
	}
	if len(read.Lines) != 0 {
		t.Fatalf("over the cap: %d lines returned, want none", len(read.Lines))
	}
	for _, want := range []string{"larger than 16 MiB", "NOT used", "Narrow the query"} {
		if !strings.Contains(read.Err.Error(), want) {
			t.Errorf("the message must say %q: %v", want, read.Err)
		}
	}
}

// The best-effort failure-context reader refuses the same way (it reports "unavailable", never a prefix).
func TestQueryLines_AnAnswerOverTheCapIsUnavailable(t *testing.T) {
	l := lokiServing(t, answerOf(maxLokiAnswerBytes+1))
	if entries, ok := l.queryLines("tr-1", "5m", time.Now()); ok || entries != nil {
		t.Fatalf("queryLines over the cap = (%d entries, %v), want (none, false)", len(entries), ok)
	}
	l = lokiServing(t, answerOf(1024))
	if entries, ok := l.queryLines("tr-1", "5m", time.Now()); !ok || len(entries) != 1 {
		t.Fatalf("positive control: queryLines under the cap = (%d, %v)", len(entries), ok)
	}
}

func TestBetterStackDoQuery_AnAnswerOverTheCapIsRefused(t *testing.T) {
	serve := func(size int) *BetterStack {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// a JSON array of rows, padded to `size` bytes inside one raw field
			const head, tail = `[{"dt":"2026-10-19 10:00:00","raw":"`, `"}]`
			_, _ = fmt.Fprint(w, head+strings.Repeat("x", size-len(head)-len(tail))+tail)
		}))
		t.Cleanup(srv.Close)
		return &BetterStack{QueryURL: srv.URL}
	}
	if rows, ok := serve(1024).doQuery(bsQuery{sql: "select 1"}); !ok || len(rows) != 1 {
		t.Fatalf("positive control: (%d rows, %v)", len(rows), ok)
	}
	if rows, ok := serve(maxLokiAnswerBytes + 1).doQuery(bsQuery{sql: "select 1"}); ok || rows != nil {
		t.Fatalf("over the cap = (%d rows, %v), want (none, false)", len(rows), ok)
	}
}
