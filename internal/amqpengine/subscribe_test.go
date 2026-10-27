package amqpengine

import (
	"bytes"
	"compress/gzip"
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Subscribe against a recording channel (no broker): the order of the calls, what the queue is, when it
// returns, what makes it Lost, and how the collector correlates.

type subLog struct {
	mu       sync.Mutex
	calls    []string
	declare  declareCall
	bind     bindCall
	qos      [3]any
	consume  consumeCall
	channels int
}

type consumeCall struct {
	queue, tag                          string
	autoAck, exclusive, noLocal, noWait bool
}

func (l *subLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *subLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type subChannel struct {
	channel // nil: an unanticipated call panics loudly
	log     *subLog
	deliver chan amqp.Delivery
	// held makes ConsumeWithContext wait until it is closed (consume-ok has not come yet)
	held   chan struct{}
	closed chan *amqp.Error
	cancel chan string
}

func (c *subChannel) NotifyClose(ch chan *amqp.Error) chan *amqp.Error { c.closed = ch; return ch }
func (c *subChannel) NotifyCancel(ch chan string) chan string          { c.cancel = ch; return ch }
func (c *subChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	c.log.add("declare")
	c.log.declare = declareCall{1, name, durable, autoDelete, exclusive, noWait, args}
	return amqp.Queue{Name: name}, nil
}
func (c *subChannel) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	c.log.add("bind")
	c.log.bind = bindCall{1, name, key, exchange}
	return nil
}
func (c *subChannel) Qos(count, size int, global bool) error {
	c.log.add("qos")
	c.log.qos = [3]any{count, size, global}
	return nil
}
func (c *subChannel) ConsumeWithContext(_ context.Context, queue, tag string, autoAck, exclusive, noLocal, noWait bool, _ amqp.Table) (<-chan amqp.Delivery, error) {
	if c.held != nil {
		<-c.held
	}
	c.log.add("consume")
	c.log.consume = consumeCall{queue, tag, autoAck, exclusive, noLocal, noWait}
	return c.deliver, nil
}
func (c *subChannel) Cancel(string, bool) error { c.log.add("cancel"); return nil }
func (c *subChannel) QueueDelete(string, bool, bool, bool) (int, error) {
	c.log.add("queue-delete")
	return 0, nil
}
func (c *subChannel) Close() error { c.log.add("channel-close"); return nil }

type subConn struct {
	log    *subLog
	ch     *subChannel
	closed chan *amqp.Error
}

func (c *subConn) Channel() (channel, error) {
	c.log.mu.Lock()
	c.log.channels++
	c.log.mu.Unlock()
	return c.ch, nil
}
func (c *subConn) Close() error { return nil }
func (c *subConn) abort()       {}
func (c *subConn) NotifyBlocked(chan amqp.Blocking) chan amqp.Blocking {
	return make(chan amqp.Blocking)
}
func (c *subConn) NotifyClose(ch chan *amqp.Error) chan *amqp.Error { c.closed = ch; return ch }

func subEngine(held chan struct{}) (*Engine, *subConn) {
	lg := &subLog{}
	ch := &subChannel{log: lg, deliver: make(chan amqp.Delivery, 64), held: held}
	conn := &subConn{log: lg, ch: ch}
	e := newEngine(conn)
	e.rpcTimeout = 2 * time.Second
	e.closeTimeout = 500 * time.Millisecond
	return e, conn
}

func testOpts() SubscribeOptions {
	return SubscribeOptions{
		CorrelationID: "tr-abc",
		TTLMillis:     120000,
		Envelope: Envelope{Correlation: []CorrelationSource{
			{Property: "correlation_id"}, {Header: "x-correlation-id"}, {Body: "correlation_id"},
		}, ListPayload: true},
	}
}

// T9: the queue is exclusive and auto-delete with x-expires, declared, then bound, then Qos, then consumed on ONE channel -
// and Subscribe returns only after consume-ok.
func TestSubscribe_DeclaresExclusiveAutoDeleteThenBindsThenQosThenConsumes(t *testing.T) {
	e, conn := subEngine(nil)
	sub, out := e.Subscribe("argus-tap-tr-abc-1", "orders.incoming", "orders.created", testOpts())
	if !out.OK || sub == nil {
		t.Fatalf("Subscribe = %+v, %v", out, sub)
	}
	defer sub.Close()
	lg := conn.log
	if want := []string{"declare", "bind", "qos", "consume"}; !reflect.DeepEqual(lg.snapshot(), want) {
		t.Fatalf("calls = %v, want %v", lg.snapshot(), want)
	}
	if lg.channels != 1 {
		t.Errorf("channels opened = %d, want exactly 1", lg.channels)
	}
	d := lg.declare
	if d.name != "argus-tap-tr-abc-1" || d.durable || !d.autoDelete || !d.exclusive || d.noWait {
		t.Errorf("declare = %+v, want durable=false autoDelete=true exclusive=true", d)
	}
	if want := (amqp.Table{"x-expires": int32(120000), "x-message-ttl": int32(120000)}); !reflect.DeepEqual(d.args, want) {
		t.Errorf("declare args = %v, want %v", d.args, want)
	}
	if b := lg.bind; b.name != "argus-tap-tr-abc-1" || b.exchange != "orders.incoming" || b.key != "orders.created" {
		t.Errorf("bind = %+v", b)
	}
	if lg.qos[0] != 256 {
		t.Errorf("qos = %v, want prefetch 256", lg.qos)
	}
	if c := lg.consume; c.queue != "argus-tap-tr-abc-1" || !c.autoAck || !c.exclusive {
		t.Errorf("consume = %+v, want autoAck and exclusive", c)
	}
}

func TestSubscribe_ReturnsOnlyAfterConsumeOK(t *testing.T) {
	held := make(chan struct{})
	e, _ := subEngine(held)
	returned := make(chan *Subscription, 1)
	go func() {
		s, _ := e.Subscribe("q", "x", "a.b", testOpts())
		returned <- s
	}()
	select {
	case <-returned:
		t.Fatal("Subscribe returned before the broker confirmed the consumer")
	case <-time.After(150 * time.Millisecond):
	}
	close(held)
	select {
	case s := <-returned:
		if s == nil {
			t.Fatal("Subscribe failed after consume-ok")
		}
		s.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after consume-ok")
	}
}

// T10: each way the subscription can stop hearing sets Lost() with a reason and a time.
func TestSubscribe_LostIsRecordedWithReasonAndTime(t *testing.T) {
	cases := []struct {
		name string
		stop func(c *subConn)
		want string
	}{
		{"the deliveries channel closes", func(c *subConn) { close(c.ch.deliver) }, "delivery channel closed"},
		{"the broker cancels the consumer", func(c *subConn) { c.ch.cancel <- "tag" }, "cancelled the consumer"},
		{"the channel is closed by the broker", func(c *subConn) { c.ch.closed <- &amqp.Error{Code: 406, Reason: "PRECONDITION_FAILED"} }, "channel closed by the broker (406: PRECONDITION_FAILED)"},
		{"the connection is closed", func(c *subConn) { c.closed <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED"} }, "connection closed by the broker (320: CONNECTION_FORCED)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, conn := subEngine(nil)
			sub, out := e.Subscribe("q", "x", "a.b", testOpts())
			if !out.OK {
				t.Fatal(out)
			}
			defer sub.Close()
			if _, lost := sub.Lost(); lost {
				t.Fatal("a healthy subscription reported lost")
			}
			tc.stop(conn)
			waitFor(t, func() bool { _, lost := sub.Lost(); return lost })
			li, _ := sub.Lost()
			if !strings.Contains(li.Reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q", li.Reason, tc.want)
			}
			if li.At.IsZero() || li.At.Before(sub.ArmedAt()) {
				t.Errorf("lost at %v, armed at %v: want a time at or after arming", li.At, sub.ArmedAt())
			}
		})
	}
}

// A subscription the caller closed is not "lost": closing it ends the deliveries on purpose.
func TestSubscribe_ACloseOfOurOwnIsNotLost(t *testing.T) {
	e, conn := subEngine(nil)
	sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
	sub.Close()
	sub.Close() // idempotent
	close(conn.ch.deliver)
	time.Sleep(50 * time.Millisecond)
	if li, lost := sub.Lost(); lost {
		t.Fatalf("our own Close read as lost: %+v", li)
	}
	calls := conn.log.snapshot()
	if got := strings.Join(calls[4:], ","); got != "cancel,queue-delete,channel-close" {
		t.Errorf("teardown calls = %q, want cancel, queue-delete, channel-close exactly once", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle waits until the collector has seen n deliveries.
func settle(t *testing.T, sub *Subscription, n int) {
	t.Helper()
	waitFor(t, func() bool { return sub.Snapshot(nil, 20).Seen >= n })
}

// T15: the collector's correlation.
func TestSubscribe_Correlation(t *testing.T) {
	json := func(s string) amqp.Delivery { return amqp.Delivery{Body: []byte(s), ContentType: "application/json"} }
	t.Run("matches through the AMQP property", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		conn.ch.deliver <- amqp.Delivery{CorrelationId: "tr-abc", Body: []byte(`{"level":"info"}`)}
		settle(t, sub, 1)
		s := sub.Snapshot(nil, 20)
		if s.Correlated != 1 || s.Count != 1 || s.Messages[0].MatchedBy != "property:correlation_id" {
			t.Fatalf("snapshot = %+v", s)
		}
	})
	t.Run("matches a header in any letter case", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		for _, name := range []string{"X-Correlation-ID", "x-correlation-id", "X-CORRELATION-ID"} {
			d := json(`{"a":1}`)
			d.Headers = amqp.Table{name: "tr-abc"}
			conn.ch.deliver <- d
		}
		settle(t, sub, 3)
		if s := sub.Snapshot(nil, 20); s.Correlated != 3 || s.Messages[0].MatchedBy != "header:x-correlation-id" {
			t.Fatalf("snapshot = %+v", s)
		}
	})
	t.Run("matches a body field inside a GoKit list and judges where on that element", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		conn.ch.deliver <- json(`[{"correlation_id":"other","level":"error"},{"correlation_id":"tr-abc","level":"info"}]`)
		settle(t, sub, 1)
		if s := sub.Snapshot(nil, 20); s.Correlated != 1 || s.Count != 1 {
			t.Fatalf("snapshot = %+v", s)
		}
		// the error-level record belongs to ANOTHER run: where level=error must not count it
		if s := sub.Snapshot(map[string][]string{"level": {"error"}}, 20); s.Correlated != 1 || s.Count != 0 {
			t.Fatalf("where on the element that carries the id: %+v", s)
		}
		s := sub.Snapshot(map[string][]string{"level": {"warn", "info"}}, 20)
		if s.Count != 1 || s.Messages[0].Body.(map[string]any)["level"] != "info" {
			t.Fatalf("any-of where: %+v", s)
		}
	})
	t.Run("matches the per-call id <cid>.<hex> and not a longer lookalike", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		conn.ch.deliver <- json(`{"correlation_id":"tr-abc.ab12cd34"}`) // the prefix form: correlated
		conn.ch.deliver <- json(`{"correlation_id":"tr-abcX"}`)         // NOT this run's id
		conn.ch.deliver <- json(`{"correlation_id":"xtr-abc"}`)         // NOT this run's id
		conn.ch.deliver <- json(`{"note":"see tr-abc for details"}`)    // a substring in an unrelated field
		conn.ch.deliver <- amqp.Delivery{CorrelationId: "tr-abcdef", Body: []byte(`{}`)}
		settle(t, sub, 5)
		s := sub.Snapshot(nil, 20)
		if s.Correlated != 1 || s.Uncorrelated != 4 {
			t.Fatalf("correlated=%d uncorrelated=%d, want 1 and 4 (equality or <cid>. prefix only, never a substring): %+v", s.Correlated, s.Uncorrelated, s)
		}
	})
	t.Run("an uncorrelated message never counts", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		conn.ch.deliver <- json(`{"level":"error"}`)
		conn.ch.deliver <- json(`{"correlation_id":"someone-else","level":"error"}`)
		settle(t, sub, 2)
		s := sub.Snapshot(map[string][]string{"level": {"error"}}, 20)
		if s.Seen != 2 || s.Uncorrelated != 2 || s.Correlated != 0 || s.Count != 0 {
			t.Fatalf("snapshot = %+v", s)
		}
	})
	t.Run("an undecodable body still matches through a header", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		conn.ch.deliver <- amqp.Delivery{Body: []byte("not json at all"), Headers: amqp.Table{"x-correlation-id": "tr-abc"}}
		conn.ch.deliver <- amqp.Delivery{Body: []byte("{broken"), Headers: amqp.Table{"x-correlation-id": "other"}}
		settle(t, sub, 2)
		s := sub.Snapshot(nil, 20)
		if s.Undecodable != 2 || s.Correlated != 1 || s.Count != 1 || s.Messages[0].MatchedBy != "header:x-correlation-id" {
			t.Fatalf("snapshot = %+v", s)
		}
		// it carries no record, so it cannot pass a where
		if s := sub.Snapshot(map[string][]string{"level": {"error"}}, 20); s.Count != 0 || s.Correlated != 1 {
			t.Fatalf("a message with no decoded record passed a where: %+v", s)
		}
	})
	t.Run("a gzip body is gunzipped; one over the cap is undecodable", func(t *testing.T) {
		e, conn := subEngine(nil)
		o := testOpts()
		o.DecodedMax = 64
		sub, _ := e.Subscribe("q", "x", "a.b", o)
		defer sub.Close()
		gz := func(s string) []byte {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			_, _ = w.Write([]byte(s))
			_ = w.Close()
			return b.Bytes()
		}
		conn.ch.deliver <- amqp.Delivery{ContentEncoding: "gzip", Body: gz(`{"correlation_id":"tr-abc"}`)}
		conn.ch.deliver <- amqp.Delivery{ContentEncoding: "gzip", Body: gz(`{"correlation_id":"tr-abc","pad":"` + strings.Repeat("x", 200) + `"}`)}
		settle(t, sub, 2)
		if s := sub.Snapshot(nil, 20); s.Correlated != 1 || s.Undecodable != 1 {
			t.Fatalf("snapshot = %+v", s)
		}
	})
	t.Run("only the first N matches are kept; count is exact", func(t *testing.T) {
		e, conn := subEngine(nil)
		sub, _ := e.Subscribe("q", "x", "a.b", testOpts())
		defer sub.Close()
		for i := 0; i < 30; i++ {
			conn.ch.deliver <- amqp.Delivery{CorrelationId: "tr-abc", Body: []byte(`{}`)}
		}
		settle(t, sub, 30)
		if s := sub.Snapshot(nil, 20); s.Count != 30 || len(s.Messages) != 20 {
			t.Fatalf("count=%d kept=%d, want 30 and 20", s.Count, len(s.Messages))
		}
	})
	t.Run("overflow is flagged, never a silent lower bound", func(t *testing.T) {
		e, conn := subEngine(nil)
		o := testOpts()
		o.CorrelatedMax = 3
		sub, _ := e.Subscribe("q", "x", "a.b", o)
		defer sub.Close()
		for i := 0; i < 5; i++ {
			conn.ch.deliver <- amqp.Delivery{CorrelationId: "tr-abc", Body: []byte(`{}`)}
		}
		settle(t, sub, 5)
		if s := sub.Snapshot(nil, 20); !s.Overflow || s.Correlated != 3 {
			t.Fatalf("snapshot = %+v, want Overflow and 3 kept", s)
		}
	})
}
