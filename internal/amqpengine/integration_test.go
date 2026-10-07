package amqpengine_test

// AC-D18 integration test: proves amqpengine's probes against a REAL
// RabbitMQ broker, provisioned with a restricted user that mirrors
// msgbus's actual "argus-test" worker identity (deploy/k8s/15-registry.yaml
// + cmd/msgbus-provision/provision.go buildIdentities, manager "hop"):
//
//	vhost configure: ^$                                     (deny-all)
//	vhost write:     ^(msgbus\.inbox|msgbus\.presence)$
//	vhost read:      ^(msgbus\.agent\.argus-test|msgbus\.presence\.stream)$
//	topic write on msgbus.inbox: ^inbox\.agent\.hop$        (policy.TopicWriteRegex, worker role)
//	topic read on msgbus.inbox:  ^$                         (deny-all)
//
// Skipped unless ARGUS_TEST_AMQP_URL is set (the restricted user's URL).
// ARGUS_TEST_AMQP_ADMIN_URL (an administrator login on the same throwaway
// broker) is required alongside it to provision the fixture topology and to
// read a message back as an admin for the delivery proof (e) — both are
// exported together by the same setup script that stands up the broker, so
// in practice they are never set independently.
import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
)

const (
	testExchange  = "msgbus.inbox"
	hopQueue      = "msgbus.agent.hop" // stands in for the manager's queue
	allowedRK     = "inbox.agent.hop"  // the ONLY routing key argus-test's topic permission allows
	forbiddenRK   = "inbox.all"        // a manager-only routing key; refused for a worker
	restrictedUID = "argus-test"
	spoofedUID    = "someone-else"
)

func TestAMQPEngine_Integration(t *testing.T) {
	url := os.Getenv("ARGUS_TEST_AMQP_URL")
	if url == "" {
		t.Skip("ARGUS_TEST_AMQP_URL not set — skipping AC-D18 AMQP engine integration test")
	}
	adminURL := os.Getenv("ARGUS_TEST_AMQP_ADMIN_URL")
	if adminURL == "" {
		t.Fatal("ARGUS_TEST_AMQP_ADMIN_URL must be set alongside ARGUS_TEST_AMQP_URL (same setup script exports both)")
	}

	// --- fixture topology, asserted as admin (raw amqp091-go: binding a
	// queue is provisioning ceremony, not part of amqpengine's probe surface) ---
	adminConn, err := amqp.Dial(adminURL)
	if err != nil {
		t.Fatalf("admin dial: %v", err)
	}
	defer adminConn.Close()
	setupCh, err := adminConn.Channel()
	if err != nil {
		t.Fatalf("admin channel: %v", err)
	}
	if err := setupCh.ExchangeDeclare(testExchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatalf("declare %s: %v", testExchange, err)
	}
	if _, err := setupCh.QueueDeclare(hopQueue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare %s: %v", hopQueue, err)
	}
	if err := setupCh.QueueBind(hopQueue, allowedRK, testExchange, false, nil); err != nil {
		t.Fatalf("bind %s -> %s: %v", hopQueue, allowedRK, err)
	}
	// Purge any leftovers from a prior run against the same throwaway broker.
	if _, err := setupCh.QueuePurge(hopQueue, false); err != nil {
		t.Fatalf("purge %s: %v", hopQueue, err)
	}
	setupCh.Close()

	restricted, err := amqpengine.Connect("ARGUS_TEST_AMQP_URL")
	if err != nil {
		t.Fatalf("Connect(restricted): %v", err)
	}
	defer restricted.Close()

	// --- (a) publish with a spoofed user_id -> 406 ---
	t.Run("spoofed_user_id_406", func(t *testing.T) {
		body := []byte("spoofed-user-id-probe")
		out := restricted.PublishConfirmed(testExchange, allowedRK, body, amqpengine.Props{
			UserID:      spoofedUID, // != the authenticated connection identity (argus-test)
			ContentType: "text/plain",
		})
		if out.OK {
			t.Fatalf("spoofed user_id publish reported OK, want a 406 refusal: %+v", out)
		}
		if out.Code != 406 {
			t.Fatalf("Code = %d, want 406 PRECONDITION_FAILED; Outcome=%+v", out.Code, out)
		}
		t.Logf("observed refusal: %+v", out)
	})

	// --- (b) publish to a forbidden routing key -> 403 ---
	t.Run("forbidden_routing_key_403", func(t *testing.T) {
		body := []byte("forbidden-routing-key-probe")
		out := restricted.PublishConfirmed(testExchange, forbiddenRK, body, amqpengine.Props{
			UserID:      restrictedUID,
			ContentType: "text/plain",
		})
		if out.OK {
			t.Fatalf("publish to forbidden routing key %q reported OK, want a 403 refusal: %+v", forbiddenRK, out)
		}
		if out.Code != 403 {
			t.Fatalf("Code = %d, want 403 ACCESS_REFUSED; Outcome=%+v", out.Code, out)
		}
		t.Logf("observed refusal: %+v", out)
	})

	// --- (c) consume another user's queue -> 403 ---
	t.Run("consume_foreign_queue_403", func(t *testing.T) {
		out, body := restricted.Consume(hopQueue, 5*time.Second)
		if out.OK {
			t.Fatalf("consuming %s reported OK, want a 403 refusal: %+v body=%q", hopQueue, out, body)
		}
		if out.Code != 403 {
			t.Fatalf("Code = %d, want 403 ACCESS_REFUSED; Outcome=%+v", out.Code, out)
		}
		t.Logf("observed refusal: %+v", out)
	})

	// --- (d) declare a queue -> 403 ---
	t.Run("declare_queue_403", func(t *testing.T) {
		out := restricted.DeclareQueue("msgbus.agent.spoof-probe-ac-d18")
		if out.OK {
			t.Fatalf("DeclareQueue reported OK, want a 403 refusal (configure denied): %+v", out)
		}
		if out.Code != 403 {
			t.Fatalf("Code = %d, want 403 ACCESS_REFUSED; Outcome=%+v", out.Code, out)
		}
		t.Logf("observed refusal: %+v", out)
	})

	// --- (e) an allowed publish -> OK, and the message is actually
	// delivered, read back as an admin — a real msgbus v2 envelope, round
	// tripped over the live broker (not simulated). ---
	var deliveredMsg envelope.Message
	t.Run("allowed_publish_delivers_e2e", func(t *testing.T) {
		msg := envelope.Message{
			MessageID:     uuid.NewString(),
			Kind:          envelope.KindMessage,
			Source:        envelope.SourceSystem,
			OriginTrust:   envelope.TrustAgent,
			FromAgentID:   restrictedUID,
			ToInbox:       "agent.hop",
			Prompt:        "AC-D18 integration: allowed publish must be delivered",
			Reply:         envelope.Reply{Mode: envelope.ReplyNone},
			CorrelationID: "corr-ac-d18-allowed-publish",
			EnqueuedAt:    time.Now().UTC().Truncate(time.Millisecond),
			Intent:        envelope.IntentInform,
		}
		body, headers, err := amqpengine.EncodeEnvelope(msg)
		if err != nil {
			t.Fatalf("EncodeEnvelope: %v", err)
		}

		out := restricted.PublishConfirmed(testExchange, allowedRK, body, amqpengine.Props{
			UserID:      restrictedUID,
			ContentType: "application/avro",
			Headers:     headers,
		})
		if !out.OK || out.Code != 0 {
			t.Fatalf("allowed publish refused, want OK/Code 0: %+v", out)
		}

		// Read it back as admin — proves actual delivery, not a simulated result.
		adminCh, err := adminConn.Channel()
		if err != nil {
			t.Fatalf("admin channel: %v", err)
		}
		defer adminCh.Close()
		deliveries, err := adminCh.ConsumeWithContext(context.Background(), hopQueue, "", false, false, false, false, nil)
		if err != nil {
			t.Fatalf("admin consume %s: %v", hopQueue, err)
		}
		select {
		case d := <-deliveries:
			_ = d.Ack(false)
			got, derr := amqpengine.DecodeEnvelope(d.Body, headersToAny(d.Headers))
			if derr != nil {
				t.Fatalf("admin-side DecodeEnvelope: %v", derr)
			}
			if got.MessageID != msg.MessageID || got.Prompt != msg.Prompt || got.CorrelationID != msg.CorrelationID {
				t.Fatalf("delivered envelope mismatch: got %+v, want messageId=%s prompt=%s corr=%s",
					got, msg.MessageID, msg.Prompt, msg.CorrelationID)
			}
			deliveredMsg = got
		case <-time.After(5 * time.Second):
			t.Fatal("admin never received the allowed publish within 5s — not actually delivered")
		}
	})
	if deliveredMsg.MessageID == "" {
		t.Fatal("prerequisite subtest allowed_publish_delivers_e2e did not run/pass; cannot continue")
	}

	// --- (f) after a refusal, the NEXT probe on the SAME connection still works ---
	t.Run("next_probe_after_refusal_still_works", func(t *testing.T) {
		// restricted has already taken four refusals above (406, 403, 403,
		// 403), each on its own channel per Publish/PublishConfirmed/Consume/
		// DeclareQueue — this repeats an allowed publish on that SAME *Engine
		// (same underlying connection) to prove none of those refusals
		// poisoned it.
		body := []byte("post-refusal probe")
		out := restricted.PublishConfirmed(testExchange, allowedRK, body, amqpengine.Props{
			UserID:      restrictedUID,
			ContentType: "text/plain",
		})
		if !out.OK || out.Code != 0 {
			t.Fatalf("post-refusal publish on the same connection failed: %+v", out)
		}
	})
}

// headersToAny converts an amqp.Table (map[string]interface{} under the
// hood) to the map[string]any DecodeEnvelope expects — a same-underlying-type
// conversion, not a copy of semantics.
func headersToAny(t amqp.Table) map[string]any {
	return map[string]any(t)
}
