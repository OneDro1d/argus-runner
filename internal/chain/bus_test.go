package chain

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The chain `bus` step against a FAKE subscription on a fake clock: nothing waits, nothing is dialled.

// fakeTap is a subscription whose contents depend on the injected clock: a message "arrives" when the clock passes its time.
type fakeTap struct {
	clk       *clock
	armed     time.Time
	msgs      []fakeMsg
	uncorr    int           // deliveries seen that carry no id
	lostAfter time.Duration // 0 = never lost
	lostWhy   string
	forceSeen int // > 0 overrides Seen
	overflow  bool
	closes    int
	mu        sync.Mutex
}

type fakeMsg struct {
	at   time.Duration
	body map[string]any
}

func newFakeTap(clk *clock) *fakeTap { return &fakeTap{clk: clk, armed: clk.Now()} }

func (f *fakeTap) elapsed() time.Duration { return f.clk.Now().Sub(f.armed) }

func (f *fakeTap) Snapshot(where map[string][]string, keep int) amqpengine.Snapshot {
	s := amqpengine.Snapshot{Uncorrelated: f.uncorr, Overflow: f.overflow}
	for _, m := range f.msgs {
		if m.at > f.elapsed() {
			continue
		}
		s.Correlated++
		pass := true
		for path, want := range where {
			got, _ := m.body[path].(string)
			hit := false
			for _, w := range want {
				hit = hit || got == w
			}
			pass = pass && hit
		}
		if !pass {
			continue
		}
		s.Count++
		if len(s.Messages) < keep {
			s.Messages = append(s.Messages, amqpengine.Message{Exchange: "x", RoutingKey: "a.b", MatchedBy: "body:correlation_id",
				Properties: map[string]any{"content_type": "application/json"}, Body: m.body})
		}
	}
	s.Seen = s.Correlated + s.Uncorrelated
	if f.forceSeen > 0 {
		s.Seen = f.forceSeen
	}
	return s
}

func (f *fakeTap) Lost() (amqpengine.LostInfo, bool) {
	if f.lostAfter > 0 && f.elapsed() >= f.lostAfter {
		return amqpengine.LostInfo{Reason: f.lostWhy, At: f.armed.Add(f.lostAfter)}, true
	}
	return amqpengine.LostInfo{}, false
}
func (f *fakeTap) ArmedAt() time.Time { return f.armed }
func (f *fakeTap) Close()             { f.mu.Lock(); f.closes++; f.mu.Unlock() }

func busClaims(t *testing.T, bullets ...string) []mcp.BodyAssert {
	t.Helper()
	a, err := scenario.BusStepClaims(bullets)
	if err != nil {
		t.Fatalf("claims %q: %v", bullets, err)
	}
	return a
}

func busSpec(clk *clock, s BusSpec) BusSpec {
	if s.Wait == 0 {
		s.Wait = 10 * time.Second
	}
	if s.Binding == "" {
		s.Binding = `exchange "orders.incoming", routing_key "orders.created"`
	}
	s.Now, s.Sleep = clk.Now, clk.Sleep
	return s
}

func runBus(t *testing.T, tap *fakeTap, spec BusSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec) report.StepResult {
	t.Helper()
	return BusStep("b", spec, want, save, func() BusTap { return tap }).Run(testCID, map[string]string{})
}

func info(id string) map[string]any { return map[string]any{"order_id": id, "level": "info"} }

// T11: a presence claim whose claims are all monotone passes as soon as the message is there, not at the end of the wait.
func TestBusStep_PresenceReturnsEarly(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: 2 * time.Second, body: info("o-1")}}
	start := clk.Now()
	res := runBus(t, tap, busSpec(clk, BusSpec{}), busClaims(t, "body has count >= 1"), nil)
	if res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if d := clk.Now().Sub(start); d < 2*time.Second || d > 2*time.Second+300*time.Millisecond {
		t.Fatalf("passed after %s, want about 2s of a 10s wait", d)
	}
}

// A claim that is not monotone (count == 1 fails again if a second message comes) is judged ONCE, when the wait ends.
func TestBusStep_ANonMonotoneClaimWaitsTheWholeWindow(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: 2 * time.Second, body: info("o-1")}, {at: 8 * time.Second, body: info("o-2")}}
	start := clk.Now()
	res := runBus(t, tap, busSpec(clk, BusSpec{}), busClaims(t, "body has count == 2"), nil)
	if res.Status != "passed" || clk.Now().Sub(start) < 10*time.Second {
		t.Fatalf("%s after %s: %s - count == 2 holds only at the end", res.Status, clk.Now().Sub(start), res.Observed)
	}
}

// T12: an absence (with its anchor) passes only after the FULL wait; a late error-level message fails it.
func TestBusStep_AbsenceWithAnchorPassesOnlyAfterTheFullWait(t *testing.T) {
	absent := []string{"body has count == 0", "body has correlated >= 1"}
	where := map[string][]string{"level": {"error"}}

	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}}
	start := clk.Now()
	res := runBus(t, tap, busSpec(clk, BusSpec{Where: where}), busClaims(t, absent...), nil)
	if res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if d := clk.Now().Sub(start); d < 10*time.Second {
		t.Fatalf("an absence passed after %s: it must watch the whole 10s", d)
	}

	// an error-level message at 9s, inside the window
	clk2 := newClock()
	tap2 := newFakeTap(clk2)
	tap2.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}, {at: 9 * time.Second, body: map[string]any{"level": "error"}}}
	res2 := runBus(t, tap2, busSpec(clk2, BusSpec{Where: where}), busClaims(t, absent...), nil)
	if res2.Status != "failed" || len(res2.FailedClaims) == 0 {
		t.Fatalf("an error-level message at 9s must fail the absence: %s %s", res2.Status, res2.Observed)
	}
}

// The anchor failing: nothing under this run's id was heard, so silence is a failure, not a pass.
func TestBusStep_NothingCorrelatedFailsTheAnchorAndSaysWhy(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.uncorr = 38
	res := runBus(t, tap, busSpec(clk, BusSpec{}), busClaims(t, "body has count == 0", "body has correlated >= 1"), nil)
	if res.Status != "failed" || !strings.Contains(res.Observed, "heard 38 delivery(ies) and none carried this run's correlation id") {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
}

// T13 - THE KEY FAIL-CLOSED TEST: a subscription lost mid-window is `error`, its claims are not judged although they
// would HOLD (count == 0 with an anchor), and the chain stops.
func TestBusStep_ALostSubscriptionIsAnErrorNotAnAbsence(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}}
	tap.lostAfter, tap.lostWhy = 5*time.Second, "the connection closed by the broker (320: CONNECTION_FORCED)"
	step := BusStep("b", busSpec(clk, BusSpec{Where: map[string][]string{"level": {"error"}}}), busClaims(t, "body has count == 0", "body has correlated >= 1"), nil, func() BusTap { return tap })
	after := false
	next := Step{Name: "after", Run: func(string, map[string]string) report.StepResult {
		after = true
		return report.StepResult{Status: "passed"}
	}}
	res := Run(testCID, []Step{step, next})
	if res.Status != report.StatusError {
		t.Fatalf("scenario status = %s, want error: %+v", res.Status, res.Steps)
	}
	got := res.Steps[0]
	if got.Status != report.StatusError || !strings.Contains(got.Observed, "was lost 5s after it was armed") ||
		!strings.Contains(got.Observed, "CONNECTION_FORCED") || !strings.Contains(got.Observed, "claims were not judged") {
		t.Fatalf("step = %s: %s", got.Status, got.Observed)
	}
	if len(got.AssertionsEnforced) != 0 || len(got.FailedClaims) != 0 {
		t.Fatalf("the claims were judged on a lost subscription: %+v", got)
	}
	if after || res.Steps[1].Status != report.StepNotMeasured {
		t.Fatalf("the chain must stop at the lost subscription; next step ran=%v status=%s", after, res.Steps[1].Status)
	}
}

// Lost between the last read and the deadline is caught by the second health check, not judged.
func TestBusStep_LostJustBeforeTheDeadlineIsStillAnError(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}}
	tap.lostAfter, tap.lostWhy = 9950*time.Millisecond, "the channel closed"
	// a tick of 100ms: the loop reads at 9.9s (healthy), sleeps to 10s, and only the final check sees the loss
	res := runBus(t, tap, busSpec(clk, BusSpec{Where: map[string][]string{"level": {"error"}}}), busClaims(t, "body has count == 0", "body has correlated >= 1"), nil)
	if res.Status != report.StatusError {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
}

// T14: a binding that delivered more than the step can judge is `error`, never a count.
func TestBusStep_SeenOverTheCapIsAnError(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}}
	tap.forceSeen = 21
	res := runBus(t, tap, busSpec(clk, BusSpec{SeenMax: 20}), busClaims(t, "body has count >= 1"), nil)
	if res.Status != report.StatusError || !strings.Contains(res.Observed, "too wide") || !strings.Contains(res.Observed, "narrow `routing_key`") {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	// exactly at the cap is fine
	tap.forceSeen = 20
	if res := runBus(t, tap, busSpec(clk, BusSpec{SeenMax: 20}), busClaims(t, "body has count >= 1"), nil); res.Status != "passed" {
		t.Fatalf("at the cap: %s %s", res.Status, res.Observed)
	}
	// the subscription's own overflow flag (more correlated messages than it keeps) is an error too
	tap2 := newFakeTap(newClock())
	tap2.overflow = true
	if res := runBus(t, tap2, busSpec(tap2.clk, BusSpec{}), busClaims(t, "body has count >= 1"), nil); res.Status != report.StatusError {
		t.Fatalf("overflow: %s %s", res.Status, res.Observed)
	}
}

// T16: redaction. A delivered message that echoes the broker password is scrubbed before it is kept, a token is redacted, the run's
// own id survives, and Observed carries counts only.
func TestBusStep_RedactionAndObservedCarryNoMessageContent(t *testing.T) {
	const pw = "S3cretBrokerPw"
	const tok = "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	scrub := func(s string) string { return strings.ReplaceAll(s, pw, "[credential]") }
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: map[string]any{
		"correlation_id": testCID, "note": "amqp://user:" + pw + "@broker.internal and token " + tok,
		"nested": map[string]any{"deep": "again " + pw}, "list": []any{"x " + pw},
	}}}
	save := map[string]scenario.SaveSpec{"note": {Path: "messages.0.body.note"}, "deep": {Path: "messages.0.body.nested.deep"}, "cid": {Path: "messages.0.body.correlation_id"}}
	res := runBus(t, tap, busSpec(clk, BusSpec{Scrub: scrub}), busClaims(t, "body has count >= 1", "body has messages.0.body.correlation_id containing "+testCID), save)
	if res.Status != "passed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	for k, v := range res.Captured {
		if strings.Contains(v, pw) || strings.Contains(v, tok) {
			t.Fatalf("captured %s leaks: %q", k, v)
		}
	}
	if !strings.Contains(res.Captured["note"], "[credential]") || !strings.Contains(res.Captured["note"], "[redacted]") {
		t.Fatalf("the note must be scrubbed and token-redacted: %q", res.Captured["note"])
	}
	if res.Captured["cid"] != testCID {
		t.Fatalf("the run's own id must survive the redaction: %q", res.Captured["cid"])
	}
	// a miss: Observed names counts and the held-out note, never a word of the message or of the claim
	res = runBus(t, tap, busSpec(clk, BusSpec{Scrub: scrub}), busClaims(t, "body has count >= 1", "body has messages.0.body.note containing CLAIMED-SENTINEL"), nil)
	if res.Status != "failed" {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	for _, leak := range []string{pw, "CLAIMED-SENTINEL", "broker.internal", "again", tok} {
		if strings.Contains(res.Observed, leak) {
			t.Fatalf("Observed must carry counts only, found %q in %q", leak, res.Observed)
		}
	}
	if !strings.Contains(res.Observed, "heard 1 delivery(ies), 1 carrying this run's id, 1 passing `where`") || !strings.Contains(res.Observed, chainClaimsNote) {
		t.Fatalf("Observed = %q", res.Observed)
	}
	// and the lost-subscription sentence is scrubbed of the broker's own words too
	tap.lostAfter, tap.lostWhy = 5*time.Second, "closed (320: user "+pw+")"
	res = runBus(t, tap, busSpec(clk, BusSpec{Scrub: scrub}), busClaims(t, "body has count == 1"), nil)
	if res.Status != report.StatusError || strings.Contains(res.Observed, pw) {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	b := fmt.Sprintf("%+v", res)
	if strings.Contains(b, pw) {
		t.Fatalf("leak in %s", b)
	}
}

// A message body longer than the cap is cut into a string, not left to break the document.
func TestBusStep_ALongBodyIsCutOnARuneBoundary(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	tap.msgs = []fakeMsg{{at: time.Second, body: map[string]any{"pad": strings.Repeat("é", scenario.BusMessageMaxBytes)}}}
	res := runBus(t, tap, busSpec(clk, BusSpec{}), busClaims(t, "body has count >= 1"), map[string]scenario.SaveSpec{"b": {Path: "messages.0.body"}})
	if res.Status != "passed" || len(res.Captured["b"]) > scenario.BusMessageMaxBytes {
		t.Fatalf("%s: captured %d bytes", res.Status, len(res.Captured["b"]))
	}
}

func TestBusStep_AnUnsavedVariableInWhereIsNotMeasured(t *testing.T) {
	clk := newClock()
	tap := newFakeTap(clk)
	st := BusStep("b", busSpec(clk, BusSpec{Where: map[string][]string{"order_id": {"${saved.oid}"}}}), busClaims(t, "body has count >= 1"), nil, func() BusTap { return tap })
	if err := st.Needs(map[string]string{}); err == nil || !strings.Contains(err.Error(), "oid") {
		t.Fatalf("Needs must name the unsaved variable, got %v", err)
	}
	// bound, it filters
	tap.msgs = []fakeMsg{{at: time.Second, body: info("o-1")}, {at: time.Second, body: info("o-2")}}
	res := st.Run(testCID, map[string]string{"oid": "o-2"})
	if res.Status != "passed" || !strings.Contains(res.Observed, "2 carrying this run's id, 1 passing `where`") {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
	if res := st.Run(testCID, map[string]string{"oid": ""}); res.Status != "failed" || !strings.Contains(res.Observed, "is empty once its ${saved.<var>} is bound") {
		t.Fatalf("an empty bound value would match nothing: %s %s", res.Status, res.Observed)
	}
}

func TestBusStep_AnUnarmedStepIsAnError(t *testing.T) {
	clk := newClock()
	res := BusStep("b", busSpec(clk, BusSpec{}), busClaims(t, "body has count >= 1"), nil, func() BusTap { return nil }).Run(testCID, map[string]string{})
	if res.Status != report.StatusError || !strings.Contains(res.Observed, "was not armed") {
		t.Fatalf("%s: %s", res.Status, res.Observed)
	}
}

// ── the hub ─────────────────────────────────────────────────────────────────────────────────────────

type fakeBusConn struct {
	subs    []string
	out     amqpengine.Outcome
	taps    []*fakeTap
	closed  int
	clk     *clock
	failOn  int // the n-th Subscribe (1-based) is refused
	nSub    int
	opts    []amqpengine.SubscribeOptions
	binding [][2]string
}

func (f *fakeBusConn) Subscribe(queue, exchange, rk string, o amqpengine.SubscribeOptions) (BusTap, amqpengine.Outcome) {
	f.nSub++
	f.subs = append(f.subs, queue)
	f.opts = append(f.opts, o)
	f.binding = append(f.binding, [2]string{exchange, rk})
	if f.failOn == f.nSub {
		return nil, f.out
	}
	tp := newFakeTap(f.clk)
	f.taps = append(f.taps, tp)
	return tp, amqpengine.Outcome{OK: true}
}
func (f *fakeBusConn) Close() error { f.closed++; return nil }

func TestBusHub_OneConnectionPerBrokerAndEverythingClosedOnce(t *testing.T) {
	conn := &fakeBusConn{clk: newClock()}
	dials := 0
	hub := NewBusHub(func(env string) (BusConn, error) { dials++; return conn, nil })
	for i, q := range []string{"argus-tap-c-1", "argus-tap-c-2"} {
		if _, err := hub.Arm(BusArm{URLEnv: "BUS_URL", Queue: q, Exchange: "x", RoutingKey: fmt.Sprintf("k.%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if dials != 1 || len(conn.subs) != 2 {
		t.Fatalf("dials=%d subscriptions=%d, want one shared connection", dials, len(conn.subs))
	}
	hub.Close()
	hub.Close() // idempotent
	if conn.closed != 1 || conn.taps[0].closes != 1 || conn.taps[1].closes != 1 {
		t.Fatalf("connection closed %d times, taps %d/%d, want exactly once each", conn.closed, conn.taps[0].closes, conn.taps[1].closes)
	}
	(*BusHub)(nil).Close() // safe on nil
}

func TestBusHub_RefusalsAreScrubbedAndNamed(t *testing.T) {
	t.Setenv("BUS_URL_FOR_TEST", "amqp://user:S3cretBrokerPw@broker.internal:5672/")
	conn := &fakeBusConn{clk: newClock(), failOn: 1, out: amqpengine.Outcome{Code: 403, Text: "ACCESS_REFUSED amqp://user:S3cretBrokerPw@broker.internal:5672/"}}
	hub := NewBusHub(func(string) (BusConn, error) { return conn, nil })
	_, err := hub.Arm(BusArm{URLEnv: "BUS_URL_FOR_TEST", Queue: "q", Exchange: "x", RoutingKey: "a.b"})
	if err == nil || !strings.Contains(err.Error(), "(403)") || strings.Contains(err.Error(), "S3cretBrokerPw") || strings.Contains(err.Error(), "broker.internal") {
		t.Fatalf("a 403 must be named and the URL scrubbed: %v", err)
	}
	hub2 := NewBusHub(func(string) (BusConn, error) {
		return nil, errors.New("dial: amqp://user:S3cretBrokerPw@broker.internal:5672/: connection refused")
	})
	_, err = hub2.Arm(BusArm{URLEnv: "BUS_URL_FOR_TEST", Queue: "q", Exchange: "x", RoutingKey: "a.b"})
	if err == nil || !strings.Contains(err.Error(), "connect (url from $BUS_URL_FOR_TEST) failed") || strings.Contains(err.Error(), "S3cretBrokerPw") {
		t.Fatalf("a dial failure must be named and the URL scrubbed: %v", err)
	}
}
