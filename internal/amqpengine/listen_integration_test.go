package amqpengine_test

// integration test: Listen against a REAL RabbitMQ, gated like TestAMQPEngine_Integration (skipped
// unless ARGUS_TEST_AMQP_URL and ARGUS_TEST_AMQP_ADMIN_URL are set; the user needs configure+write+read on
// `argus-tap-*` and read on the exchange). It proves, end to end: the private queue binds, a published copy reaches
// it while the original queue still holds its own copy (nothing is taken), a gzip body arrives with its
// content-encoding, and the queue is gone after DeleteQueue.

import (
	"context"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
)

func TestAMQPEngine_ListenIntegration(t *testing.T) {
	url := os.Getenv("ARGUS_TEST_AMQP_URL")
	adminURL := os.Getenv("ARGUS_TEST_AMQP_ADMIN_URL")
	if url == "" || adminURL == "" {
		t.Skip("ARGUS_TEST_AMQP_URL / ARGUS_TEST_AMQP_ADMIN_URL not set - skipping the Listen integration test")
	}
	admin, err := amqp.Dial(adminURL)
	if err != nil {
		t.Fatalf("admin dial: %v", err)
	}
	defer admin.Close()
	ch, err := admin.Channel()
	if err != nil {
		t.Fatalf("admin channel: %v", err)
	}
	const ex, rk, owned = "argus.listen.test", "evt.created", "argus.listen.test.owned"
	if err := ch.ExchangeDeclare(ex, "topic", false, true, false, false, nil); err != nil {
		t.Fatalf("declare exchange: %v", err)
	}
	if _, err := ch.QueueDeclare(owned, false, true, false, false, nil); err != nil {
		t.Fatalf("declare owned queue: %v", err)
	}
	if err := ch.QueueBind(owned, rk, ex, false, nil); err != nil {
		t.Fatalf("bind owned queue: %v", err)
	}

	e, err := amqpengine.Connect("ARGUS_TEST_AMQP_URL")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer e.Close()

	const q = "argus-tap-tr-listen-it-0"
	if o := e.Listen(q, ex, rk, 120000); !o.OK {
		t.Fatalf("Listen = %+v", o)
	}
	defer e.DeleteQueue(q)

	body := []byte{0x1f, 0x8b} // not a real gzip stream: the engine returns bytes and properties, it does not decode
	if err := ch.PublishWithContext(context.Background(), ex, rk, false, false,
		amqp.Publishing{ContentEncoding: "gzip", Body: body}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	o, d := e.ConsumeDelivery(q, 5*time.Second)
	if !o.OK || d.ContentEncoding != "gzip" || string(d.Body) != string(body) {
		t.Fatalf("ConsumeDelivery(listen queue) = %+v %+v", o, d)
	}
	if _, ok, err := ch.Get(owned, true); err != nil || !ok {
		t.Errorf("the owned queue lost its copy (ok=%v err=%v): a listen must take nothing from another consumer", ok, err)
	}
	if o := e.DeleteQueue(q); !o.OK {
		t.Errorf("DeleteQueue(listen queue) = %+v", o)
	}
	if o := e.DeleteQueue(q); o.OK || o.Code != 404 {
		t.Errorf("second DeleteQueue = %+v, want 404 (the queue is gone)", o)
	}
}
