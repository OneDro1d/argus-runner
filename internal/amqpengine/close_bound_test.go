package amqpengine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// These tests stand in for a broker that has BLOCKED the connection (a memory or disk alarm): it
// stops reading, so no close-ok ever comes. Hermetic: no broker, no network.

// hungChannel is a channel whose Close never completes (until the test releases it) and whose
// other calls succeed, so the operation's own outcome is decided and only the close hangs.
type hungChannel struct {
	channel // nil: any call the test did not anticipate panics, loudly
	release chan struct{}
	confirm chan amqp.Confirmation
	closed  chan *amqp.Error
}

func (c *hungChannel) Close() error       { <-c.release; return nil }
func (c *hungChannel) Confirm(bool) error { return nil }
func (c *hungChannel) NotifyPublish(ch chan amqp.Confirmation) chan amqp.Confirmation {
	c.confirm = ch
	return ch
}
func (c *hungChannel) NotifyClose(ch chan *amqp.Error) chan *amqp.Error { c.closed = ch; return ch }
func (c *hungChannel) PublishWithContext(context.Context, string, string, bool, bool, amqp.Publishing) error {
	return nil
}
func (c *hungChannel) ConsumeWithContext(context.Context, string, string, bool, bool, bool, bool, amqp.Table) (<-chan amqp.Delivery, error) {
	return make(chan amqp.Delivery), nil
}
func (c *hungChannel) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}
func (c *hungChannel) ExchangeDeclare(string, string, bool, bool, bool, bool, amqp.Table) error {
	return nil
}
func (c *hungChannel) QueueDelete(string, bool, bool, bool) (int, error)    { return 0, nil }
func (c *hungChannel) QueueUnbind(string, string, string, amqp.Table) error { return nil }
func (c *hungChannel) ExchangeDelete(string, bool, bool) error              { return nil }

// hungConn is a connection whose Close (and, when hangOpen, Channel) never completes.
type hungConn struct {
	release  chan struct{}
	hangOpen bool
	blocks   chan amqp.Blocking
	aborts   atomic.Int32
	once     sync.Once
}

func newHungConn() *hungConn {
	return &hungConn{release: make(chan struct{}), blocks: make(chan amqp.Blocking, 8)}
}

func (c *hungConn) Channel() (channel, error) {
	if c.hangOpen {
		<-c.release
		return nil, amqp.ErrClosed
	}
	return &hungChannel{release: c.release}, nil
}
func (c *hungConn) Close() error { <-c.release; return nil }
func (c *hungConn) abort()       { c.aborts.Add(1) }
func (c *hungConn) NotifyBlocked(r chan amqp.Blocking) chan amqp.Blocking {
	return c.blocks
}
func (c *hungConn) free() { c.once.Do(func() { close(c.release) }) }

// testEngine builds an Engine over c with short waits. rpcTimeout is deliberately LONGER than the
// bound a test asserts, so an operation that relied on the watchdog instead of the close bound
// fails the assertion instead of passing slowly.
func testEngine(t *testing.T, c *hungConn) *Engine {
	t.Helper()
	e := newEngine(c)
	e.confirmTimeout = 50 * time.Millisecond
	e.closeTimeout = 100 * time.Millisecond
	e.rpcTimeout = 5 * time.Second
	t.Cleanup(c.free)
	return e
}

// within runs fn and fails the test if it does not return inside d.
func within[T any](t *testing.T, d time.Duration, what string, fn func() T) T {
	t.Helper()
	res := make(chan T, 1)
	go func() { res <- fn() }()
	select {
	case v := <-res:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
		panic("unreachable")
	}
}

const returnBound = 2 * time.Second

func TestEveryOperationReturnsWhenTheChannelCloseNeverCompletes(t *testing.T) {
	cases := []struct {
		name     string
		run      func(e *Engine) Outcome
		wantOK   bool
		wantText string
	}{
		{"Publish", func(e *Engine) Outcome { return e.Publish("x", "rk", []byte("b"), Props{}) }, true, ""},
		{"PublishConfirmed", func(e *Engine) Outcome { return e.PublishConfirmed("x", "rk", []byte("b"), Props{}) }, false, "timed out waiting for publisher confirm"},
		{"Consume", func(e *Engine) Outcome { o, _ := e.Consume("q", 50*time.Millisecond); return o }, false, "timed out waiting for a message"},
		{"DeclareQueue", func(e *Engine) Outcome { return e.DeclareQueue("q") }, true, ""},
		{"DeclareExchange", func(e *Engine) Outcome { return e.DeclareExchange("x", "topic") }, true, ""},
		{"DeleteQueue", func(e *Engine) Outcome { return e.DeleteQueue("q") }, true, ""},
		{"UnbindQueue", func(e *Engine) Outcome { return e.UnbindQueue("q", "x", "rk") }, true, ""},
		{"DeleteExchange", func(e *Engine) Outcome { return e.DeleteExchange("x") }, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHungConn()
			e := testEngine(t, c)
			start := time.Now()
			out := within(t, returnBound, tc.name, func() Outcome { return tc.run(e) })
			if out.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (outcome %+v)", out.OK, tc.wantOK, out)
			}
			// The outcome the operation had already decided must survive the hung close.
			if !strings.Contains(out.Text, tc.wantText) {
				t.Fatalf("Text = %q, want it to contain %q", out.Text, tc.wantText)
			}
			if strings.Contains(out.Text, "torn down") {
				t.Fatalf("a close that hung replaced the operation's own outcome: %q", out.Text)
			}
			if got := c.aborts.Load(); got != 1 {
				t.Fatalf("connection torn down %d times, want exactly once (the close never completed)", got)
			}
			if d := time.Since(start); d < e.closeTimeout {
				t.Fatalf("returned in %s, before the close bound %s: the close was not attempted", d, e.closeTimeout)
			}
		})
	}
}

func TestEngineCloseReturnsWhenTheBrokerNeverAnswers(t *testing.T) {
	c := newHungConn()
	e := testEngine(t, c)
	err := within(t, returnBound, "Engine.Close", e.Close)
	if err == nil || !strings.Contains(err.Error(), "torn down") {
		t.Fatalf("Close error = %v, want one saying the connection was torn down", err)
	}
	if got := c.aborts.Load(); got != 1 {
		t.Fatalf("connection torn down %d times, want once", got)
	}
}

func TestOperationReturnsWhenTheBrokerNeverOpensTheChannel(t *testing.T) {
	c := newHungConn()
	c.hangOpen = true
	e := testEngine(t, c)
	e.rpcTimeout = 150 * time.Millisecond
	out := within(t, returnBound, "Publish", func() Outcome { return e.Publish("x", "rk", nil, Props{}) })
	if out.OK || !strings.Contains(out.Text, "did not answer within 150ms") {
		t.Fatalf("outcome = %+v, want a failure naming the unanswered wait", out)
	}
	if c.aborts.Load() != 1 {
		t.Fatalf("connection torn down %d times, want once", c.aborts.Load())
	}
}

// waitBlocked waits until the engine has seen (or cleared) the broker's block.
func waitBlocked(t *testing.T, e *Engine, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if (e.blockedNote() != "") == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("engine never reached blocked=%v", want)
}

func TestBlockedConnectionIsNamedWithTheBrokersReason(t *testing.T) {
	c := newHungConn()
	e := testEngine(t, c)
	c.blocks <- amqp.Blocking{Active: true, Reason: "low on memory (amqp://alarm:s3cret@broker/)"}
	waitBlocked(t, e, true)

	out := within(t, returnBound, "PublishConfirmed", func() Outcome {
		return e.PublishConfirmed("x", "rk", []byte("b"), Props{})
	})
	if out.OK {
		t.Fatalf("outcome OK on a blocked connection: %+v", out)
	}
	for _, want := range []string{
		"timed out waiting for publisher confirm",
		"the broker blocked this connection",
		"low on memory",
		"may still be delivered if the alarm clears while the connection is open",
	} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("Text = %q, missing %q", out.Text, want)
		}
	}
	if strings.Contains(out.Text, "s3cret") {
		t.Fatalf("the broker's reason went out unredacted: %q", out.Text)
	}
}

func TestUnblockedConnectionIsNotNamed(t *testing.T) {
	c := newHungConn()
	e := testEngine(t, c)
	c.blocks <- amqp.Blocking{Active: true, Reason: "disk alarm"}
	waitBlocked(t, e, true)
	c.blocks <- amqp.Blocking{Active: false}
	waitBlocked(t, e, false)

	out := within(t, returnBound, "PublishConfirmed", func() Outcome {
		return e.PublishConfirmed("x", "rk", []byte("b"), Props{})
	})
	if strings.Contains(out.Text, "blocked") {
		t.Fatalf("an unblocked connection was named as blocked: %q", out.Text)
	}
}

func TestBlockedConnectionNamedOnAWatchdogFailureToo(t *testing.T) {
	c := newHungConn()
	c.hangOpen = true
	e := testEngine(t, c)
	e.rpcTimeout = 100 * time.Millisecond
	c.blocks <- amqp.Blocking{Active: true, Reason: "memory alarm"}
	waitBlocked(t, e, true)
	out := within(t, returnBound, "DeclareQueue", func() Outcome { return e.DeclareQueue("q") })
	if !strings.Contains(out.Text, "memory alarm") || !strings.Contains(out.Text, "did not answer") {
		t.Fatalf("Text = %q, want the unanswered wait AND the block reason", out.Text)
	}
}

func TestBlockedSuccessStaysSuccess(t *testing.T) {
	c := newHungConn()
	e := testEngine(t, c)
	c.blocks <- amqp.Blocking{Active: true, Reason: "memory alarm"}
	waitBlocked(t, e, true)
	out := within(t, returnBound, "DeclareQueue", func() Outcome { return e.DeclareQueue("q") })
	if !out.OK || out.Text != "" {
		t.Fatalf("a successful operation was annotated: %+v", out)
	}
}
