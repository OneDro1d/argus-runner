package sutlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// ── fakes: a Kubernetes API server and a Loki, both httptest ──────────────────────────────────────

type fakePod struct{ Name, Container string }

type fakeAPI struct {
	t  *testing.T
	ns string

	mu       sync.Mutex
	pods     []fakePod
	listCode int                      // 0 = 200
	logCode  int                      // 0 = 200
	script   map[string][]string      // "pod/container" -> responses, one per request; the last repeats
	hold     map[string]bool          // "pod/container" -> keep the stream open after its lines
	requests []logReq                 // every log request, in order
	auths    []string                 // Authorization of every request
	open     map[string]chan struct{} // closed when the stream's request context ends
}

type logReq struct {
	Pod, Container, Since, Follow, Timestamps, Accept string
}

func newFakeAPI(t *testing.T, ns string) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, ns: ns, script: map[string][]string{}, hold: map[string]bool{}, open: map[string]chan struct{}{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	prefix := "/api/v1/namespaces/" + f.ns + "/pods"
	switch {
	case r.URL.Path == prefix:
		f.mu.Lock()
		code := f.listCode
		pods := append([]fakePod(nil), f.pods...)
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			fmt.Fprint(w, `{"kind":"Status","message":"pods is forbidden: User system:serviceaccount:x"}`)
			return
		}
		type cs struct {
			Name    string `json:"name"`
			ImageID string `json:"imageID"`
		}
		byPod := map[string][]cs{}
		var order []string
		for _, p := range pods {
			if _, ok := byPod[p.Name]; !ok {
				order = append(order, p.Name)
			}
			byPod[p.Name] = append(byPod[p.Name], cs{Name: p.Container})
		}
		var items []map[string]any
		for _, n := range order {
			items = append(items, map[string]any{"metadata": map[string]any{"name": n},
				"status": map[string]any{"phase": "Running", "containerStatuses": byPod[n]}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case strings.HasPrefix(r.URL.Path, prefix+"/") && strings.HasSuffix(r.URL.Path, "/log"):
		pod := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"/"), "/log")
		q := r.URL.Query()
		key := pod + "/" + q.Get("container")
		f.mu.Lock()
		f.requests = append(f.requests, logReq{pod, q.Get("container"), q.Get("sinceTime"), q.Get("follow"), q.Get("timestamps"), r.Header.Get("Accept")})
		code := f.logCode
		n := 0
		for _, rq := range f.requests {
			if rq.Pod == pod && rq.Container == q.Get("container") {
				n++
			}
		}
		resp := f.script[key]
		var body string
		if len(resp) > 0 {
			body = resp[min(n-1, len(resp)-1)]
		}
		hold := f.hold[key]
		done := make(chan struct{})
		f.open[key] = done
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if hold {
			<-r.Context().Done()
		}
		close(done)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) setPods(p ...fakePod) { f.mu.Lock(); f.pods = p; f.mu.Unlock() }

func (f *fakeAPI) logRequests(pod, container string) []logReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []logReq
	for _, r := range f.requests {
		if r.Pod == pod && r.Container == container {
			out = append(out, r)
		}
	}
	return out
}

type pushed struct {
	Labels map[string]string
	TS     string
	Line   string
}

type fakeLoki struct {
	mu      sync.Mutex
	codes   []int // status per push, in order; the last repeats; empty = 200
	pushes  int
	got     []pushed
	headers []http.Header
	orderOK bool
}

func newFakeLoki(t *testing.T) (*fakeLoki, *httptest.Server) {
	l := &fakeLoki{orderOK: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.pushes++
		code := 200
		if len(l.codes) > 0 {
			code = l.codes[min(l.pushes-1, len(l.codes)-1)]
		}
		l.headers = append(l.headers, r.Header.Clone())
		if code >= 300 {
			w.WriteHeader(code)
			fmt.Fprint(w, "nope")
			return
		}
		var body struct {
			Streams []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("fake Loki: bad body: %v", err)
		}
		for _, s := range body.Streams {
			var last int64
			for _, v := range s.Values {
				n, _ := strconv.ParseInt(v[0], 10, 64)
				if n < last {
					l.orderOK = false
				}
				last = n
				l.got = append(l.got, pushed{s.Stream, v[0], v[1]})
			}
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return l, srv
}

func (l *fakeLoki) entries() []pushed {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]pushed(nil), l.got...)
}

func (l *fakeLoki) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.got) }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// harness wires API -> collector -> shipper -> Loki.
type harness struct {
	api    *fakeAPI
	loki   *fakeLoki
	stderr *lockedBuf
	cancel context.CancelFunc
	done   chan struct{}
	ship   *Shipper
	// tokPath is the ServiceAccount token file the client re-reads for every request.
	tokPath string
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func start(t *testing.T, ns string, mod func(*Options, *obsquery.Loki), token string) *harness {
	t.Helper()
	api, apiSrv := newFakeAPI(t, ns)
	loki, lokiSrv := newFakeLoki(t)
	tokPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	client := envcapture.NewFileTokenClient(apiSrv.URL, tokPath, apiSrv.Client())
	errBuf := &lockedBuf{}
	warn := &RateLog{Out: errBuf}
	ll := &obsquery.Loki{BaseURL: lokiSrv.URL}
	ship := &Shipper{Loki: ll, Warn: warn, FlushEvery: 10 * time.Millisecond, Sleep: func(ctx context.Context, d time.Duration) { time.Sleep(time.Millisecond) }}
	o := Options{
		API: client, Namespace: ns, Rules: jsonRules, Sink: ship, Warn: warn,
		ListEvery: 20 * time.Millisecond, ReopenDelay: 10 * time.Millisecond,
	}
	if mod != nil {
		mod(&o, ll)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{api: api, loki: loki, stderr: errBuf, cancel: cancel, done: make(chan struct{}), ship: ship}
	go ship.Run(ctx)
	go func() { Run(ctx, o); close(h.done) }()
	t.Cleanup(func() { cancel(); <-h.done })
	h.tokPath = tokPath
	return h
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func nanos(t *testing.T, ts string) string {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(tm.UnixNano(), 10)
}

// ── tests ─────────────────────────────────────────────────────────────────────────────────────────

// The timestamp becomes the Loki entry time and is STRIPPED; the stored line is the SUT's raw line, byte
// for byte — JSON, logfmt, odd spacing, and a trailing CR. (An EMPTY line is not shipped: see TestCollect_EmptyLineIsNotShipped.)
func TestCollect_TimestampStrippedAndUsed_RawLineByteIdentical(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	lines := []string{
		`{"level":"info","correlation_id":"tr-r-s-aaaaaaaa","msg":"héllo  \"quoted\"  "}`,
		`level=info msg="two  spaces" a=b`,
		"  leading spaces and a trailing CR\r",
		`{"event_type":"saga"}`,
	}
	var body strings.Builder
	var stamps []string
	for i, l := range lines {
		ts := fmt.Sprintf("2026-10-11T10:00:0%d.%09dZ", i, 123456789+i)
		stamps = append(stamps, ts)
		body.WriteString(ts + " " + l + "\n")
	}
	h.api.script["p1/api"] = []string{body.String()}
	h.api.hold["p1/api"] = true
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "four entries", func() bool { return h.loki.count() >= 4 })
	got := h.loki.entries()
	for i, l := range lines {
		if got[i].Line != l {
			t.Errorf("line %d = %q, want the raw %q", i, got[i].Line, l)
		}
		if got[i].TS != nanos(t, stamps[i]) {
			t.Errorf("line %d ts = %s, want %s", i, got[i].TS, nanos(t, stamps[i]))
		}
		if strings.Contains(got[i].Line, "2026-10-11T10:00") {
			t.Errorf("line %d still carries the timestamp prefix: %q", i, got[i].Line)
		}
	}
	// labels of the first (json, tr- id) and the saga one
	if got[0].Labels["correlation_id"] != "tr-r-s-aaaaaaaa" || got[0].Labels["level"] != "info" || got[0].Labels["service"] != "api" {
		t.Errorf("labels = %v", got[0].Labels)
	}
	if got[3].Labels["event_type"] != "saga" {
		t.Errorf("saga labels = %v", got[3].Labels)
	}
	// the request asked for exactly what the brief says
	rq := h.api.logRequests("p1", "api")[0]
	if rq.Follow != "true" || rq.Timestamps != "true" || rq.Accept != "" {
		t.Errorf("request = %+v", rq)
	}
}

// A line with no parsable timestamp is shipped with the receive time, unstripped, never dropped.
func TestCollect_LineWithoutATimestampIsShippedWithReceiveTime(t *testing.T) {
	fixedNow := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h := start(t, "sut", func(o *Options, _ *obsquery.Loki) { o.Now = func() time.Time { return fixedNow } }, "tok")
	h.api.script["p1/api"] = []string{"no timestamp here at all\n"}
	h.api.hold["p1/api"] = true
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "one entry", func() bool { return h.loki.count() >= 1 })
	e := h.loki.entries()[0]
	if e.Line != "no timestamp here at all" || e.TS != strconv.FormatInt(fixedNow.UnixNano(), 10) {
		t.Errorf("entry = %+v", e)
	}
}

// First sight: sinceTime = now - 5 min.
func TestCollect_FirstSightReadsFiveMinutesBack(t *testing.T) {
	fixedNow := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h := start(t, "sut", func(o *Options, _ *obsquery.Loki) { o.Now = func() time.Time { return fixedNow } }, "tok")
	h.api.hold["p1/api"] = true
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "a log request", func() bool { return len(h.api.logRequests("p1", "api")) >= 1 })
	if got, want := h.api.logRequests("p1", "api")[0].Since, "2026-10-11T11:55:00Z"; got != want {
		t.Errorf("sinceTime = %q, want %q", got, want)
	}
}

// Two pods of one container -> ONE stream (same labels), every entry accepted by an order-checking Loki.
func TestCollect_TwoPodsOfOneContainerAreOneStream(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	h.api.script["p1/api"] = []string{"2026-10-11T10:00:01.000000000Z a1\n2026-10-11T10:00:05.000000000Z a2\n"}
	h.api.script["p2/api"] = []string{"2026-10-11T10:00:02.000000000Z b1\n2026-10-11T10:00:03.000000000Z b2\n"}
	h.api.hold["p1/api"], h.api.hold["p2/api"] = true, true
	h.api.setPods(fakePod{"p1", "api"}, fakePod{"p2", "api"})
	eventually(t, "four entries", func() bool { return h.loki.count() >= 4 })
	keys := map[string]bool{}
	for _, e := range h.loki.entries() {
		b, _ := json.Marshal(e.Labels)
		keys[string(b)] = true
	}
	if len(keys) != 1 {
		t.Errorf("%d distinct label sets, want 1: %v", len(keys), keys)
	}
	h.loki.mu.Lock()
	defer h.loki.mu.Unlock()
	if !h.loki.orderOK {
		t.Error("Loki saw a stream out of order")
	}
}

// Within ONE push, a stream's entries go out in non-decreasing time even when they arrived out of order.
func TestShipper_SortsEachStreamByTime(t *testing.T) {
	loki, srv := newFakeLoki(t)
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL}}
	lbl := map[string]string{"service": "x"}
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	for _, off := range []int{5, 1, 3, 2, 4} {
		s.Add(lbl, base.Add(time.Duration(off)*time.Second), fmt.Sprint("l", off))
	}
	s.FlushOnce(context.Background(), false)
	var order []string
	for _, e := range loki.entries() {
		order = append(order, e.Line)
	}
	if strings.Join(order, ",") != "l1,l2,l3,l4,l5" || !loki.orderOK {
		t.Errorf("order = %v ok=%v", order, loki.orderOK)
	}
}

// A broken stream resumes from the last timestamp it shipped.
func TestCollect_StreamBreakResumesFromTheLastTimestamp(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	h.api.script["p1/api"] = []string{
		"2026-10-11T10:00:01.111111111Z one\n2026-10-11T10:00:02.222222222Z two\n2026-10-11T10:00:03.3333", // cut mid-line
		"2026-10-11T10:00:02.222222222Z two\n2026-10-11T10:00:03.333333333Z three\n",
	}
	h.api.hold["p1/api"] = false // the first response simply ends
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "three distinct lines", func() bool {
		seen := map[string]bool{}
		for _, e := range h.loki.entries() {
			seen[e.Line] = true
		}
		return seen["one"] && seen["two"] && seen["three"]
	})
	reqs := h.api.logRequests("p1", "api")
	if len(reqs) < 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[1].Since != "2026-10-11T10:00:02.222222222Z" {
		t.Errorf("second request sinceTime = %q, want the last shipped timestamp", reqs[1].Since)
	}
	for _, e := range h.loki.entries() {
		if strings.HasPrefix(e.Line, "2026-10-11T10:00:03.3333") {
			t.Errorf("a cut, unterminated line was shipped: %q", e.Line)
		}
	}
}

// A pod that is gone from the list has its stream closed.
func TestCollect_PodGoneClosesItsStream(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	h.api.hold["p1/api"] = true
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "stream open", func() bool { return len(h.api.logRequests("p1", "api")) >= 1 })
	h.api.mu.Lock()
	done := h.api.open["p1/api"]
	h.api.mu.Unlock()
	h.api.setPods() // the pod is gone
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of a pod that is gone was not closed")
	}
	n := len(h.api.logRequests("p1", "api"))
	time.Sleep(100 * time.Millisecond)
	if len(h.api.logRequests("p1", "api")) != n {
		t.Error("a stream was reopened for a pod that is gone")
	}
}

// Its own pod is skipped when the SUT namespace is the instance namespace.
func TestCollect_SkipsItsOwnPod(t *testing.T) {
	h := start(t, "sut", func(o *Options, _ *obsquery.Loki) { o.SelfPod, o.SelfNamespace = "sut-logs-abc", "sut" }, "tok")
	h.api.hold["sut-logs-abc/sut-logs"], h.api.hold["other/api"] = true, true
	h.api.setPods(fakePod{"sut-logs-abc", "sut-logs"}, fakePod{"other", "api"})
	eventually(t, "the other pod", func() bool { return len(h.api.logRequests("other", "api")) >= 1 })
	time.Sleep(60 * time.Millisecond)
	if n := len(h.api.logRequests("sut-logs-abc", "sut-logs")); n != 0 {
		t.Errorf("followed its own pod %d times", n)
	}
}

// 403: ONE clear stderr line per minute naming the namespace and the cure; the loop keeps trying; no token,
// credential or log line on stderr.
func TestCollect_403IsOneLinePerMinuteAndNeverExits(t *testing.T) {
	h := start(t, "locked-ns", nil, "TOKEN-SECRET-VALUE")
	h.api.mu.Lock()
	h.api.listCode = 403
	h.api.mu.Unlock()
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "the 403 line", func() bool { return h.stderr.String() != "" })
	// many more list cycles pass (ListEvery is 20 ms)
	h.api.mu.Lock()
	before := len(h.api.auths)
	h.api.mu.Unlock()
	eventually(t, "more list attempts", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return len(h.api.auths) >= before+5
	})
	out := h.stderr.String()
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Errorf("%d stderr lines, want exactly 1:\n%s", n, out)
	}
	for _, want := range []string{"locked-ns", "--sut-logs-via api", "--emit-sut-access-role", "403"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TOKEN-SECRET-VALUE") || strings.Contains(out, "forbidden: User") {
		t.Errorf("stderr leaks a token or the API's message:\n%s", out)
	}
	select {
	case <-h.done:
		t.Fatal("Run exited on a 403")
	default:
	}
	// the cure takes effect without a restart
	h.api.mu.Lock()
	h.api.script["p1/api"] = []string{"2026-10-11T10:00:01.000000000Z after the grant\n"}
	h.api.hold["p1/api"] = true
	h.api.listCode = 0
	h.api.mu.Unlock()
	eventually(t, "collection starts once granted", func() bool { return h.loki.count() >= 1 })
}

// A 403 on the log request itself (list granted, pods/log not) is the same one line.
func TestCollect_403OnTheLogRequestIsTheSameLine(t *testing.T) {
	h := start(t, "sut", nil, "tok")
	h.api.mu.Lock()
	h.api.logCode = 403
	h.api.mu.Unlock()
	h.api.setPods(fakePod{"p1", "api"}, fakePod{"p2", "api"})
	eventually(t, "the 403 line", func() bool { return h.stderr.String() != "" })
	time.Sleep(150 * time.Millisecond)
	if n := strings.Count(strings.TrimSpace(h.stderr.String()), "\n") + 1; n != 1 {
		t.Errorf("%d lines for two forbidden streams, want 1:\n%s", n, h.stderr.String())
	}
}

// The token is re-read from disk for every request: a rotated token is the one presented next.
func TestCollect_TokenIsReReadForEveryRequest(t *testing.T) {
	h := start(t, "sut", nil, "token-one")
	h.api.setPods(fakePod{"p1", "api"})
	eventually(t, "a request with the first token", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		for _, a := range h.api.auths {
			if a == "Bearer token-one" {
				return true
			}
		}
		return false
	})
	if err := os.WriteFile(h.tokPath, []byte("token-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a request with the rotated token", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return h.api.auths[len(h.api.auths)-1] == "Bearer token-two"
	})
}

// ── the pusher ────────────────────────────────────────────────────────────────────────────────────

// Loki 500 x3 then 200: nothing is lost, each entry arrives once.
func TestShipper_LokiDownThenUpLosesNothing(t *testing.T) {
	loki, srv := newFakeLoki(t)
	loki.codes = []int{500, 500, 500, 200}
	var slept int
	errBuf := &lockedBuf{}
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL}, Warn: &RateLog{Out: errBuf}, Sleep: func(context.Context, time.Duration) { slept++ }}
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		s.Add(map[string]string{"service": "x"}, base.Add(time.Duration(i)*time.Millisecond), fmt.Sprint("line-", i))
	}
	s.FlushOnce(context.Background(), true)
	if got := loki.count(); got != 50 {
		t.Fatalf("delivered %d entries, want 50", got)
	}
	seen := map[string]int{}
	for _, e := range loki.entries() {
		seen[e.Line]++
	}
	for i := 0; i < 50; i++ {
		if seen[fmt.Sprint("line-", i)] != 1 {
			t.Errorf("line-%d delivered %d times", i, seen[fmt.Sprint("line-", i)])
		}
	}
	if slept != 3 {
		t.Errorf("backed off %d times, want 3", slept)
	}
	if s.Pending() != 0 {
		t.Errorf("%d bytes still pending", s.Pending())
	}
}

// The backoff doubles and is capped at 5 s.
func TestShipper_BackoffIsCappedAtFiveSeconds(t *testing.T) {
	loki, srv := newFakeLoki(t)
	loki.codes = []int{500}
	var waits []time.Duration
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	s.Sleep = func(_ context.Context, d time.Duration) {
		waits = append(waits, d)
		if len(waits) == 9 {
			cancel()
		}
	}
	s.Add(map[string]string{"a": "b"}, time.Now(), "x")
	s.FlushOnce(ctx, true)
	if len(waits) != 9 || waits[0] != 100*time.Millisecond || waits[len(waits)-1] != 5*time.Second {
		t.Fatalf("waits = %v", waits)
	}
	for _, w := range waits {
		if w > 5*time.Second {
			t.Errorf("wait %v over the cap", w)
		}
	}
	if s.Pending() == 0 {
		t.Error("an unsent entry left the buffer")
	}
}

// Buffer full: the OLDEST entries are dropped and counted, ONE line per minute says how many.
func TestShipper_FullBufferDropsTheOldestAndCountsThem(t *testing.T) {
	loki, srv := newFakeLoki(t)
	loki.codes = []int{500}
	errBuf := &lockedBuf{}
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	rl := &RateLog{Out: errBuf, Now: func() time.Time { return now }}
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL}, Warn: rl, BufferMax: 10 * (len("line-00") + entryOverhead)}
	for i := 0; i < 25; i++ {
		s.Add(map[string]string{"service": "x"}, now.Add(time.Duration(i)*time.Second), fmt.Sprintf("line-%02d", i))
	}
	if d, _ := s.Dropped(); d != 15 {
		t.Fatalf("dropped = %d, want 15", d)
	}
	s.ReportDrops()
	s.ReportDrops() // same minute: silent
	if n := strings.Count(strings.TrimSpace(errBuf.String()), "\n") + 1; n != 1 || !strings.Contains(errBuf.String(), "dropped the 15 oldest") {
		t.Errorf("stderr:\n%s", errBuf.String())
	}
	if d, _ := s.Dropped(); d != 0 {
		t.Errorf("reported drops still counted: %d", d)
	}
	// Loki comes back: what is left is the NEWEST ten
	loki.mu.Lock()
	loki.codes = []int{200}
	loki.pushes = 0
	loki.mu.Unlock()
	s.FlushOnce(context.Background(), false)
	var lines []string
	for _, e := range loki.entries() {
		lines = append(lines, e.Line)
	}
	sort.Strings(lines)
	if len(lines) != 10 || lines[0] != "line-15" || lines[9] != "line-24" {
		t.Errorf("delivered %v", lines)
	}
	now = now.Add(61 * time.Second)
	for i := 0; i < 30; i++ {
		s.Add(map[string]string{"service": "x"}, now, fmt.Sprintf("more-%02d", i))
	}
	s.ReportDrops()
	if n := strings.Count(strings.TrimSpace(errBuf.String()), "\n") + 1; n != 2 {
		t.Errorf("a new minute should allow a second line, got %d:\n%s", n, errBuf.String())
	}
}

// Add never blocks while Loki is down.
func TestShipper_AddNeverBlocks(t *testing.T) {
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: "http://127.0.0.1:1"}, BufferMax: 1 << 12}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			s.Add(map[string]string{"a": "b"}, time.Now(), strings.Repeat("x", 100))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Add blocked")
	}
}

// Tenant header and Basic Auth are sent; PushURL is used verbatim.
func TestShipper_SendsTenantAndBasicAuthToThePushURL(t *testing.T) {
	loki, srv := newFakeLoki(t)
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: "http://unused.invalid", PushURL: srv.URL + "/loki/api/v1/push", Tenant: "inst-7", Credential: "user:pa55"}}
	s.Add(map[string]string{"service": "x"}, time.Now(), "hello")
	s.FlushOnce(context.Background(), false)
	if loki.count() != 1 {
		t.Fatalf("delivered %d", loki.count())
	}
	h := loki.headers[0]
	if h.Get("X-Scope-OrgID") != "inst-7" {
		t.Errorf("tenant header = %q", h.Get("X-Scope-OrgID"))
	}
	if u, p, ok := (&http.Request{Header: h}).BasicAuth(); !ok || u != "user" || p != "pa55" {
		t.Errorf("basic auth = %q %q %v", u, p, ok)
	}
	// and none when none is configured
	loki2, srv2 := newFakeLoki(t)
	s2 := &Shipper{Loki: &obsquery.Loki{BaseURL: srv2.URL}}
	s2.Add(map[string]string{"service": "x"}, time.Now(), "hello")
	s2.FlushOnce(context.Background(), false)
	h2 := loki2.headers[0]
	if h2.Get("X-Scope-OrgID") != "" || h2.Get("Authorization") != "" {
		t.Errorf("headers without tenant/credential: %v", h2)
	}
}

// A body Loki refuses for good (400) is retried a few times and then DROPPED, so it cannot block the lines
// behind it; the drop is counted and reported.
func TestShipper_APermanentlyRefusedBatchDoesNotBlockTheQueue(t *testing.T) {
	loki, srv := newFakeLoki(t)
	loki.codes = []int{400, 400, 400, 200}
	errBuf := &lockedBuf{}
	rl := &RateLog{Out: errBuf}
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL}, Warn: rl, Sleep: func(context.Context, time.Duration) {}}
	s.Add(map[string]string{"service": "x"}, time.Now(), "poison")
	s.FlushOnce(context.Background(), true)
	if s.Pending() != 0 {
		t.Fatalf("pending %d", s.Pending())
	}
	s.Add(map[string]string{"service": "x"}, time.Now(), "next")
	s.FlushOnce(context.Background(), true)
	if loki.count() != 1 || loki.entries()[0].Line != "next" {
		t.Errorf("delivered %v", loki.entries())
	}
	s.ReportDrops()
	if !strings.Contains(errBuf.String(), "refused 1 log entries") {
		t.Errorf("stderr:\n%s", errBuf.String())
	}
}

// Nothing of the SUT's lines and no credential is ever written to stderr, however the pushes fail.
func TestCollect_StderrNeverCarriesALogLineOrACredential(t *testing.T) {
	loki, srv := newFakeLoki(t)
	loki.codes = []int{500, 500, 400, 400, 400, 200}
	errBuf := &lockedBuf{}
	rl := &RateLog{Out: errBuf, Every: time.Nanosecond} // speak at every chance
	s := &Shipper{Loki: &obsquery.Loki{BaseURL: srv.URL, Credential: "bob:CRED-SECRET-123", Tenant: "t"}, Warn: rl, Sleep: func(context.Context, time.Duration) {}, BufferMax: 400}
	for i := 0; i < 20; i++ {
		s.Add(map[string]string{"service": "x"}, time.Now().Add(time.Duration(i)), "SUT-LINE-SECRET password=hunter2 token=abc")
	}
	s.FlushOnce(context.Background(), true)
	s.ReportDrops()
	// also a dead Loki (a transport error)
	dead := &Shipper{Loki: &obsquery.Loki{BaseURL: "http://127.0.0.1:1", Credential: "bob:CRED-SECRET-123"}, Warn: rl}
	dead.Add(map[string]string{"service": "x"}, time.Now(), "SUT-LINE-SECRET")
	dead.FlushOnce(context.Background(), false)
	out := errBuf.String()
	if out == "" {
		t.Fatal("expected some stderr output")
	}
	for _, secret := range []string{"SUT-LINE-SECRET", "hunter2", "CRED-SECRET-123", "127.0.0.1:1", srv.URL, "bob"} {
		if strings.Contains(out, secret) {
			t.Errorf("stderr contains %q:\n%s", secret, out)
		}
	}
	// every line is JSON
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]string
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("stderr line is not JSON: %q", l)
		}
	}
}

// readLine / splitTimestamp unit rules.
func TestSplitTimestamp(t *testing.T) {
	ts, line, ok := splitTimestamp("2026-10-11T10:00:00.5Z  two  spaces")
	if !ok || ts.UnixNano() != time.Date(2026, 10, 11, 10, 0, 0, 500000000, time.UTC).UnixNano() || line != " two  spaces" {
		t.Errorf("got %v %q %v", ts, line, ok)
	}
	if _, line, ok := splitTimestamp("2026-10-11T10:00:00Z"); !ok || line != "" {
		t.Errorf("empty-line case: %q %v", line, ok)
	}
	if _, line, ok := splitTimestamp("hello world"); ok || line != "hello world" {
		t.Errorf("no timestamp: %q %v", line, ok)
	}
}
