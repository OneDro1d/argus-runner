package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// AC-D69 (design B2): the per-session tool hooks the local router uses. The executor and the control plane leave them
// nil — hooks_unset_test.go pins that nothing changes for them.

// subjectAuth maps a token to a principal: "<hat>:<subject>" — so one test can present two callers on one session id.
type subjectAuth struct{}

func (subjectAuth) Authenticate(tok string) (Principal, error) {
	hat, subject, ok := strings.Cut(tok, ":")
	if !ok {
		return Principal{}, fmt.Errorf("bad token")
	}
	return Principal{Hat: role.Role(hat), Subject: subject}, nil
}

func okHandler(payload any) Handler {
	return func(json.RawMessage, Principal) Outcome { return Ok(payload) }
}

// hookServer builds a server with NO static tools and a SessionTools hook that builds per subject. builds counts calls.
type hookServer struct {
	*Server
	builds  atomic.Int64
	perSubj sync.Map // subject -> *atomic.Int64
}

func (h *hookServer) buildsFor(subject string) int64 {
	v, _ := h.perSubj.LoadOrStore(subject, &atomic.Int64{})
	return v.(*atomic.Int64).Load()
}

func newHookServer(t *testing.T, build func(sessionID string, prin Principal) SessionSet) *hookServer {
	t.Helper()
	s, err := NewServerWithAuth(subjectAuth{})
	if err != nil {
		t.Fatal(err)
	}
	h := &hookServer{Server: s}
	s.SessionTools = func(sid string, prin Principal) SessionSet {
		h.builds.Add(1)
		v, _ := h.perSubj.LoadOrStore(prin.Subject, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
		return build(sid, prin)
	}
	return h
}

func twoPlaneSet() *ToolSet {
	ts, skipped := NewToolSet(
		Tool{Name: "runner__ping", Namespace: NSRunner, Description: "r", InputSchema: sch(), Handler: okHandler("pong")},
		Tool{Name: "author_write", Namespace: NSAuthor, Description: "a", InputSchema: sch(), Aliases: []string{"author__write"},
			Handler: okHandler("written")},
		Tool{Name: "author_hidden", Namespace: NSAuthor, Hidden: true, InputSchema: sch(), Handler: okHandler("hidden")},
	)
	if len(skipped) != 0 {
		panic(fmt.Sprint("fixture skipped ", skipped))
	}
	return ts
}

func listNames(t *testing.T, s *Server, sid, tok string) []string {
	t.Helper()
	raw, _ := s.Dispatch(sid, tok, reqBytes(9, "tools/list", nil))
	var r struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Error != nil {
		t.Fatalf("tools/list: %s", raw)
	}
	var out []string
	for _, tl := range r.Result.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func initialize(t *testing.T, s *Server, sid, tok string) map[string]any {
	t.Helper()
	raw, _ := s.Dispatch(sid, tok, reqBytes(1, "initialize", map[string]any{}))
	var r struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Result == nil {
		t.Fatalf("initialize: %s", raw)
	}
	s.Dispatch(sid, tok, notifBytes("notifications/initialized", nil))
	return r.Result
}

func TestSessionTools_InitializeBuildsAFreshSetAndCarriesTheInstructions(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet {
		return SessionSet{Tools: twoPlaneSet(), Instructions: "the author tools are not listed: because"}
	})
	res := initialize(t, h.Server, "s1", "test:a")
	if res["instructions"] != "the author tools are not listed: because" {
		t.Fatalf("initialize did not carry the hook's text: %v", res)
	}
	if h.builds.Load() != 1 {
		t.Fatalf("initialize built %d sets, want 1", h.builds.Load())
	}
	listNames(t, h.Server, "s1", "test:a")
	if h.builds.Load() != 1 {
		t.Fatalf("tools/list rebuilt a set initialize had just stored (%d builds)", h.builds.Load())
	}
	// A second initialize on the same session is a new session start: built anew (decision 3).
	initialize(t, h.Server, "s1", "test:a")
	if h.builds.Load() != 2 {
		t.Fatalf("a second initialize reused the old set (%d builds) — a tool added upstream would never appear", h.builds.Load())
	}
}

func TestSessionTools_NoInstructionsKeyWhenTheTextIsEmpty(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	if _, ok := initialize(t, h.Server, "s", "test:a")["instructions"]; ok {
		t.Fatal(`an empty text produced an "instructions" key`)
	}
}

func TestSessionTools_ListUsesTheSetThroughTheSameHatFilter(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	initialize(t, h.Server, "t", "test:a")
	initialize(t, h.Server, "p", "product:b")
	if got := strings.Join(listNames(t, h.Server, "t", "test:a"), ","); got != "runner__ping,author_write" {
		t.Fatalf("test hat lists %q — want the set's listed tools, no alias, no hidden tool", got)
	}
	if got := strings.Join(listNames(t, h.Server, "p", "product:b"), ","); got != "runner__ping" {
		t.Fatalf("product hat lists %q — the NSAuthor filter must apply to a session set exactly as to static tools", got)
	}
}

// B2 "bound to the caller": session ids come from the client, so a DIFFERENT token presenting the same id must get its
// own set — built for it — and never the other subject's.
func TestSessionTools_TheSetIsBoundToTheCallerNotOnlyTheSession(t *testing.T) {
	h := newHookServer(t, func(_ string, prin Principal) SessionSet {
		if prin.Hat == role.Test {
			return SessionSet{Tools: twoPlaneSet(), Instructions: "for-" + prin.Subject}
		}
		ts, _ := NewToolSet(Tool{Name: "runner__ping", Namespace: NSRunner, InputSchema: sch(), Handler: okHandler("pong")})
		return SessionSet{Tools: ts}
	})
	initialize(t, h.Server, "shared", "test:a")
	// The product caller never initialized: the session is ready (by a's handshake), so its first use builds its own set.
	got := listNames(t, h.Server, "shared", "product:b")
	if strings.Contains(strings.Join(got, ","), "author") {
		t.Fatalf("a product caller on a test session's id was served the test set: %v", got)
	}
	if h.buildsFor("b") != 1 || h.buildsFor("a") != 1 {
		t.Fatalf("builds a=%d b=%d, want 1 each", h.buildsFor("a"), h.buildsFor("b"))
	}
	// And calling the test set's tool by name, as the product caller, is refused by the scope check.
	raw, _ := h.Dispatch("shared", "product:b", reqBytes(3, "tools/call", map[string]any{"name": "author_write", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeMethodNotFound && r.Error.Code != CodeUnauthorized {
		t.Fatalf("a product caller reached a test-set tool by name: %s", raw)
	}
}

func TestSessionTools_AnAdoptedSessionBuildsOnFirstUse(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	h.AdoptSession("adopted") // a router restart: the client's id is new to this process
	if got := listNames(t, h.Server, "adopted", "test:a"); len(got) != 2 {
		t.Fatalf("first use did not build the set: %v", got)
	}
	raw, _ := h.Dispatch("adopted", "test:a", reqBytes(3, "tools/call", map[string]any{"name": "author_write", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error != nil {
		t.Fatalf("call on an adopted session: %s", raw)
	}
	if h.builds.Load() != 1 {
		t.Fatalf("%d builds, want 1", h.builds.Load())
	}
}

func TestSessionTools_TwoParallelFirstUsesBuildOnce(t *testing.T) {
	release := make(chan struct{})
	h := newHookServer(t, func(string, Principal) SessionSet {
		<-release
		return SessionSet{Tools: twoPlaneSet()}
	})
	h.AdoptSession("x")
	var wg sync.WaitGroup
	results := make([][]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = listNames(t, h.Server, "x", "test:a")
		}(i)
	}
	waitFor(t, func() bool { return h.builds.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // give a second (wrong) build every chance to start
	close(release)
	wg.Wait()
	if h.builds.Load() != 1 {
		t.Fatalf("two parallel first uses built %d sets, want 1", h.builds.Load())
	}
	for i, r := range results {
		if len(r) != 2 {
			t.Fatalf("caller %d got %v", i, r)
		}
	}
}

// The build makes network calls in the router; holding the server lock through it would stall every session.
func TestSessionTools_TheBuildHoldsNoServerLock(t *testing.T) {
	h := newHookServer(t, nil)
	var blocked atomic.Bool
	h.SessionTools = func(string, Principal) SessionSet {
		done := make(chan struct{})
		go func() {
			h.NewSession("someone-else") // takes s.mu
			h.Dispatch("someone-else", "test:z", reqBytes(1, "tools/list", nil))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			blocked.Store(true) // NOT a panic: the server recovers a hook's panic by design, which would hide this
		}
		return SessionSet{Tools: twoPlaneSet()}
	}
	initialize(t, h.Server, "s", "test:a")
	if blocked.Load() {
		t.Fatal("the server lock was held while the session's tools were built — every other session would stall")
	}
}

// Nor is any OTHER lock: while one caller's build hangs (an upstream that does not answer), another session's whole
// session start — with a build of its own — completes.
func TestSessionTools_ASlowBuildDoesNotHoldBackAnotherSessionsBuild(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHookServer(t, func(_ string, prin Principal) SessionSet {
		if prin.Subject == "slow" {
			<-release
		}
		return SessionSet{Tools: twoPlaneSet()}
	})
	// raw Dispatch, not the t-using helpers: this goroutine may outlive the test (it ends when release closes)
	go h.Dispatch("s-slow", "test:slow", reqBytes(1, "initialize", map[string]any{}))
	waitFor(t, func() bool { return h.buildsFor("slow") == 1 })
	done := make(chan struct{})
	go func() {
		initialize(t, h.Server, "s-fast", "test:fast")
		listNames(t, h.Server, "s-fast", "test:fast")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a session start waited for ANOTHER session's build — builds are serialised")
	}
	if h.buildsFor("fast") != 1 {
		t.Fatalf("the fast session's set was not built by its own hook call: %d", h.buildsFor("fast"))
	}
}

// The caller binding holds under concurrency too: a product caller presenting a session id whose TEST build is still in
// flight gets its own build, never the test caller's result.
func TestSessionTools_ACallerNeverJoinsAnotherCallersBuildInFlight(t *testing.T) {
	release := make(chan struct{})
	h := newHookServer(t, func(_ string, prin Principal) SessionSet {
		if prin.Subject == "a" {
			<-release
			return SessionSet{Tools: twoPlaneSet()}
		}
		ts, _ := NewToolSet(Tool{Name: "runner__ping", Namespace: NSRunner, InputSchema: sch(), Handler: okHandler("pong")})
		return SessionSet{Tools: ts}
	})
	h.AdoptSession("shared")
	aDone := make(chan struct{})
	go func() { // raw Dispatch: no t in a goroutine that ends after release closes
		h.Dispatch("shared", "test:a", reqBytes(9, "tools/list", nil))
		close(aDone)
	}()
	defer func() { <-aDone }()
	var once sync.Once
	releaseA := func() { once.Do(func() { close(release) }) }
	defer releaseA() // runs before the wait for a above: a's build always ends
	waitFor(t, func() bool { return h.buildsFor("a") == 1 })

	// b's list must neither wait for a's build nor get its result. Bounded, so a caller that joined a's build fails
	// here instead of hanging the test until go test's own timeout.
	bRaw := make(chan []byte, 1)
	go func() {
		raw, _ := h.Dispatch("shared", "product:b", reqBytes(9, "tools/list", nil))
		bRaw <- raw
	}()
	var raw []byte
	select {
	case raw = <-bRaw:
	case <-time.After(2 * time.Second):
		releaseA()
		t.Fatal("the product caller waited for the test caller's build in flight — it joined that build")
	}
	releaseA()
	var r struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Result.Tools) != 1 || r.Result.Tools[0].Name != "runner__ping" || h.buildsFor("b") != 1 {
		t.Fatalf("the product caller got %s (builds b=%d) — want its own set: runner__ping only", raw, h.buildsFor("b"))
	}
}

func TestSessionTools_InitializeJoinsABuildAlreadyInFlight(t *testing.T) {
	release := make(chan struct{})
	h := newHookServer(t, func(string, Principal) SessionSet {
		<-release
		return SessionSet{Tools: twoPlaneSet()}
	})
	h.AdoptSession("s")
	first := make(chan []string, 1)
	go func() { first <- listNames(t, h.Server, "s", "test:a") }()
	waitFor(t, func() bool { return h.builds.Load() == 1 })
	second := make(chan map[string]any, 1)
	go func() { second <- initialize(t, h.Server, "s", "test:a") }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-first
	<-second
	if h.builds.Load() != 1 {
		t.Fatalf("initialize started a second build for the same (session, caller): %d builds", h.builds.Load())
	}
}

func TestSessionTools_APanickingHookReleasesItsWaitersAndStoresNothing(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	h := newHookServer(t, func(_ string, prin Principal) SessionSet {
		if prin.Subject == "bad" {
			if calls.Add(1) == 1 {
				<-release
			}
			panic("boom")
		}
		return SessionSet{Tools: twoPlaneSet()}
	})
	h.AdoptSession("s")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := listNames(t, h.Server, "s", "test:bad"); len(got) != 0 {
				t.Errorf("a failed build listed %v", got)
			}
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("the waiters started their own builds: %d", calls.Load())
	}
	// Nothing was stored, so the next initialize builds again — and says why the list is empty.
	if res := initialize(t, h.Server, "s", "test:bad"); res["instructions"] != buildFailedText {
		t.Fatalf("a failed build's session start did not say so: %v", res)
	}
	// Every other session is still served.
	initialize(t, h.Server, "other", "test:good")
	if got := listNames(t, h.Server, "other", "test:good"); len(got) != 2 {
		t.Fatalf("the server stopped serving after a hook panic: %v", got)
	}
}

func TestSessionTools_ASessionEvictedDuringTheBuildIsAnsweredButNotStored(t *testing.T) {
	release := make(chan struct{})
	h := newHookServer(t, func(string, Principal) SessionSet {
		<-release
		return SessionSet{Tools: twoPlaneSet()}
	})
	h.AdoptSession("gone")
	got := make(chan []string, 1)
	go func() { got <- listNames(t, h.Server, "gone", "test:a") }()
	waitFor(t, func() bool { return h.builds.Load() == 1 })
	h.EndSession("gone")
	close(release)
	if names := <-got; len(names) != 2 {
		t.Fatalf("the request in flight was not answered with its set: %v", names)
	}
	h.mu.Lock()
	_, present := h.sessions["gone"]
	h.mu.Unlock()
	if present {
		t.Fatal("storing the set re-created a session that was ended during the build")
	}
}

// A failed first-use set is rebuilt after RetryAfter — in the BACKGROUND: the request that finds it due is answered with
// the stored set at once, so a call never waits for the upstream's timeout because of a retry. A set built at initialize
// is kept for the session (its client was told to reconnect).
func TestSessionTools_AFailedFirstUseSetIsRebuiltInTheBackground_AnInitializeSetIsKept(t *testing.T) {
	release := make(chan struct{})
	var aBuilds atomic.Int64
	h := newHookServer(t, func(_ string, prin Principal) SessionSet {
		if prin.Subject == "a" && aBuilds.Add(1) == 2 {
			<-release // the retry is slow — as an upstream that times out would be
		}
		return SessionSet{Tools: twoPlaneSet(), Instructions: "part down", RetryAfter: 80 * time.Millisecond}
	})
	h.AdoptSession("adopted")
	listNames(t, h.Server, "adopted", "test:a")
	listNames(t, h.Server, "adopted", "test:a")
	if h.buildsFor("a") != 1 {
		t.Fatalf("rebuilt before RetryAfter: %d", h.buildsFor("a"))
	}
	time.Sleep(120 * time.Millisecond)
	start := time.Now()
	if got := listNames(t, h.Server, "adopted", "test:a"); len(got) != 2 {
		t.Fatalf("the due request was not answered with the stored set: %v", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the request waited %v for the retry — a retry must never sit on the request path", took)
	}
	waitFor(t, func() bool { return h.buildsFor("a") == 2 })
	// Calls made while the retry is still running are answered at once too, and start no second retry.
	listNames(t, h.Server, "adopted", "test:a")
	if h.buildsFor("a") != 2 {
		t.Fatalf("a second retry started while one was in flight: %d", h.buildsFor("a"))
	}
	close(release)

	initialize(t, h.Server, "started", "test:b")
	time.Sleep(120 * time.Millisecond)
	listNames(t, h.Server, "started", "test:b")
	if h.buildsFor("b") != 1 {
		t.Fatalf("a set built at initialize was rebuilt (%d builds) — its client was told to reconnect", h.buildsFor("b"))
	}
}

// The wait before the next retry doubles with every failed retry, up to 2^5 × RetryAfter.
func TestSessionTools_FailedRetriesBackOff(t *testing.T) {
	built := time.Now()
	for _, c := range []struct {
		attempt int
		after   time.Duration
		due     bool
	}{{0, 29 * time.Second, false}, {0, 30 * time.Second, true}, {1, 59 * time.Second, false}, {1, 60 * time.Second, true},
		{3, 239 * time.Second, false}, {3, 240 * time.Second, true}, {9, 959 * time.Second, false}, {9, 960 * time.Second, true}} {
		st := &storedSet{set: SessionSet{RetryAfter: 30 * time.Second}, built: built, firstUse: true, attempt: c.attempt}
		if got := st.expired(built.Add(c.after)); got != c.due {
			t.Errorf("attempt %d, %v after the build: due=%v, want %v", c.attempt, c.after, got, c.due)
		}
	}
	if (&storedSet{set: SessionSet{RetryAfter: time.Second}, built: built}).expired(built.Add(time.Hour)) {
		t.Error("a set built at initialize (firstUse=false) came due for a retry")
	}
}

// One ToolSet, a thousand sessions: the definitions are held once (a decoded list per session, at up to 4096
// sessions, would cost hundreds of MB).
func TestSessionTools_SessionsShareOneToolSet(t *testing.T) {
	shared := twoPlaneSet()
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: shared} })
	for i := 0; i < 1000; i++ {
		initialize(t, h.Server, fmt.Sprintf("s%d", i), "test:a")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, sess := range h.sessions {
		for _, st := range sess.sets {
			if st.set.Tools != shared {
				t.Fatal("a session holds its own copy of the tool definitions")
			}
			n++
		}
	}
	if n != 1000 {
		t.Fatalf("%d stored sets, want 1000", n)
	}
}

// The initialize path mints a session for any id it is given (the legacy POST path does); with sets stored that must
// count against maxSessions.
func TestSessionTools_TheInitializeMintPathRespectsMaxSessions(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	for i := 0; i < maxSessions+500; i++ {
		h.Dispatch(fmt.Sprintf("minted-%d", i), "test:a", reqBytes(1, "initialize", map[string]any{}))
	}
	h.mu.Lock()
	n := len(h.sessions)
	h.mu.Unlock()
	if n > maxSessions {
		t.Fatalf("%d sessions after %d initialize mints — the bound is %d", n, maxSessions+500, maxSessions)
	}
}

func TestSessionTools_TheClosedSchemaCheckIsNotAppliedToASessionTool(t *testing.T) {
	var got json.RawMessage
	closed := map[string]any{"type": "object", "additionalProperties": false,
		"properties": map[string]any{"instance_id": map[string]any{"type": "string"}}}
	ts, _ := NewToolSet(Tool{Name: "author_x", Namespace: NSAuthor, InputSchema: closed,
		Handler: func(args json.RawMessage, _ Principal) Outcome { got = args; return Ok("ran") }})
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: ts} })
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "author_x",
		"arguments": map[string]any{"instance_id": "i", "undeclared": "u"}}))
	if r := decode(t, raw); r.Error != nil {
		t.Fatalf("refused: %s", raw)
	}
	if !strings.Contains(string(got), "undeclared") {
		t.Fatalf("the handler did not receive the caller's arguments unchanged: %s", got)
	}
}

func TestSessionTools_AnAliasDispatchesToTheSameTool(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "author__write", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error != nil || !strings.Contains(string(raw), "written") {
		t.Fatalf("the old name did not reach the tool: %s", raw)
	}
	raw, _ = h.Dispatch("s", "product:b", reqBytes(2, "tools/call", map[string]any{"name": "author__write", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("an alias was a scope back door for the product hat: %s", raw)
	}
}

// ── UnknownTool ──

func TestUnknownTool_NilAndNilResultKeepMinus32601(t *testing.T) {
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeMethodNotFound || r.Error.Message != "unknown tool nope" {
		t.Fatalf("hook unset: %s", raw)
	}
	h.UnknownTool = func(string, string, json.RawMessage, Principal, any) (*Tool, *Outcome) { return nil, nil }
	raw, _ = h.Dispatch("s", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeMethodNotFound {
		t.Fatalf("hook answering nothing: %s", raw)
	}
}

func TestUnknownTool_GetsTheStateTheArgsAndTheCaller(t *testing.T) {
	state := &struct{ n int }{7}
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet(), State: state} })
	var gotState any
	var gotArgs json.RawMessage
	var gotPrin Principal
	h.UnknownTool = func(_ string, name string, args json.RawMessage, prin Principal, st any) (*Tool, *Outcome) {
		gotState, gotArgs, gotPrin = st, args, prin
		o := ToolErr(map[string]any{"error": "said why: " + name})
		return nil, &o
	}
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "author__gone", "arguments": map[string]any{"instance_id": "i"}}))
	if gotState != state {
		t.Fatal("UnknownTool did not receive the set's own State (by reference)")
	}
	if !strings.Contains(string(gotArgs), `"instance_id":"i"`) || gotPrin.Subject != "a" {
		t.Fatalf("args %s prin %+v", gotArgs, gotPrin)
	}
	if !strings.Contains(string(raw), "said why: author__gone") || !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("the hook's Outcome was not the answer: %s", raw)
	}
	if !strings.Contains(h.Metrics(), `tool="(unlisted)"`) || strings.Contains(h.Metrics(), `tool="author__gone"`) {
		t.Fatalf("an invented name became a metrics label:\n%s", h.Metrics())
	}
}

// A Tool returned by UnknownTool goes through the SAME scope checks as a set tool: the hook cannot open a back door.
func TestUnknownTool_AReturnedToolIsDispatchedThroughTheScopeChecks(t *testing.T) {
	var ran atomic.Int64
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: twoPlaneSet()} })
	h.UnknownTool = func(string, string, json.RawMessage, Principal, any) (*Tool, *Outcome) {
		return &Tool{Name: "author_late", Namespace: NSAuthor, InputSchema: sch(),
			Handler: func(json.RawMessage, Principal) Outcome { ran.Add(1); return Ok("late") }}, nil
	}
	initialize(t, h.Server, "p", "product:b")
	raw, _ := h.Dispatch("p", "product:b", reqBytes(2, "tools/call", map[string]any{"name": "author_late", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeUnauthorized || ran.Load() != 0 {
		t.Fatalf("a hook-returned author tool ran for the product hat: %s (ran %d)", raw, ran.Load())
	}
	// The refusal is the server's own, byte for byte: the instance read the way instanceOf reads it (a key that only
	// folds to instance_id included), and counted as "denied" — under the fixed label, never the name the caller sent.
	raw, _ = h.Dispatch("p", "product:b", reqBytes(3, "tools/call", map[string]any{"name": "author_late", "arguments": map[string]any{"INSTANCE_ID": "suta"}}))
	if want := `{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"not permitted for the product scope: author_late","data":{"instance_id":"suta"}}}`; string(raw) != want {
		t.Fatalf("product refusal\n got: %s\nwant: %s", raw, want)
	}
	if m := h.Metrics(); !strings.Contains(m, `tool="(unlisted)",instance_id="suta",outcome="denied"} 1`) || strings.Contains(m, `tool="author_late"`) {
		t.Fatalf("the refusal was not counted as denied under the fixed label:\n%s", m)
	}
	initialize(t, h.Server, "t", "test:a")
	raw, _ = h.Dispatch("t", "test:a", reqBytes(2, "tools/call", map[string]any{"name": "author_late", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error != nil || ran.Load() != 1 {
		t.Fatalf("a hook-returned tool did not run for the test hat: %s", raw)
	}
	if m := h.Metrics(); !strings.Contains(m, `tool="(unlisted)",instance_id="local",outcome="ok"} 1`) || strings.Contains(m, `tool="author_late"`) {
		t.Fatalf("a hook-returned tool's answer was not counted under the fixed label:\n%s", m)
	}
}

// ── Outcome.Result and error data: answers passed back unchanged (decision 14) ──

func TestOutcomeResult_IsTheWireResultUnchanged(t *testing.T) {
	upstream := `{"content":[{"type":"text","text":"{\"a\":1}"},{"type":"image","data":"QUJD","mimeType":"image/png"}],"isError":true,"structuredContent":{"k":[1,2]}}`
	ts, _ := NewToolSet(Tool{Name: "author_pass", Namespace: NSAuthor, InputSchema: sch(),
		Handler: func(json.RawMessage, Principal) Outcome { return Outcome{Result: json.RawMessage(upstream)} }},
		Tool{Name: "author_broken", Namespace: NSAuthor, InputSchema: sch(),
			Handler: func(json.RawMessage, Principal) Outcome { return Outcome{Result: json.RawMessage(`{not json`)} }})
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: ts} })
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(5, "tools/call", map[string]any{"name": "author_pass", "arguments": map[string]any{}}))
	want := `{"jsonrpc":"2.0","id":5,"result":` + upstream + `}`
	if string(raw) != want {
		t.Fatalf("the result was not passed back unchanged\n got: %s\nwant: %s", raw, want)
	}
	if !strings.Contains(h.Metrics(), `tool="author_pass",instance_id="local",outcome="tool_error"`) {
		t.Fatalf("the upstream's isError was not counted:\n%s", h.Metrics())
	}
	raw, _ = h.Dispatch("s", "test:a", reqBytes(6, "tools/call", map[string]any{"name": "author_broken", "arguments": map[string]any{}}))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeInternal {
		t.Fatalf("an invalid result was not refused: %s", raw)
	}
}

func TestOutcomeRPCError_DataIsPassedAsGivenAndAbsentStaysAbsent(t *testing.T) {
	ts, _ := NewToolSet(
		Tool{Name: "author_nodata", Namespace: NSAuthor, InputSchema: sch(), Handler: func(json.RawMessage, Principal) Outcome {
			return Outcome{RPCErr: &RPCError{Code: CodeInvalidParams, Message: "instance_id is required"}}
		}},
		Tool{Name: "author_data", Namespace: NSAuthor, InputSchema: sch(), Handler: func(json.RawMessage, Principal) Outcome {
			return Outcome{RPCErr: &RPCError{Code: CodeUnauthorized, Message: "no", Data: json.RawMessage(`{"instance_id":"x","n":[1]}`)}}
		}})
	h := newHookServer(t, func(string, Principal) SessionSet { return SessionSet{Tools: ts} })
	initialize(t, h.Server, "s", "test:a")
	raw, _ := h.Dispatch("s", "test:a", reqBytes(7, "tools/call", map[string]any{"name": "author_nodata", "arguments": map[string]any{}}))
	if want := `{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"instance_id is required"}}`; string(raw) != want {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
	raw, _ = h.Dispatch("s", "test:a", reqBytes(8, "tools/call", map[string]any{"name": "author_data", "arguments": map[string]any{}}))
	if want := `{"jsonrpc":"2.0","id":8,"error":{"code":-32001,"message":"no","data":{"instance_id":"x","n":[1]}}}`; string(raw) != want {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
}

func TestNewToolSet_SkipsDuplicatesAndCollidingAliases_FirstWins(t *testing.T) {
	ts, skipped := NewToolSet(
		Tool{Name: "a", Handler: okHandler(1), Aliases: []string{"a_old", "b"}},
		Tool{Name: "b", Handler: okHandler(2)},
		Tool{Name: "a", Handler: okHandler(3), Aliases: []string{"dup_alias"}},
		Tool{Name: "", Handler: okHandler(4)},
	)
	if got := strings.Join(ts.Names(), ","); got != "a,b" {
		t.Fatalf("names %q", got)
	}
	if _, ok := ts.lookup("dup_alias"); ok {
		t.Fatal("a skipped duplicate brought its aliases in")
	}
	if tl, _ := ts.lookup("b"); tl.Handler(nil, Principal{}).Payload != 2 {
		t.Fatal("an alias shadowed a real tool")
	}
	if tl, _ := ts.lookup("a_old"); tl.Handler(nil, Principal{}).Payload != 1 {
		t.Fatal("the first occurrence did not win")
	}
	if strings.Join(skipped, ",") != "a,,b" {
		t.Fatalf("skipped = %q, want [a, \"\", b] (the duplicate, the empty name, the colliding alias)", skipped)
	}
}
