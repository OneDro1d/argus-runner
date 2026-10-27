package chain

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE "bus" CHAIN STEP: an AMQP subscription as evidence, judged by the `body …` claims on ONE JSON
// document:
//
//	{"count":N,"correlated":N,"seen":N,"uncorrelated":N,"undecodable":N,"messages":[{…}]}
//
// The subscription is armed by the executor BEFORE the chain's first step (BusHub.Arm) and torn down on every path
// (BusHub.Close); this step only READS it, where it stands in the chain.
//
// ⛔ NO PARALLEL EXECUTOR AND NO SECOND CLAIM GRAMMAR (as the db and logs steps): a Step for the SAME chain.Run, claims by
// scenario.ParseBodyAsserts judged by mcp.BodyAssertsMissReason, `save` through saveFromHTTP.
//
// ⛔ A LOST SUBSCRIPTION NEVER MAKES `count == 0` HOLD. A subscription that stopped hearing at any moment before the
// judgement is `error`; the claims are not judged at all. A binding that carried more than scenario.BusSeenMax deliveries,
// or more correlated messages than the subscription keeps, is `error` too: a count taken over part of the traffic is
// never reported as a count.
//
// ⛔ THE MESSAGES ARE THE SYSTEM'S OWN WORDS. They reach an author only (failed_claims, saved values). Observed carries
// counts and the subscription's health and nothing of a message; every string of a kept message passes the credential
// scrub and the token redaction BEFORE it is kept, with the run's own id held out.

// BusTap is the slice of a subscription a bus step reads. *amqpengine.Subscription satisfies it; tests substitute a fake.
type BusTap interface {
	Snapshot(where map[string][]string, keep int) amqpengine.Snapshot
	Lost() (amqpengine.LostInfo, bool)
	ArmedAt() time.Time
	Close()
}

// BusConn is the slice of amqpengine a bus step's arming drives: one connection, any number of subscriptions on it.
type BusConn interface {
	Subscribe(queue, exchange, routingKey string, o amqpengine.SubscribeOptions) (BusTap, amqpengine.Outcome)
	Close() error
}

// BusDialer opens a BusConn from an environment variable NAME (never a URL).
type BusDialer func(urlEnv string) (BusConn, error)

type engineBusConn struct{ e *amqpengine.Engine }

func (c engineBusConn) Subscribe(queue, exchange, routingKey string, o amqpengine.SubscribeOptions) (BusTap, amqpengine.Outcome) {
	s, out := c.e.Subscribe(queue, exchange, routingKey, o)
	if s == nil {
		return nil, out // never a typed-nil *Subscription inside a non-nil interface
	}
	return s, out
}
func (c engineBusConn) Close() error { return c.e.Close() }

// DialBus is the production BusDialer: amqpengine.Connect, which reads the URL from urlEnv and keeps no copy of it.
func DialBus(urlEnv string) (BusConn, error) {
	e, err := amqpengine.Connect(urlEnv)
	if err != nil {
		return nil, err
	}
	return engineBusConn{e}, nil
}

// BusArm is one subscription to arm.
type BusArm struct {
	URLEnv, Queue, Exchange, RoutingKey string
	Opts                                amqpengine.SubscribeOptions
}

// BusHub arms the chain's subscriptions and owns their teardown. One connection per broker (per urlEnv) is shared by the
// subscriptions on it.
type BusHub struct {
	dial BusDialer

	mu    sync.Mutex
	conns map[string]BusConn
	order []string
	taps  []BusTap
}

// NewBusHub returns a hub that dials with dial.
func NewBusHub(dial BusDialer) *BusHub { return &BusHub{dial: dial, conns: map[string]BusConn{}} }

// Arm subscribes and returns only once the broker has confirmed the consumer. The error is a sentence for the report:
// the broker URL and its credentials are scrubbed out of it.
func (h *BusHub) Arm(a BusArm) (BusTap, error) {
	h.mu.Lock()
	conn, have := h.conns[a.URLEnv]
	h.mu.Unlock()
	if !have {
		c, err := h.dial(a.URLEnv)
		if err != nil {
			return nil, fmt.Errorf("amqp connect (url from $%s) failed: %s", a.URLEnv, scrubBroker(err.Error(), a.URLEnv))
		}
		h.mu.Lock()
		h.conns[a.URLEnv] = c
		h.order = append(h.order, a.URLEnv)
		h.mu.Unlock()
		conn = c
	}
	tap, out := conn.Subscribe(a.Queue, a.Exchange, a.RoutingKey, a.Opts)
	if tap == nil || !out.OK {
		code := ""
		if out.Code != 0 {
			code = fmt.Sprintf(" (%d)", out.Code)
		}
		return nil, fmt.Errorf("the broker refused the subscription%s: %s", code, scrubBroker(out.Text, a.URLEnv))
	}
	h.mu.Lock()
	h.taps = append(h.taps, tap)
	h.mu.Unlock()
	return tap, nil
}

// Close tears every subscription down, then the connections. Idempotent, safe on a nil hub, and it never changes a verdict.
func (h *BusHub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	taps, order, conns := h.taps, h.order, h.conns
	h.taps, h.order, h.conns = nil, nil, map[string]BusConn{}
	h.mu.Unlock()
	for _, t := range taps {
		t.Close()
	}
	for _, env := range order {
		_ = conns[env].Close()
	}
}

// BusSpec is one bus step as the executor resolved it.
type BusSpec struct {
	// Binding names what was subscribed to, for the step's own sentences (exchange and routing key are not secrets).
	Binding string
	// Where is the step's `where` (the run's own id already filled in; a ${saved.<var>} is bound at run time).
	Where map[string][]string
	// Wait is how long the step watches, measured from when the step starts.
	Wait time.Duration
	// Tick is how often the subscription is read; 0 = 100ms.
	Tick time.Duration
	// Scrub cleans a message of the executor's credentials (nil = none).
	Scrub func(string) string
	// SeenMax overrides scenario.BusSeenMax (tests); 0 = the constant.
	SeenMax int
	// Now and Sleep are the clock; nil = time.Now and time.Sleep. Tests inject theirs, so nothing waits.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// BusStep builds the bus step. want is the step's `body …` claims; save is an http step's. tap returns the subscription the
// executor armed for this step before the chain started (it is read at run time, so the step can be built before arming).
func BusStep(name string, spec BusSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, tap func() BusTap) Step {
	scrub := func(s string) string {
		if spec.Scrub != nil {
			s = spec.Scrub(s)
		}
		return s
	}
	now, sleep := spec.Now, spec.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	tick := spec.Tick
	if tick <= 0 {
		tick = 100 * time.Millisecond
	}
	seenMax := spec.SeenMax
	if seenMax <= 0 {
		seenMax = scenario.BusSeenMax
	}
	early := scenario.BusStepEarlyExit(want)
	enforced := func() []string { return EnforcedAssertions(mcp.Expect{Body: want}) }

	bindWhere := func(vars map[string]string) (map[string][]string, error) {
		if len(spec.Where) == 0 {
			return nil, nil
		}
		out := make(map[string][]string, len(spec.Where))
		for path, vals := range spec.Where {
			for _, v := range vals {
				b, err := bindParam(v, vars)
				if err != nil {
					return nil, err
				}
				if v != "" && b == "" {
					// ⛔ an empty value would match nothing, and the absence claim would hold for the wrong reason
					return nil, fmt.Errorf("`where` value of %q is empty once its ${saved.<var>} is bound - nothing was measured", path)
				}
				out[path] = append(out[path], b)
			}
		}
		return out, nil
	}
	needs := func(vars map[string]string) error {
		if _, err := bindWhere(vars); err != nil {
			return err
		}
		return needsBodyAsserts(want, vars)
	}

	run := func(cid string, vars map[string]string) report.StepResult {
		where, err := bindWhere(vars)
		if err != nil {
			// An authoring error (an unresolved ${saved.<var>}), never retried and nothing is read.
			return report.StepResult{Status: "failed", Observed: err.Error()}
		}
		boundWant, cerr := bindBodyAsserts(want, vars)
		if cerr != nil {
			return report.StepResult{Status: "failed", Observed: "content assertion: " + cerr.Error()}
		}
		t := tap()
		if t == nil {
			return report.StepResult{Status: report.StatusError, Observed: "the bus subscription of this step was not armed, so nothing was heard"}
		}
		lostResult := func(li amqpengine.LostInfo) report.StepResult {
			off := li.At.Sub(t.ArmedAt()).Round(time.Millisecond)
			return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
				"the bus subscription (%s) was lost %s after it was armed (%s) - the claims were not judged, because a subscription that "+
					"stopped hearing proves nothing about what was published", spec.Binding, off, scrub(li.Reason))}
		}
		tooWide := func(s amqpengine.Snapshot) (report.StepResult, bool) {
			if s.Seen > seenMax || s.Overflow {
				return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
					"the bus binding (%s) is too wide: it delivered more than the step can judge (%d deliveries seen, limit %d), so a count "+
						"would be a lower bound - narrow `routing_key` (or the stream's)", spec.Binding, s.Seen, seenMax)}, true
			}
			return report.StepResult{}, false
		}

		deadline := now().Add(spec.Wait)
		for {
			if li, lost := t.Lost(); lost {
				return lostResult(li)
			}
			snap := t.Snapshot(where, scenario.BusMessagesKept)
			if sr, wide := tooWide(snap); wide {
				return sr
			}
			if early {
				doc, derr := busDocument(snap, cid, scrub)
				if derr != nil {
					return report.StepResult{Status: report.StatusError, Observed: derr.Error()}
				}
				if miss, _ := mcp.BodyAssertsMissReason(string(doc), boundWant); !miss {
					return busPass(snap, doc, save, enforced())
				}
			}
			if !now().Before(deadline) {
				break
			}
			wait := deadline.Sub(now())
			if wait > tick {
				wait = tick
			}
			sleep(wait)
		}

		// the deadline: health again, then judge ONCE
		if li, lost := t.Lost(); lost {
			return lostResult(li)
		}
		snap := t.Snapshot(where, scenario.BusMessagesKept)
		if sr, wide := tooWide(snap); wide {
			return sr
		}
		doc, derr := busDocument(snap, cid, scrub)
		if derr != nil {
			return report.StepResult{Status: report.StatusError, Observed: derr.Error()}
		}
		miss, nonNumeric := mcp.BodyAssertsMissReason(string(doc), boundWant)
		if !miss {
			return busPass(snap, doc, save, enforced())
		}
		reason := "the counts did not satisfy the scenario's body assertion(s)"
		if nonNumeric {
			reason = "a numeric comparison in the scenario's body assertion(s) found a non-numeric observed value"
		}
		observed := fmt.Sprintf("the bus subscription heard %d delivery(ies), %d carrying this run's id, %d passing `where`, but %s %s",
			snap.Seen, snap.Correlated, snap.Count, reason, chainClaimsNote)
		if snap.Correlated == 0 {
			observed = fmt.Sprintf("the bus subscription heard %d delivery(ies) and none carried this run's correlation id under the declared "+
				"sources (%d without one, %d undecodable), so the claims could not hold: %s %s", snap.Seen, snap.Uncorrelated, snap.Undecodable, reason, chainClaimsNote)
		}
		failed, omitted := failedBodyClaims(string(doc), want, boundWant, vars, scrub)
		st := report.StepResult{Status: "failed", Observed: observed, FailedClaims: failed, FailedClaimsOmitted: omitted}
		st.AssertionsEnforced = enforced()
		st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
		return st
	}

	return Step{
		Name:     name,
		OwnError: true,
		Needs:    needs,
		Run:      run,
	}
}

// busPass is the passed result, with the step's `save` read from the document.
func busPass(snap amqpengine.Snapshot, doc []byte, save map[string]scenario.SaveSpec, enforced []string) report.StepResult {
	st := report.StepResult{Status: "passed", Observed: fmt.Sprintf("the bus subscription heard %d delivery(ies), %d carrying this run's id, %d passing `where`",
		snap.Seen, snap.Correlated, snap.Count)}
	st.AssertionsEnforced = enforced
	st.AssertionsEnforcedCount = len(enforced)
	if len(save) > 0 {
		captured := make(map[string]string, len(save))
		for varName, sp := range save {
			val, fail := saveFromHTTP(doc, varName, sp)
			if fail != "" {
				return report.StepResult{Status: "failed", Observed: fail}
			}
			recordCapture(captured, varName, sp, val)
		}
		st.Captured = captured
	}
	return st
}

// busDocument builds the ONE JSON document the claims are judged against. Every string of a kept message is redacted (the
// credential scrub, then the token redaction the logs step uses, with the run's own id held out) before it is kept, and a
// body that is still longer than scenario.BusMessageMaxBytes is cut (on a rune boundary) into a string.
func busDocument(snap amqpengine.Snapshot, cid string, scrub func(string) string) ([]byte, error) {
	msgs := make([]amqpengine.Message, 0, len(snap.Messages))
	for _, m := range snap.Messages {
		m.Properties, _ = redactBusValue(m.Properties, cid, scrub).(map[string]any)
		if m.Headers != nil {
			m.Headers, _ = redactBusValue(m.Headers, cid, scrub).(map[string]any)
		}
		body := redactBusValue(m.Body, cid, scrub)
		if raw, err := json.Marshal(body); err == nil && len(raw) > scenario.BusMessageMaxBytes {
			body = cutOnRune(string(raw), scenario.BusMessageMaxBytes)
		}
		m.Body = body
		msgs = append(msgs, m)
	}
	doc, err := jsonText(struct {
		Count        int                  `json:"count"`
		Correlated   int                  `json:"correlated"`
		Seen         int                  `json:"seen"`
		Uncorrelated int                  `json:"uncorrelated"`
		Undecodable  int                  `json:"undecodable"`
		Messages     []amqpengine.Message `json:"messages"`
	}{snap.Count, snap.Correlated, snap.Seen, snap.Uncorrelated, snap.Undecodable, msgs})
	if err != nil {
		return nil, fmt.Errorf("the messages cannot be placed in the document: %v", err)
	}
	return doc, nil
}

// redactBusValue redacts every string leaf of a decoded JSON value (keys are left as they are).
func redactBusValue(v any, cid string, scrub func(string) string) any {
	switch t := v.(type) {
	case string:
		return redactLogLine(t, cid, scrub)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = redactBusValue(x, cid, scrub)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactBusValue(x, cid, scrub)
		}
		return out
	}
	return v
}
