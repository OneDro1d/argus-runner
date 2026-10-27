package amqpengine

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The engine's listen and bind, and the delivery's properties, against a RECORDING channel
// (no broker). The calls are asserted in order and on ONE channel.

type recChannel struct {
	channel // nil: an unanticipated call panics loudly
	id      int
	log     *recLog
	deliver chan amqp.Delivery
}

type recLog struct {
	mu       sync.Mutex
	calls    []string
	declared []declareCall
	binds    []bindCall
	channels int
	closed   []int
}

type declareCall struct {
	ch                                     int
	name                                   string
	durable, autoDelete, exclusive, noWait bool
	args                                   amqp.Table
}

type bindCall struct {
	ch                  int
	name, key, exchange string
}

func (r *recLog) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (c *recChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	c.log.add("declare")
	c.log.mu.Lock()
	c.log.declared = append(c.log.declared, declareCall{c.id, name, durable, autoDelete, exclusive, noWait, args})
	c.log.mu.Unlock()
	return amqp.Queue{Name: name}, nil
}

func (c *recChannel) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	c.log.add("bind")
	c.log.mu.Lock()
	c.log.binds = append(c.log.binds, bindCall{c.id, name, key, exchange})
	c.log.mu.Unlock()
	return nil
}

func (c *recChannel) ConsumeWithContext(_ context.Context, _, _ string, _, _, _, _ bool, _ amqp.Table) (<-chan amqp.Delivery, error) {
	return c.deliver, nil
}

func (c *recChannel) Close() error {
	c.log.mu.Lock()
	c.log.closed = append(c.log.closed, c.id)
	c.log.mu.Unlock()
	return nil
}

type recConn struct {
	log     *recLog
	deliver chan amqp.Delivery
}

func (c *recConn) Channel() (channel, error) {
	c.log.mu.Lock()
	c.log.channels++
	id := c.log.channels
	c.log.mu.Unlock()
	return &recChannel{id: id, log: c.log, deliver: c.deliver}, nil
}
func (c *recConn) Close() error { return nil }
func (c *recConn) abort()       {}
func (c *recConn) NotifyBlocked(r chan amqp.Blocking) chan amqp.Blocking {
	return make(chan amqp.Blocking)
}

func (c *recConn) NotifyClose(r chan *amqp.Error) chan *amqp.Error { return r }

func recEngine() (*Engine, *recConn) {
	conn := &recConn{log: &recLog{}, deliver: make(chan amqp.Delivery, 1)}
	e := newEngine(conn)
	e.rpcTimeout = 2 * time.Second
	e.closeTimeout = 500 * time.Millisecond
	return e, conn
}

func TestListen_DeclaresWithExactArgumentsAndBindsOnOneChannel(t *testing.T) {
	e, conn := recEngine()
	o := e.Listen("argus-tap-tr-1-2", "orders.events", "order.created", 120000)
	if !o.OK {
		t.Fatalf("Listen = %+v, want OK", o)
	}
	lg := conn.log
	if want := []string{"declare", "bind"}; !reflect.DeepEqual(lg.calls, want) {
		t.Fatalf("calls = %v, want %v (declare, then bind)", lg.calls, want)
	}
	if lg.channels != 1 {
		t.Errorf("channels opened = %d, want exactly 1 (declare and bind on ONE channel)", lg.channels)
	}
	d := lg.declared[0]
	if d.name != "argus-tap-tr-1-2" || d.durable || d.autoDelete || d.exclusive || d.noWait {
		t.Errorf("declare = %+v, want name argus-tap-tr-1-2, durable=false autoDelete=false exclusive=false noWait=false", d)
	}
	wantArgs := amqp.Table{"x-expires": int32(120000), "x-message-ttl": int32(120000)}
	if !reflect.DeepEqual(d.args, wantArgs) {
		t.Errorf("declare args = %v, want %v (x-expires AND x-message-ttl)", d.args, wantArgs)
	}
	b := lg.binds[0]
	if b.ch != d.ch || b.name != "argus-tap-tr-1-2" || b.exchange != "orders.events" || b.key != "order.created" {
		t.Errorf("bind = %+v, want queue argus-tap-tr-1-2 to exchange orders.events under order.created on channel %d", b, d.ch)
	}
	if len(lg.closed) != 1 || lg.closed[0] != d.ch {
		t.Errorf("closed channels = %v, want the one channel closed once", lg.closed)
	}
}

func TestBindQueue_BindsExistingQueueWithoutDeclaring(t *testing.T) {
	e, conn := recEngine()
	if o := e.BindQueue("q", "x", "rk"); !o.OK {
		t.Fatalf("BindQueue = %+v", o)
	}
	if want := []string{"bind"}; !reflect.DeepEqual(conn.log.calls, want) {
		t.Errorf("calls = %v, want only a bind (no declare of a queue the chain did not make)", conn.log.calls)
	}
	if b := conn.log.binds[0]; b.name != "q" || b.exchange != "x" || b.key != "rk" {
		t.Errorf("bind = %+v", b)
	}
}

func TestConsumeDelivery_ReturnsTheDeliverysProperties(t *testing.T) {
	e, conn := recEngine()
	conn.deliver <- amqp.Delivery{Body: []byte("zz"), ContentEncoding: "gzip", ContentType: "application/json"}
	o, d := e.ConsumeDelivery("q", time.Second)
	if !o.OK || string(d.Body) != "zz" || d.ContentEncoding != "gzip" || d.ContentType != "application/json" {
		t.Fatalf("ConsumeDelivery = %+v %+v", o, d)
	}
	conn.deliver <- amqp.Delivery{Body: []byte("plain")}
	if o, b := e.Consume("q", time.Second); !o.OK || string(b) != "plain" {
		t.Errorf("Consume (body only, unchanged) = %+v %q", o, b)
	}
}

func TestConsumeDelivery_TimeoutIsAnEmptyDelivery(t *testing.T) {
	e, _ := recEngine()
	o, d := e.ConsumeDelivery("q", 30*time.Millisecond)
	if o.OK || d.Body != nil || d.ContentEncoding != "" {
		t.Errorf("timeout = %+v %+v, want not OK and an empty delivery", o, d)
	}
}
