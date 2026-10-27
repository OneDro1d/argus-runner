package amqpengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Outcome.TimedOut is how a caller tells "the consume's own wait ran out on an open
// consumer" (the only thing `queue is empty` may pass on) from every other Code == 0 failure. Hermetic:
// a fake connection whose consumer channel the test controls.

// consumeChannel is a channel whose Consume returns the deliveries channel / error the test chose.
type consumeChannel struct {
	channel // nil: any call the test did not anticipate panics, loudly
	deliv   chan amqp.Delivery
	err     error
}

func (c *consumeChannel) Close() error { return nil }
func (c *consumeChannel) ConsumeWithContext(context.Context, string, string, bool, bool, bool, bool, amqp.Table) (<-chan amqp.Delivery, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.deliv, nil
}

type consumeConn struct {
	ch      *consumeChannel
	chanErr error
	hang    chan struct{} // when non-nil, Channel() blocks on it (the broker never answers)
	aborted chan struct{}
}

func (c *consumeConn) Channel() (channel, error) {
	if c.hang != nil {
		select {
		case <-c.hang:
		case <-c.aborted:
		}
		return nil, amqp.ErrClosed
	}
	if c.chanErr != nil {
		return nil, c.chanErr
	}
	return c.ch, nil
}
func (c *consumeConn) Close() error { return nil }
func (c *consumeConn) abort() {
	if c.aborted != nil {
		select {
		case <-c.aborted:
		default:
			close(c.aborted)
		}
	}
}
func (c *consumeConn) NotifyBlocked(r chan amqp.Blocking) chan amqp.Blocking { return r }
func (c *consumeConn) NotifyClose(r chan *amqp.Error) chan *amqp.Error       { return r }

func consumeEngine(c *consumeConn) *Engine {
	e := newEngine(c)
	e.closeTimeout = 100 * time.Millisecond
	e.rpcTimeout = 5 * time.Second
	return e
}

func TestConsumeTimedOutIsSetOnlyOnTheTimeoutBranch(t *testing.T) {
	t.Run("a wait that runs out on an open consumer is TimedOut", func(t *testing.T) {
		c := &consumeConn{ch: &consumeChannel{deliv: make(chan amqp.Delivery)}}
		out, _ := consumeEngine(c).ConsumeDelivery("q", 30*time.Millisecond)
		if out.OK || out.Code != 0 || !out.TimedOut {
			t.Fatalf("want a code-less TimedOut failure, got %+v", out)
		}
	})
	t.Run("a deliveries channel that closes is NOT TimedOut", func(t *testing.T) {
		deliv := make(chan amqp.Delivery)
		close(deliv)
		c := &consumeConn{ch: &consumeChannel{deliv: deliv}}
		out, _ := consumeEngine(c).ConsumeDelivery("q", 5*time.Second)
		if out.OK || out.Code != 0 || out.TimedOut {
			t.Fatalf("a closed consumer channel must not read as a timeout: %+v", out)
		}
		if !strings.Contains(out.Text, "consumer channel closed before a message arrived") {
			t.Fatalf("text: %q", out.Text)
		}
	})
	t.Run("a code-less consume error is NOT TimedOut", func(t *testing.T) {
		c := &consumeConn{ch: &consumeChannel{err: errors.New("write: broken pipe")}}
		out, _ := consumeEngine(c).ConsumeDelivery("q", 5*time.Second)
		if out.OK || out.Code != 0 || out.TimedOut {
			t.Fatalf("a code-less error must not read as a timeout: %+v", out)
		}
	})
	t.Run("a channel-open error is NOT TimedOut", func(t *testing.T) {
		c := &consumeConn{chanErr: amqp.ErrClosed}
		out, _ := consumeEngine(c).ConsumeDelivery("q", 5*time.Second)
		if out.OK || out.TimedOut {
			t.Fatalf("a channel-open failure must not read as a timeout: %+v", out)
		}
	})
	t.Run("the watchdog teardown is NOT TimedOut", func(t *testing.T) {
		c := &consumeConn{hang: make(chan struct{}), aborted: make(chan struct{})}
		e := consumeEngine(c)
		e.rpcTimeout = 100 * time.Millisecond
		out, _ := e.ConsumeDelivery("q", 10*time.Millisecond)
		if out.OK || out.Code != 0 || out.TimedOut {
			t.Fatalf("a watchdog teardown must not read as a timeout: %+v", out)
		}
		if !strings.Contains(out.Text, "the connection was torn down") {
			t.Fatalf("text: %q", out.Text)
		}
	})
}
