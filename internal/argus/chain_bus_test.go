package argus

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The bus step through the WHOLE executor path (claims, preflight, arming, BusStep, chain.Run, teardown)
// with a fake broker behind the real chain.BusHub. No RabbitMQ, no cluster.

const busTestCID = "tr-20261019T101530123-CHN-RT-abcdef12"
const busURLEnv = "ARGUS_BUS_TEST_URL"
const busSecret = "S3cretBusPw"

// fakeBus is the broker: it records, in ONE ordered log, what the executor and the system under test do, and routes a
// publish to the taps armed so far (a message published before a subscription exists is lost, as on a real broker).
type fakeBus struct {
	mu        sync.Mutex
	events    []string
	taps      []*busFakeTap
	dials     int
	connsOpen int
	connsDone int
	dialErr   error
	subOut    amqpengine.Outcome // a refusal; zero value = accept
	subFailOn int                // the n-th Subscribe is refused (0 = never)
	nSub      int
	panicOn   bool
	opts      []amqpengine.SubscribeOptions
	binds     [][3]string
}

func (b *fakeBus) log(e string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
}

func (b *fakeBus) eventLog() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

// publish is what the system under test does: it reaches every tap that is armed NOW.
func (b *fakeBus) publish(body map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.taps {
		t.add(body)
	}
}

func (b *fakeBus) loseAll(why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.taps {
		t.lose(why)
	}
}

func (b *fakeBus) dial(env string) (chain.BusConn, error) {
	b.mu.Lock()
	b.dials++
	b.events = append(b.events, "dial "+env)
	err := b.dialErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.connsOpen++
	b.mu.Unlock()
	return &fakeBusConn{b: b}, nil
}

type fakeBusConn struct{ b *fakeBus }

func (c *fakeBusConn) Subscribe(queue, exchange, rk string, o amqpengine.SubscribeOptions) (chain.BusTap, amqpengine.Outcome) {
	b := c.b
	b.mu.Lock()
	b.nSub++
	n := b.nSub
	b.events = append(b.events, "subscribe "+queue)
	b.opts = append(b.opts, o)
	b.binds = append(b.binds, [3]string{queue, exchange, rk})
	refuse := b.subFailOn == n
	out := b.subOut
	b.mu.Unlock()
	if refuse {
		return nil, out
	}
	t := &busFakeTap{b: b, armed: time.Now(), queue: queue}
	b.mu.Lock()
	b.taps = append(b.taps, t)
	b.mu.Unlock()
	return t, amqpengine.Outcome{OK: true}
}

func (c *fakeBusConn) Close() error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	c.b.connsDone++
	c.b.events = append(c.b.events, "close connection")
	return nil
}

type busFakeTap struct {
	b      *fakeBus
	queue  string
	armed  time.Time
	mu     sync.Mutex
	msgs   []map[string]any
	lostBy string
	closes int
}

func (t *busFakeTap) add(m map[string]any) { t.mu.Lock(); t.msgs = append(t.msgs, m); t.mu.Unlock() }
func (t *busFakeTap) lose(why string)      { t.mu.Lock(); t.lostBy = why; t.mu.Unlock() }
func (t *busFakeTap) Snapshot(where map[string][]string, keep int) amqpengine.Snapshot {
	if t.b.panicOn {
		panic("boom in a bus step")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := amqpengine.Snapshot{}
	for _, m := range t.msgs {
		s.Correlated++
		pass := true
		for p, want := range where {
			got, _ := m[p].(string)
			hit := false
			for _, w := range want {
				hit = hit || got == w
			}
			pass = pass && hit
		}
		if pass {
			s.Count++
			if len(s.Messages) < keep {
				s.Messages = append(s.Messages, amqpengine.Message{Exchange: "x", RoutingKey: "a.b", MatchedBy: "body:correlation_id", Properties: map[string]any{}, Body: m})
			}
		}
	}
	s.Seen = s.Correlated
	return s
}
func (t *busFakeTap) Lost() (amqpengine.LostInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lostBy == "" {
		return amqpengine.LostInfo{}, false
	}
	return amqpengine.LostInfo{Reason: t.lostBy, At: time.Now()}, true
}
func (t *busFakeTap) ArmedAt() time.Time { return t.armed }
func (t *busFakeTap) Close() {
	t.mu.Lock()
	t.closes++
	t.mu.Unlock()
	t.b.log("close tap " + t.queue)
}

// withFakeBus swaps dialBus for the test's lifetime.
func withFakeBus(t *testing.T) *fakeBus {
	t.Helper()
	b := &fakeBus{}
	old := dialBus
	dialBus = b.dial
	t.Cleanup(func() { dialBus = old })
	t.Setenv(busURLEnv, "amqp://svc:"+busSecret+"@broker.internal:5672/")
	return b
}

func busTestConfig() *config.Config {
	c := &config.Config{}
	c.Targets.MessageBroker = &config.MQTarget{
		URL: "amqp://svc:" + busSecret + "@broker.internal:5672/", URLEnvName: busURLEnv,
		Envelope: &config.BusEnvelope{Correlation: []config.BusCorrelationSource{{Property: "correlation_id"}, {Body: "correlation_id"}}, ListPayload: true},
		Streams:  map[string]config.BusStream{"logs": {Exchange: "tr.logs", RoutingKey: "logs.orders"}},
	}
	return c
}

// busChain: an http step that counts its requests (and runs onHit), then the bus step json.
func busChain(t *testing.T, onHit func(), busStep, extraExpect string) (*scenario.Scenario, *int32) {
	t.Helper()
	var hits int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if onHit != nil {
			onHit()
		}
		_, _ = w.Write([]byte(`{"id":"o-1"}`))
	}))
	t.Cleanup(api.Close)
	trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"` + api.URL + `/o","save":{"oid":"id"}},` + busStep + `]}`
	return scenario.Parse(chainRunMD("", trig, "### Runnable\n- step mk: status=200\n"+extraExpect)), &hits
}

func runBusChain(c *config.Config, s *scenario.Scenario) report.ScenarioResult {
	return runChainScenarioWithFaults(c, s, busTestCID, "testkit/ui", nil, nil, nil, faultEnv{})
}

const busGoodStep = `{"type":"bus","name":"b","exchange":"orders.incoming","routing_key":"orders.created","wait":"1s"}`
const busGoodExpect = "- step b: body has count >= 1\n"

func indexOf(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// T17 - ORDERING. The subscription is armed BEFORE the first step fires: a message the first step provokes is heard, and the
// log shows `subscribe` before the http request.
func TestBusChain_ArmsBeforeTheTriggerFires(t *testing.T) {
	bus := withFakeBus(t)
	s, hits := busChain(t, func() {
		bus.log("http request")
		bus.publish(map[string]any{"correlation_id": busTestCID, "level": "info"})
	}, busGoodStep, busGoodExpect)
	res := runBusChain(busTestConfig(), s)
	if res.Status != "passed" {
		t.Fatalf("want passed (the message the trigger provoked must be heard): %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	ev := bus.eventLog()
	sub, req := indexOf(ev, "subscribe "), indexOf(ev, "http request")
	if sub < 0 || req < 0 || sub > req {
		t.Fatalf("the subscription must be armed before the trigger fires: %v", ev)
	}
	if want := "subscribe argus-tap-" + busTestCID + "-1"; ev[sub] != want {
		t.Fatalf("queue name = %q, want %q (index of the bus step in the chain)", ev[sub], want)
	}
	if *hits != 1 {
		t.Fatalf("hits = %d", *hits)
	}
	// what was armed: the binding, the run's id and the envelope of the target
	o := bus.opts[0]
	if bus.binds[0][1] != "orders.incoming" || bus.binds[0][2] != "orders.created" || o.CorrelationID != busTestCID ||
		!o.Envelope.ListPayload || len(o.Envelope.Correlation) != 2 || o.Envelope.Correlation[0].Property != "correlation_id" || o.Envelope.Correlation[1].Body != "correlation_id" {
		t.Fatalf("armed with %+v %+v", bus.binds[0], o)
	}
	if o.TTLMillis != scenario.ListenQueueTTLMillis {
		t.Fatalf("ttl = %d", o.TTLMillis)
	}
}

func TestBusChain_AStreamIsResolvedFromTheTarget(t *testing.T) {
	bus := withFakeBus(t)
	s, _ := busChain(t, func() { bus.publish(map[string]any{"level": "error"}) },
		`{"type":"bus","name":"b","stream":"logs","where":{"level":["error"]},"wait":"1s"}`, busGoodExpect)
	res := runBusChain(busTestConfig(), s)
	if res.Status != "passed" || bus.binds[0][1] != "tr.logs" || bus.binds[0][2] != "logs.orders" {
		t.Fatalf("%q %+v binds=%v", res.Status, res.Failure, bus.binds)
	}
}

// T17b - an arming failure is `error`, nothing was fired, and what was already armed is torn down.
func TestBusChain_ArmingFailuresAreErrorsAndNothingIsFired(t *testing.T) {
	cases := []struct {
		name  string
		setup func(b *fakeBus)
		want  string
	}{
		{"the dial fails", func(b *fakeBus) {
			b.dialErr = errors.New("dial: amqp://svc:" + busSecret + "@broker.internal:5672/: connection refused")
		}, "connect (url from $" + busURLEnv + ") failed"},
		{"the grant is missing (403)", func(b *fakeBus) {
			b.subFailOn, b.subOut = 1, amqpengine.Outcome{Code: 403, Text: "ACCESS_REFUSED - access to queue 'argus-tap-x' in vhost '/' refused for user 'svc'"}
		}, "the broker refused the subscription (403)"},
		{"the exchange does not exist (404)", func(b *fakeBus) {
			b.subFailOn, b.subOut = 1, amqpengine.Outcome{Code: 404, Text: "NOT_FOUND - no exchange 'orders.incoming' in vhost '/'"}
		}, "the broker refused the subscription (404)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bus := withFakeBus(t)
			c.setup(bus)
			s, hits := busChain(t, nil, busGoodStep, busGoodExpect)
			res := runBusChain(busTestConfig(), s)
			if res.Status != report.StatusError || res.Failure == nil || !strings.Contains(res.Failure.Observed, c.want) || !strings.Contains(res.Failure.Observed, "- arming") {
				t.Fatalf("want error naming %q, got %q %+v", c.want, res.Status, res.Failure)
			}
			if *hits != 0 || len(res.Steps) != 0 {
				t.Fatalf("nothing may be fired when arming fails: %d request(s), steps %+v", *hits, res.Steps)
			}
			if b, _ := json.Marshal(res); strings.Contains(string(b), busSecret) || strings.Contains(string(b), "broker.internal") {
				t.Fatalf("the broker URL leaked into the result: %s", b)
			}
		})
	}
	t.Run("the second subscription is refused: the first is torn down", func(t *testing.T) {
		bus := withFakeBus(t)
		bus.subFailOn, bus.subOut = 2, amqpengine.Outcome{Code: 403, Text: "ACCESS_REFUSED"}
		trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"http://sut.invalid/o"},` + busGoodStep + `,` +
			`{"type":"bus","name":"b2","exchange":"orders.incoming","routing_key":"orders.updated","wait":"1s"}]}`
		s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step mk: status=200\n- step b: body has count >= 1\n- step b2: body has count >= 1\n"))
		res := runBusChain(busTestConfig(), s)
		if res.Status != report.StatusError {
			t.Fatalf("%q %+v", res.Status, res.Failure)
		}
		if bus.taps[0].closes != 1 || bus.connsDone != 1 {
			t.Fatalf("the armed subscription must be closed once: taps closed %d, connections closed %d", bus.taps[0].closes, bus.connsDone)
		}
	})
}

// T18 - TEARDOWN ON EVERY PATH: passed, failed, errored and a panic each close the subscription and the connection exactly
// once; a chain refused at preflight arms (and dials) nothing.
func TestBusChain_TeardownOnEveryPath(t *testing.T) {
	check := func(t *testing.T, bus *fakeBus) {
		t.Helper()
		if len(bus.taps) != 1 || bus.taps[0].closes != 1 || bus.connsOpen != 1 || bus.connsDone != 1 {
			t.Fatalf("taps=%d closes=%v connections opened/closed = %d/%d, want one each, closed once", len(bus.taps), bus.taps, bus.connsOpen, bus.connsDone)
		}
		ev := bus.eventLog()
		if indexOf(ev, "close tap") > indexOf(ev, "close connection") {
			t.Fatalf("the subscription is closed before its connection: %v", ev)
		}
	}
	t.Run("passed", func(t *testing.T) {
		bus := withFakeBus(t)
		s, _ := busChain(t, func() { bus.publish(map[string]any{"a": "b"}) }, busGoodStep, busGoodExpect)
		if res := runBusChain(busTestConfig(), s); res.Status != "passed" {
			t.Fatalf("%q %+v", res.Status, res.Failure)
		}
		check(t, bus)
	})
	t.Run("failed", func(t *testing.T) {
		bus := withFakeBus(t)
		s, _ := busChain(t, nil, busGoodStep, busGoodExpect)
		if res := runBusChain(busTestConfig(), s); res.Status != "failed" {
			t.Fatalf("%q %+v", res.Status, res.Failure)
		}
		check(t, bus)
	})
	t.Run("errored (the subscription is lost)", func(t *testing.T) {
		bus := withFakeBus(t)
		s, _ := busChain(t, func() { bus.loseAll("the connection closed by the broker (320: CONNECTION_FORCED)") }, busGoodStep, busGoodExpect)
		res := runBusChain(busTestConfig(), s)
		if res.Status != report.StatusError || !strings.Contains(res.Steps[1].Observed, "was lost") {
			t.Fatalf("%q %+v", res.Status, res.Steps)
		}
		check(t, bus)
	})
	t.Run("a panic in the run", func(t *testing.T) {
		bus := withFakeBus(t)
		bus.panicOn = true
		s, _ := busChain(t, nil, busGoodStep, busGoodExpect)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("setup: the step was expected to panic")
				}
			}()
			runBusChain(busTestConfig(), s)
		}()
		check(t, bus)
	})
	t.Run("a refused chain arms nothing", func(t *testing.T) {
		bus := withFakeBus(t)
		s, hits := busChain(t, nil, `{"type":"bus","name":"b","exchange":"x","routing_key":"#"}`, busGoodExpect)
		res := runBusChain(busTestConfig(), s)
		if res.Status != "failed" || bus.dials != 0 || len(bus.taps) != 0 || *hits != 0 {
			t.Fatalf("%q dials=%d taps=%d hits=%d", res.Status, bus.dials, len(bus.taps), *hits)
		}
	})
}

// T3 (the executor half) - THE TWO DOORS SAY THE SAME THING. Every shape and claim refusal comes from the one function
// the validator asks, as `failed` with the preflight lead; the broker is never dialled and the first step never fires.
func TestBusPreflight_RefusesWhatValidateRefuses(t *testing.T) {
	cases := []struct {
		name   string
		step   string
		expect string
		want   string
	}{
		{"bare #", `{"type":"bus","name":"b","exchange":"x","routing_key":"#"}`, busGoodExpect, "binds the whole exchange"},
		{"the default exchange", `{"type":"bus","name":"b","exchange":"","routing_key":"a.b"}`, busGoodExpect, "the default exchange cannot be subscribed to"},
		{"a placeholder in the binding", `{"type":"bus","name":"b","exchange":"x","routing_key":"${cid}"}`, busGoodExpect, "`routing_key` carries `${…}`"},
		{"no binding", `{"type":"bus","name":"b"}`, busGoodExpect, "this bus step declares no binding"},
		{"stream and exchange", `{"type":"bus","name":"b","stream":"logs","exchange":"x","routing_key":"a.b"}`, busGoodExpect, "both `stream` and `exchange`/`routing_key`"},
		{"wait 61s", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","wait":"61s"}`, busGoodExpect, "outside the 1s..1m0s bounds of a bus step"},
		{"wait 0", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","wait":"0s"}`, busGoodExpect, "outside the 1s..1m0s bounds of a bus step"},
		{"poll", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","poll":{"timeout":"5s","interval":"1s"}}`, busGoodExpect, "may not carry `poll` or `always`"},
		{"always", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","always":true}`, busGoodExpect, "may not carry `poll` or `always`"},
		{"a stray key", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","url":"http://evil.invalid"}`, busGoodExpect, `this bus step carries the field "url"`},
		{"where foreign placeholder", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","where":{"a":"${TOKEN}"}}`, busGoodExpect, "`where` value of \"a\" carries `${TOKEN}`"},
		{"where not an object", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","where":"level"}`, busGoodExpect, "`where` must be a JSON object"},
		{"an absence with no anchor", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b"}`, "- step b: body has count == 0\n", "no anchor: add `body has correlated >= 1`"},
		{"a status claim", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b"}`, "- step b: status=200\n", "is a status claim - a bus step has no HTTP status"},
		{"a claim over every message", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b"}`, "- step b: body every messages has body.level equals info\n", "is a claim over every message"},
		{"a negative claim over the list", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b"}`, "- step b: body not contains error\n", "is a negative claim over the message list"},
		{"no claim", `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b"}`, "", "needs at least one `body …` claim"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bus := withFakeBus(t)
			s, hits := busChain(t, nil, c.step, c.expect)
			res := runBusChain(busTestConfig(), s)
			if res.Status != "failed" || res.Failure == nil || !strings.HasSuffix(res.Failure.Observed, "— preflight") || !strings.Contains(res.Failure.Observed, c.want) {
				t.Fatalf("want a preflight failure containing %q, got %q %+v", c.want, res.Status, res.Failure)
			}
			if *hits != 0 || bus.dials != 0 || len(res.Steps) != 0 {
				t.Fatalf("a refused chain fires and dials nothing: hits=%d dials=%d steps=%+v", *hits, bus.dials, res.Steps)
			}
			// the authoring door says the SAME sentence (no claim at all is the executor's own backstop)
			if c.expect != "" {
				_, errs := scenario.Validate(chainRunMD("", `{"steps":[{"type":"http","name":"mk","method":"GET","url":"http://x.invalid/o","save":{"oid":"id"}},`+c.step+`]}`,
					"### Runnable\n- step mk: status=200\n"+c.expect))
				found := false
				for _, e := range errs {
					found = found || strings.Contains(e.Message, c.want)
				}
				if !found {
					t.Fatalf("the authoring validator must refuse it with the same sentence %q; got %v", c.want, errs)
				}
			}
		})
	}
	t.Run("a bus step at index 0", func(t *testing.T) {
		bus := withFakeBus(t)
		s := scenario.Parse(chainRunMD("", `{"steps":[`+busGoodStep+`]}`, "### Runnable\n"+busGoodExpect))
		res := runBusChain(busTestConfig(), s)
		if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "this bus step is the chain's first step") || bus.dials != 0 {
			t.Fatalf("%q %+v dials=%d", res.Status, res.Failure, bus.dials)
		}
	})
}

// The environment refusals are `error` (the harness is not set up for it), not `failed`; nothing is dialled and nothing fires.
func TestBusPreflight_EnvironmentRefusalsAreErrors(t *testing.T) {
	cases := []struct {
		name  string
		step  string
		tweak func(c *config.Config)
		unset bool
		want  string
	}{
		{"an unknown target", `{"type":"bus","name":"b","target":"nope","exchange":"x","routing_key":"a.b"}`, nil, false, `unknown message_broker target "nope"`},
		{"no broker declared", busGoodStep, func(c *config.Config) { c.Targets.MessageBroker = nil }, false, "declares no targets.message_broker"},
		{"no envelope.correlation: there is no default", busGoodStep, func(c *config.Config) { c.Targets.MessageBroker.Envelope = nil }, false, "declares no envelope.correlation"},
		{"an empty correlation list", busGoodStep, func(c *config.Config) { c.Targets.MessageBroker.Envelope.Correlation = nil }, false, "declares no envelope.correlation"},
		{"the url is not one whole ${NAME}", busGoodStep, func(c *config.Config) { c.Targets.MessageBroker.URLEnvName = "" }, false, "url must be one whole ${NAME} reference"},
		{"the NAME is unset in the executor", busGoodStep, nil, true, "refers to the environment variable " + busURLEnv + ", which is not set"},
		{"an unknown stream", `{"type":"bus","name":"b","stream":"nope"}`, nil, false, `unknown stream "nope" - targets.message_broker declares: logs`},
		{"a declared vhost that is not the url's", busGoodStep, func(c *config.Config) { c.Targets.MessageBroker.Vhost = "acme-orders" }, false, `vhost is "acme-orders" but the URL names another vhost ("/")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bus := withFakeBus(t)
			if c.unset {
				t.Setenv(busURLEnv, "")
			}
			cfg := busTestConfig()
			if c.tweak != nil {
				c.tweak(cfg)
			}
			s, hits := busChain(t, nil, c.step, busGoodExpect)
			res := runBusChain(cfg, s)
			if res.Status != report.StatusError || res.Failure == nil || !strings.Contains(res.Failure.Observed, c.want) || !strings.Contains(res.Failure.Observed, "nothing was dialled and nothing was sent") {
				t.Fatalf("want error containing %q, got %q %+v", c.want, res.Status, res.Failure)
			}
			if *hits != 0 || bus.dials != 0 {
				t.Fatalf("hits=%d dials=%d", *hits, bus.dials)
			}
			if b, _ := json.Marshal(res); strings.Contains(string(b), busSecret) {
				t.Fatalf("a secret leaked: %s", b)
			}
		})
	}
}

// A vhost that IS the URL's passes the check.
func TestBusPreflight_ADeclaredVhostThatMatchesIsAccepted(t *testing.T) {
	bus := withFakeBus(t)
	cfg := busTestConfig()
	cfg.Targets.MessageBroker.URL = "amqp://svc:" + busSecret + "@broker.internal:5672/acme-orders"
	cfg.Targets.MessageBroker.Vhost = "acme-orders"
	s, _ := busChain(t, func() { bus.publish(map[string]any{"a": "b"}) }, busGoodStep, busGoodExpect)
	if res := runBusChain(cfg, s); res.Status != "passed" {
		t.Fatalf("%q %+v", res.Status, res.Failure)
	}
}

// A named target is selected with `target`.
func TestBusChain_ANamedTargetIsSelected(t *testing.T) {
	bus := withFakeBus(t)
	t.Setenv("ARGUS_BUS_OTHER_URL", "amqp://u:p@other.internal/")
	cfg := busTestConfig()
	cfg.Targets.MessageBrokerTargets = map[string]*config.MQTarget{"sut-bus": {
		URL: "amqp://u:p@other.internal/", URLEnvName: "ARGUS_BUS_OTHER_URL",
		Envelope: &config.BusEnvelope{Correlation: []config.BusCorrelationSource{{Header: "x-correlation-id"}}},
	}}
	s, _ := busChain(t, func() { bus.publish(map[string]any{"a": "b"}) },
		`{"type":"bus","name":"b","target":"sut-bus","exchange":"x","routing_key":"a.b","wait":"1s"}`, busGoodExpect)
	if res := runBusChain(cfg, s); res.Status != "passed" {
		t.Fatalf("%q %+v", res.Status, res.Failure)
	}
	if ev := bus.eventLog(); ev[0] != "dial ARGUS_BUS_OTHER_URL" {
		t.Fatalf("the named target's variable is dialled: %v", ev)
	}
	if bus.opts[0].Envelope.Correlation[0].Header != "x-correlation-id" {
		t.Fatalf("the named target's envelope is used: %+v", bus.opts[0].Envelope)
	}
}

// `where` carries the run's own id and a saved value, both filled in; an environment value never is.
func TestBusChain_WhereIsFilledFromTheRunAndTheSavedValues(t *testing.T) {
	bus := withFakeBus(t)
	s, _ := busChain(t, func() {
		bus.publish(map[string]any{"order_id": "o-1", "trace": busTestCID})
		bus.publish(map[string]any{"order_id": "o-2", "trace": busTestCID})
	}, `{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","where":{"order_id":"${saved.oid}","trace":"${cid}"},"wait":"1s"}`,
		"- step b: body has count == 1\n- step b: body has correlated >= 1\n")
	if res := runBusChain(busTestConfig(), s); res.Status != "passed" {
		t.Fatalf("%q %+v %+v", res.Status, res.Failure, res.Steps)
	}
}

// The wait is part of the whole-chain backstop; a chain with a bus step is never re-fired after a rate limit.
func TestBusChain_BackstopRetryAndTheNote(t *testing.T) {
	steps, _ := scenario.ParseChainSteps(`{"steps":[{"type":"http","name":"a","method":"GET","url":"http://x"},{"type":"bus","name":"b","exchange":"x","routing_key":"a.b","wait":"30s"},{"type":"bus","name":"c","exchange":"x","routing_key":"a.c"}]}`)
	polls := stepPollTimeouts(steps)
	if polls[0] != 0 || polls[1] != 30*time.Second || polls[2] != scenario.BusWaitDefault {
		t.Fatalf("stepPollTimeouts = %v, want the bus waits (30s and the 10s default)", polls)
	}
	if got := chainBackstop(60*time.Second, polls); got != 3*60*time.Second+40*time.Second {
		t.Fatalf("backstop = %s, want TIMEOUT x steps + the waits", got)
	}
	s, _ := busChain(t, nil, busGoodStep, busGoodExpect)
	if mayRefire(s) {
		t.Fatal("a chain with a bus step must not be re-fired after a rate limit")
	}
	if !chainHasBusStep(s) {
		t.Fatal("chainHasBusStep")
	}
	res := report.ScenarioResult{RateLimited: true, Failure: &report.Failure{Observed: "step 'mk' not measured"}}
	noteBusNotRetried(&res)
	if !strings.Contains(res.Failure.Observed, "this chain has a bus step, so it was not re-fired after the rate limit") {
		t.Fatalf("Observed = %q", res.Failure.Observed)
	}
	res2 := report.ScenarioResult{RateLimited: true}
	noteBusNotRetried(&res2)
	if res2.Failure == nil || !strings.Contains(res2.Failure.Observed, "not re-fired") {
		t.Fatalf("%+v", res2.Failure)
	}
	plain := scenario.Parse(chainRunMD("", `{"steps":[{"type":"http","name":"mk","method":"GET","url":"http://x"}]}`, "### Runnable\n- step mk: status=200\n"))
	if !mayRefire(plain) || chainHasBusStep(plain) {
		t.Fatal("a chain without a bus step keeps today's behaviour")
	}
	_ = fmt.Sprint
}
