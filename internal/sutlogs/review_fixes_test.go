package sutlogs

// review_fixes_test.go — the independent review of: an empty line is not shipped, and the
// pod LIST cannot hang forever.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// FIX 3: promtail drops an EMPTY line, so does the collector. A whitespace-only line is the SUT's and is
// shipped unchanged (not trimmed). The empty line still advances the resume point of its stream: the
// reopened stream asks from the timestamp of the LAST line, which here is an empty one.
func TestCollect_EmptyLineIsNotShipped_WhitespaceOnlyIsShippedUnchanged(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	const (
		t1 = "2026-10-11T10:00:01.000000001Z"
		t2 = "2026-10-11T10:00:02.000000002Z"
		t3 = "2026-10-11T10:00:03.000000003Z"
		t4 = "2026-10-11T10:00:04.000000004Z"
	)
	h.api.script["p1/api"] = []string{
		t1 + " first\n" + // shipped
			t2 + " \n" + // empty after the prefix: dropped
			t3 + "   \n" + // two spaces: the SUT's line, shipped unchanged
			t4 + " \n", // empty, and the last line the stream shipped or skipped
		"",
	}
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "a reopened stream", func() bool { return len(h.api.logRequests("p1", "api")) >= 2 })
	eventually(t, "two entries", func() bool { return h.loki.count() >= 2 })
	time.Sleep(100 * time.Millisecond) // a third (empty) entry would have arrived by now
	got := h.loki.entries()
	if len(got) != 2 {
		t.Fatalf("%d entries shipped, want 2 (first and the two-space line): %+v", len(got), got)
	}
	if got[0].Line != "first" || got[1].Line != "  " {
		t.Errorf("lines = %q, %q; want %q and %q (whitespace is not trimmed)", got[0].Line, got[1].Line, "first", "  ")
	}
	if want := t4; h.api.logRequests("p1", "api")[1].Since != want {
		t.Errorf("resumed from %q, want the empty line's timestamp %q", h.api.logRequests("p1", "api")[1].Since, want)
	}
}

// FIX 4: a list response whose body stalls after the headers returns an error within the list deadline.
func stallingListServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // headers are out; the body never comes
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func tokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListRunning_ABodyThatStallsAfterItsHeadersErrorsWithinTheDeadline(t *testing.T) {
	var hits atomic.Int32
	srv := stallingListServer(t, &hits)
	// a stream client: no overall timeout of its own (the default http.Client has none)
	c := envcapture.NewFileTokenClient(srv.URL, tokenFile(t), &http.Client{}).WithListTimeout(150 * time.Millisecond)
	start := time.Now()
	done := make(chan error, 1)
	go func() { _, err := c.ListRunning(context.Background(), "sut"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled list body returned no error")
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("took %v, want about the 150ms deadline", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListRunning is still waiting on a stalled body: it has no deadline of its own")
	}
}

// ... and the NEXT poll still happens: the loop is not stuck behind the stalled list.
func TestRun_AStalledListDoesNotStopTheNextPoll(t *testing.T) {
	var hits atomic.Int32
	srv := stallingListServer(t, &hits)
	c := envcapture.NewFileTokenClient(srv.URL, tokenFile(t), &http.Client{}).WithListTimeout(50 * time.Millisecond)
	errBuf := &lockedBuf{}
	warn := &RateLog{Out: errBuf}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Options{API: c, Namespace: "sut", Sink: &Shipper{Warn: warn}, Warn: warn, ListEvery: 20 * time.Millisecond})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "a second list request after the first one stalled", func() bool { return hits.Load() >= 2 })
}
