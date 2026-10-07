package amqpengine

import (
	"context"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// confirmTimeout bounds how long PublishConfirmed waits for the broker to
// either confirm or close the channel.
const confirmTimeout = 10 * time.Second

// closeGrace is how long PublishConfirmed waits, after its NotifyPublish
// channel has closed with no confirmation delivered, to also observe
// NotifyClose's real reason before falling back to a generic closed error.
// Both notifications fire from the same broker-side shutdown, not
// necessarily in the same instant — the same race pkg/busclient.Client
// resolves in msgbus.
const closeGrace = 200 * time.Millisecond

// Props are the publishing properties a probe sends. UserID and ContentType
// are named explicitly because they are what the broker's refusal checks
// turn on: a spoofed UserID (one that does not match the connection's
// authenticated identity) is refused with 406 PRECONDITION_FAILED.
type Props struct {
	UserID        string
	ContentType   string
	CorrelationID string
	Headers       map[string]any
	// Persistent sets delivery-mode 2 (survives a broker restart) when true;
	// probes default to non-persistent (0/false) since they are disposable.
	Persistent bool
}

func (p Props) publishing(body []byte) amqp.Publishing {
	pub := amqp.Publishing{
		ContentType:   p.ContentType,
		UserId:        p.UserID,
		CorrelationId: p.CorrelationID,
		Body:          body,
	}
	if len(p.Headers) > 0 {
		pub.Headers = amqp.Table(p.Headers)
	}
	if p.Persistent {
		pub.DeliveryMode = amqp.Persistent
	}
	return pub
}

// Every operation below runs under Engine.run (a hard watchdog, guard.go) and closes its channel
// with Engine.closeChannel (bounded by closeTimeout). Neither can leave an operation waiting on a
// broker that has stopped answering, and a failed outcome is named when the broker has blocked the
// connection.

// Publish sends body to exchange under routingKey on its own fire-and-forget
// channel (no publisher confirms). Because AMQP publishing is asynchronous,
// a permission refusal (403/406) the broker raises AFTER accepting the
// frame closes the channel a moment later, past the point Publish has
// already returned — this Outcome will usually read OK even when the
// message was in fact refused. Use PublishConfirmed when the refusal itself
// is what is being probed for; Publish is for the "does this basic path
// work at all" case where losing a late async refusal doesn't matter.
func (e *Engine) Publish(exchange, routingKey string, body []byte, props Props) Outcome {
	return e.runOutcome(e.rpcTimeout, func() Outcome {
		ch, err := e.conn.Channel()
		if err != nil {
			return errOutcome(err)
		}
		defer e.closeChannel(ch)
		if err := ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, props.publishing(body)); err != nil {
			return errOutcome(err)
		}
		return ok()
	})
}

// PublishConfirmed is Publish's reliable sibling. Its channel is put into
// publisher-confirm mode before the send, and the Outcome is derived from
// whichever the broker does first: ack/nack the confirm, or close the
// channel outright. RabbitMQ's actual behaviour on a 403/406 permission
// refusal is the latter — it does not nack the confirm, it closes the
// channel asynchronously — so PublishConfirmed races NotifyPublish against
// NotifyClose (the same pattern pkg/busclient.Client.publish uses in
// msgbus) instead of only waiting on the confirm, which would otherwise
// hang until confirmTimeout and lose the refusal's actual code.
func (e *Engine) PublishConfirmed(exchange, routingKey string, body []byte, props Props) Outcome {
	return e.runOutcome(e.confirmTimeout+e.rpcTimeout, func() Outcome {
		ch, err := e.conn.Channel()
		if err != nil {
			return errOutcome(err)
		}
		// The confirm timeout below returns THROUGH this deferred close; on a blocked broker the
		// close gets no answer, so it is bounded and never replaces the outcome already decided.
		defer e.closeChannel(ch)
		if err := ch.Confirm(false); err != nil {
			return errOutcome(err)
		}
		confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
		closed := ch.NotifyClose(make(chan *amqp.Error, 1))

		if err := ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, props.publishing(body)); err != nil {
			// A synchronous publish error (e.g. the channel was already dead) —
			// no need to race the notify channels, the error IS the outcome.
			return errOutcome(err)
		}

		timer := time.NewTimer(e.confirmTimeout)
		defer timer.Stop()
		select {
		case conf, chOK := <-confirms:
			if !chOK {
				// NotifyPublish closed with no confirmation delivered: the
				// channel died before it could ack/nack. Give NotifyClose a
				// short grace period to deliver the real AMQP reply code.
				return outcomeFromClose(waitCloseReason(closed))
			}
			if conf.Ack {
				return ok()
			}
			return Outcome{OK: false, Code: 0, Text: "publish nacked by broker (no channel close observed)"}
		case aerr := <-closed:
			return outcomeFromClose(aerr)
		case <-timer.C:
			return Outcome{OK: false, Code: 0, Text: "timed out waiting for publisher confirm"}
		}
	})
}

// outcomeFromClose turns a channel-close reason into an Outcome, falling
// back to amqp.ErrClosed's generic code when the close carried no reason
// (a local/graceful close, which a probe should never see mid-publish, but
// nil is defensively handled the same way busclient.waitCloseReason does).
func outcomeFromClose(aerr *amqp.Error) Outcome {
	if aerr == nil {
		aerr = amqp.ErrClosed
	}
	return Outcome{OK: false, Code: aerr.Code, Text: RedactURL(aerr.Reason)}
}

// waitCloseReason gives closed a short grace period to deliver the real AMQP
// close reason, returning nil (which outcomeFromClose maps to amqp.ErrClosed)
// if nothing arrives in time.
func waitCloseReason(closed <-chan *amqp.Error) *amqp.Error {
	select {
	case e := <-closed:
		return e
	case <-time.After(closeGrace):
		return nil
	}
}

// Consume performs a single-message receive from queue on its own channel:
// it waits up to timeout for exactly one delivery, acks it, and returns.
// The second return value is the delivered message body (nil unless
// Outcome.OK). A permission refusal on the consume itself (e.g. reading
// another agent's queue, 403 access-refused) is a synchronous AMQP method
// response, so it comes back as an error from Channel.Consume — no
// NotifyClose race is needed here the way PublishConfirmed needs one.
func (e *Engine) Consume(queue string, timeout time.Duration) (Outcome, []byte) {
	return e.run(timeout+e.rpcTimeout, func() (Outcome, []byte) {
		ch, err := e.conn.Channel()
		if err != nil {
			return errOutcome(err), nil
		}
		defer e.closeChannel(ch)

		deliveries, err := ch.ConsumeWithContext(context.Background(), queue, "", false, false, false, false, nil)
		if err != nil {
			return errOutcome(err), nil
		}

		select {
		case d, chOK := <-deliveries:
			if !chOK {
				return Outcome{OK: false, Text: "consumer channel closed before a message arrived"}, nil
			}
			_ = d.Ack(false)
			return ok(), d.Body
		case <-time.After(timeout):
			return Outcome{OK: false, Text: "timed out waiting for a message"}, nil
		}
	})
}

// withChannel runs a one-call operation on its own channel, closed (bounded) when it is done.
func (e *Engine) withChannel(call func(ch channel) error) Outcome {
	return e.runOutcome(e.rpcTimeout, func() Outcome {
		ch, err := e.conn.Channel()
		if err != nil {
			return errOutcome(err)
		}
		defer e.closeChannel(ch)
		if err := call(ch); err != nil {
			return errOutcome(err)
		}
		return ok()
	})
}

// DeclareQueue asserts (non-passively — passive=false) that queue exists,
// creating it durable if it does not. A caller with no `configure`
// permission on the vhost is refused 403 access-refused.
func (e *Engine) DeclareQueue(name string) Outcome {
	return e.withChannel(func(ch channel) error {
		_, err := ch.QueueDeclare(name, true, false, false, false, nil)
		return err
	})
}

// DeclareExchange asserts (non-passively — passive=false) that a durable
// exchange of the given kind ("topic", "fanout", ...) exists.
func (e *Engine) DeclareExchange(name, kind string) Outcome {
	return e.withChannel(func(ch channel) error {
		return ch.ExchangeDeclare(name, kind, true, false, false, false, nil)
	})
}

// DeleteQueue deletes a queue unconditionally (ifUnused=false, ifEmpty=false — the caller already
// decided this delete is safe; the chain step's own guard, P3 #24c, is what stands between an
// author and a live queue, not a broker precondition that would only turn a refusal into a
// different refusal). A caller with no `configure` permission is refused 403 access-refused; a
// caller naming a queue that does not exist is refused 404 not-found.
func (e *Engine) DeleteQueue(name string) Outcome {
	return e.withChannel(func(ch channel) error {
		_, err := ch.QueueDelete(name, false, false, false)
		return err
	})
}

// UnbindQueue removes one binding of queue to exchange under routingKey. Unbinding a binding that
// does not exist is refused 404 not-found by the broker (amqp091-go's QueueUnbind is synchronous, so
// that refusal comes back as an ordinary channel error, exactly like Consume's own 403/404 paths).
func (e *Engine) UnbindQueue(queue, exchange, routingKey string) Outcome {
	return e.withChannel(func(ch channel) error {
		return ch.QueueUnbind(queue, routingKey, exchange, nil)
	})
}

// DeleteExchange deletes an exchange unconditionally (ifUnused=false — see DeleteQueue's note on
// why a broker precondition is not this call's job).
func (e *Engine) DeleteExchange(name string) Outcome {
	return e.withChannel(func(ch channel) error {
		return ch.ExchangeDelete(name, false, false)
	})
}
