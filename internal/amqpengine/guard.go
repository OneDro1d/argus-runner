package amqpengine

import (
	"context"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// closeTimeout bounds every channel close and the engine's connection close. amqp091-go's
// Channel.Close and Connection.Close send a close frame and then wait for the broker's close-ok with
// NO deadline, and a broker that has BLOCKED a connection (a memory or disk alarm) stops reading from
// it, so it never answers. Without this bound one blocked publish held the executor for 7 min 50 s
// . When it expires the connection is torn down, so nothing keeps waiting on it.
const closeTimeout = 3 * time.Second

// rpcTimeout is the watchdog for the part of an operation that waits on the broker for a method
// response (channel.open, confirm.select, queue.declare, ...). Each of those waits has no deadline
// in amqp091-go either. An operation that has its own, shorter wait (confirmTimeout, Consume's
// timeout) gets that wait PLUS this.
const rpcTimeout = 30 * time.Second

// abortWait is how long the engine waits for a forced teardown to finish before giving up on it.
const abortWait = time.Second

// channel is the slice of *amqp.Channel the probes drive. It exists so a test can stand in a channel
// whose Close never completes; *amqp.Channel satisfies it unchanged.
type channel interface {
	Close() error
	Confirm(noWait bool) error
	NotifyPublish(confirm chan amqp.Confirmation) chan amqp.Confirmation
	NotifyClose(c chan *amqp.Error) chan *amqp.Error
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
	ConsumeWithContext(ctx context.Context, queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	QueueDelete(name string, ifUnused, ifEmpty, noWait bool) (int, error)
	QueueUnbind(name, key, exchange string, args amqp.Table) error
	ExchangeDelete(name string, ifUnused, noWait bool) error
}

// connection is the slice of *amqp.Connection the engine drives.
type connection interface {
	Channel() (channel, error)
	Close() error
	// abort tears the connection down WITHOUT waiting for the broker.
	abort()
	NotifyBlocked(receiver chan amqp.Blocking) chan amqp.Blocking
}

// realConn adapts *amqp.Connection to connection.
type realConn struct{ *amqp.Connection }

func (r realConn) Channel() (channel, error) {
	ch, err := r.Connection.Channel()
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// abort: CloseDeadline with a deadline already in the past makes the close frame's write fail at
// once, and CloseDeadline shuts the connection down on the way out whatever the write did. The past
// deadline also wakes any other goroutine stuck writing to the socket.
func (r realConn) abort() { _ = r.Connection.CloseDeadline(time.Now()) }

// boundedCall runs fn in its own goroutine and waits at most d for it. The result goes through a
// buffered channel, so an abandoned fn can finish later without blocking or writing anywhere shared.
func boundedCall[T any](d time.Duration, fn func() T) (T, bool) {
	res := make(chan T, 1)
	go func() { res <- fn() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case v := <-res:
		return v, true
	case <-t.C:
		var zero T
		return zero, false
	}
}

// teardown forces the connection down and returns once that is done, or after abortWait.
func (e *Engine) teardown() {
	boundedCall(abortWait, func() struct{} {
		e.conn.abort()
		return struct{}{}
	})
}

// closeChannel closes ch within e.closeTimeout. A close that does not complete means the broker is
// not answering, so the connection is torn down. The close error is never reported: the operation's
// own outcome was decided before this ran and a close error must not replace it.
func (e *Engine) closeChannel(ch channel) {
	_, done := boundedCall(e.closeTimeout, func() struct{} {
		_ = ch.Close()
		return struct{}{}
	})
	if !done {
		e.teardown()
	}
}

// watchBlocked records connection.blocked / connection.unblocked. It ends when the connection shuts
// down (amqp091-go closes the receiver then).
func (e *Engine) watchBlocked(ch <-chan amqp.Blocking) {
	for b := range ch {
		e.mu.Lock()
		e.blocked, e.blockReason = b.Active, b.Reason
		e.mu.Unlock()
	}
}

// blockedNote is the clause appended to a failed outcome while the broker has the connection blocked.
func (e *Engine) blockedNote() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.blocked {
		return ""
	}
	reason := e.blockReason
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("the broker blocked this connection (reason: %s); a publish sent before the block may still be delivered if the alarm clears while the connection is open", reason)
}

// annotate names a blocked connection on a failed Outcome and redacts the result, the broker's
// reason included.
func (e *Engine) annotate(o Outcome) Outcome {
	if o.OK {
		return o
	}
	if note := e.blockedNote(); note != "" {
		if o.Text != "" {
			o.Text += "; "
		}
		o.Text += note
	}
	o.Text = RedactURL(o.Text)
	return o
}

// run is how every operation executes: fn under a hard watchdog of limit. If fn does not return in
// time the connection is torn down (so fn's own waits end) and the operation reports that, while a
// late result of the abandoned fn is dropped.
func (e *Engine) run(limit time.Duration, fn func() (Outcome, []byte)) (Outcome, []byte) {
	type result struct {
		o Outcome
		b []byte
	}
	r, done := boundedCall(limit, func() result {
		o, b := fn()
		return result{o, b}
	})
	if !done {
		e.teardown()
		return e.annotate(Outcome{Text: fmt.Sprintf("the broker did not answer within %s; the connection was torn down", limit)}), nil
	}
	return e.annotate(r.o), r.b
}

// runOutcome is run for an operation with no body.
func (e *Engine) runOutcome(limit time.Duration, fn func() Outcome) Outcome {
	o, _ := e.run(limit, func() (Outcome, []byte) { return fn(), nil })
	return o
}

// newEngine wraps an established connection and starts listening for connection.blocked.
func newEngine(c connection) *Engine {
	e := &Engine{conn: c, confirmTimeout: confirmTimeout, closeTimeout: closeTimeout, rpcTimeout: rpcTimeout}
	go e.watchBlocked(c.NotifyBlocked(make(chan amqp.Blocking, 8)))
	return e
}
