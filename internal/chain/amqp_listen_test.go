package chain

import (
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hamba/avro/v2"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The listen op, the registry that deletes what a listen made, and the gzip decode, against the
// fake Broker.

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func listenSpec(idx int) AMQPSpec {
	return AMQPSpec{Op: "listen", URLEnv: "X", Exchange: "orders.events", RoutingKey: "order.created", StepIndex: idx}
}

var saveQ = map[string]scenario.SaveSpec{"q": {Path: "queue"}}

func TestListen_NameIsArgusMadeAndTheBrokerGetsTheExactCall(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	dial, _ := dialerFor(f)
	reg := NewListenRegistry()
	res := Run("tr-abc", []Step{AMQPStep("tap", listenSpec(3), scenario.AMQPStepWant{Broker: accepts}, dial,
		WithListenRegistry(reg), WithSave(saveQ))})
	if res.Status != "passed" {
		t.Fatalf("listen = %+v", res)
	}
	if len(f.listens) != 1 {
		t.Fatalf("listens = %+v", f.listens)
	}
	got := f.listens[0]
	want := listenCall{"argus-tap-tr-abc-3", "orders.events", "order.created", scenario.ListenQueueTTLMillis}
	if got != want {
		t.Errorf("Listen call = %+v, want %+v", got, want)
	}
	if got.ttl != 120000 {
		t.Errorf("ttl = %d, want 120000 (the tap's constant)", got.ttl)
	}
	if q := res.Steps[0].Captured["q"]; q != "argus-tap-tr-abc-3" {
		t.Errorf("saved q = %q, want the generated name", q)
	}
	if !strings.Contains(res.Steps[0].Observed, "broker accepted the listen") {
		t.Errorf("observed = %q", res.Steps[0].Observed)
	}
}

func TestListen_SavedQueueReachesTheConsumeAsTheGeneratedName(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"id":"7"}`)}
	dial, _ := dialerFor(f)
	reg := NewListenRegistry()
	consume := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "${saved.q}", Wait: 90 * time.Second}
	res := Run("tr-abc", []Step{
		AMQPStep("tap", listenSpec(0), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(reg), WithSave(saveQ)),
		AMQPStep("take", consume, scenario.AMQPStepWant{Broker: accepts}, dial),
	})
	if res.Status != "passed" {
		t.Fatalf("chain = %+v", res)
	}
	if len(f.consumes) != 1 || f.consumes[0] != "argus-tap-tr-abc-0" {
		t.Errorf("consume reached the broker with %v, want argus-tap-tr-abc-0", f.consumes)
	}
	if len(f.waits) != 1 || f.waits[0] != 90*time.Second {
		t.Errorf("consume wait = %v, want 90s", f.waits)
	}
}

func TestListen_FailedListenLeavesTheConsumeNotMeasured(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: false, Code: 403, Text: "access refused"}}
	dial, _ := dialerFor(f)
	res := Run("tr-abc", []Step{
		AMQPStep("tap", listenSpec(0), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(NewListenRegistry()), WithSave(saveQ)),
		AMQPStep("take", AMQPSpec{Op: "consume", URLEnv: "X", Queue: "${saved.q}"}, scenario.AMQPStepWant{Broker: accepts}, dial),
	})
	if res.Steps[0].Status != "failed" || !strings.Contains(res.Steps[0].Observed, "refused the listen") {
		t.Errorf("listen step = %+v", res.Steps[0])
	}
	if res.Steps[1].Status != report.StepNotMeasured || len(f.consumes) != 0 {
		t.Errorf("the consume must not run without a queue: %+v consumes=%v", res.Steps[1], f.consumes)
	}
}

func TestListenRegistry_CleanupDeletesEveryListenQueueEvenAfterAFailedChain(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	dial, _ := dialerFor(f)
	reg := NewListenRegistry()
	// the chain FAILS (the consume gets nothing), the registry still knows both queues
	f2 := &fakeBroker{outcome: amqpengine.Outcome{OK: false, Text: "timed out waiting for a message"}}
	dial2 := func(string) (Broker, error) { return f2, nil }
	res := Run("tr-abc", []Step{
		AMQPStep("tap0", listenSpec(0), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(reg), WithSave(map[string]scenario.SaveSpec{"a": {Path: "queue"}})),
		AMQPStep("tap1", listenSpec(1), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(reg), WithSave(map[string]scenario.SaveSpec{"b": {Path: "queue"}})),
		AMQPStep("take", AMQPSpec{Op: "consume", URLEnv: "X", Queue: "${saved.a}"}, scenario.AMQPStepWant{Broker: accepts}, dial2),
	})
	if res.Status == "passed" {
		t.Fatalf("setup: the chain should have failed: %+v", res)
	}
	del := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	deleted := reg.Cleanup(func(string) (Broker, error) { return del, nil })
	if strings.Join(del.deletedQueues, ",") != "argus-tap-tr-abc-0,argus-tap-tr-abc-1" || len(deleted) != 2 {
		t.Errorf("deleted = %v / %v, want both listen queues", del.deletedQueues, deleted)
	}
	if del.closed != 1 {
		t.Errorf("cleanup connection closed %d times, want once", del.closed)
	}
	if again := reg.Cleanup(func(string) (Broker, error) { return del, nil }); len(again) != 0 {
		t.Errorf("a second Cleanup deleted %v, want nothing (idempotent)", again)
	}
}

func TestListenRegistry_ADialErrorAtCleanupIsLoggedNotFatal(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	dial, _ := dialerFor(f)
	reg := NewListenRegistry()
	Run("tr-abc", []Step{AMQPStep("tap", listenSpec(0), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(reg), WithSave(saveQ))})
	deleted := reg.Cleanup(func(string) (Broker, error) { return nil, errors.New("connection refused") })
	if len(deleted) != 0 {
		t.Errorf("deleted = %v, want none (no connection)", deleted)
	}
}

func TestListenRegistry_NothingIsRecordedWhenTheDialFailed(t *testing.T) {
	reg := NewListenRegistry()
	dial := func(string) (Broker, error) { return nil, errors.New("down") }
	res := Run("tr-abc", []Step{AMQPStep("tap", listenSpec(0), scenario.AMQPStepWant{Broker: accepts}, dial, WithListenRegistry(reg), WithSave(saveQ))})
	if res.Status != report.StatusError {
		t.Fatalf("a dial error is an execution error: %+v", res)
	}
	del := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	if got := reg.Cleanup(func(string) (Broker, error) { return del, nil }); len(got) != 0 {
		t.Errorf("cleanup deleted %v for a listen that never reached a broker", got)
	}
}

func TestListen_QueueBindGoesToTheBroker(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	sr := runOne(t, AMQPSpec{Op: "queue_bind", URLEnv: "X", Queue: "mine", Exchange: "x", RoutingKey: "rk"}, accepts, f)
	if sr.Status != "passed" || len(f.binds) != 1 || f.binds[0] != [3]string{"mine", "x", "rk"} {
		t.Errorf("queue_bind = %+v binds=%v", sr, f.binds)
	}
}

func TestListenChain_ArithmeticOfListenHTTPAndAConsumeOf90Seconds(t *testing.T) {
	// The chain backstop is TIMEOUT x step count (runChainScenario): a 3-step chain with the 120 s `## TIMEOUT`
	// ceiling has 360 s. The consume's own wait is 90 s and the engine's watchdog is wait+30 s = 120 s, so the
	// slowest legal chain is: listen (<= 30 s rpc watchdog) + http (<= 30 s client timeout) + consume 90 s.
	const timeout, steps = 120 * time.Second, 3
	backstop := timeout * steps
	worst := 30*time.Second + 30*time.Second + (90*time.Second + 30*time.Second)
	if backstop != 360*time.Second || worst > backstop {
		t.Fatalf("backstop %s, worst case %s: the arithmetic in the docs no longer holds", backstop, worst)
	}
	// and the 120 s wait ceiling on a listen queue plus its 30 s watchdog still fits one step's share
	if scenario.AMQPListenWaitMax+30*time.Second > timeout+30*time.Second {
		t.Errorf("a listen wait of %s exceeds the chain TIMEOUT ceiling %s", scenario.AMQPListenWaitMax, timeout)
	}
}

// ── gzip ────────────────────────────────────────────────────────────────────────────────────────────

func consumeGz(t *testing.T, f *fakeBroker, decode string, want scenario.AMQPStepWant) report.StepResult {
	t.Helper()
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Decode: decode, Wait: time.Second}
	return runOneWant(t, spec, want, f)
}

func bodyEq(field, v string) []mcp.BodyAssert {
	return []mcp.BodyAssert{{Field: field, Op: mcp.BodyEqualsOp, Value: v}}
}

func TestGzip_AutomaticWhenTheDeliveryAnnouncesIt(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: gz(t, `{"id":"abc"}`), consumeEncoding: "gzip"}
	sr := consumeGz(t, f, "", scenario.AMQPStepWant{Broker: accepts, Body: bodyEq("id", "abc")})
	if sr.Status != "passed" {
		t.Errorf("content-encoding: gzip must be gunzipped without `decode`: %+v", sr)
	}
}

func TestGzip_DeclaredWithoutTheHeader(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: gz(t, `{"id":"abc"}`)}
	if sr := consumeGz(t, f, "gzip", scenario.AMQPStepWant{Broker: accepts, Body: bodyEq("id", "abc")}); sr.Status != "passed" {
		t.Errorf("decode: gzip must gunzip a body with no content-encoding: %+v", sr)
	}
	// without either, the compressed bytes are judged as they are (unchanged behaviour)
	if sr := consumeGz(t, f, "", scenario.AMQPStepWant{Broker: accepts, Body: bodyEq("id", "abc")}); sr.Status != "failed" {
		t.Errorf("no decode and no header: the raw gzip bytes cannot satisfy the claim: %+v", sr)
	}
}

func TestGzip_BombEndsTheStepAsErrorNamed(t *testing.T) {
	big := strings.Repeat("a", scenario.AMQPDecodedMaxBytes+1)
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: gz(t, big), consumeEncoding: "gzip"}
	sr := consumeGz(t, f, "", scenario.AMQPStepWant{Broker: accepts})
	if sr.Status != report.StatusError || !strings.Contains(sr.Observed, "decompresses to more than 1048576 bytes") {
		t.Errorf("bomb = %+v, want status error naming the cap", sr)
	}
	exactly := strings.Repeat("a", scenario.AMQPDecodedMaxBytes)
	f2 := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: gz(t, exactly), consumeEncoding: "gzip"}
	if sr := consumeGz(t, f2, "", scenario.AMQPStepWant{Broker: accepts}); sr.Status != "passed" {
		t.Errorf("a body of exactly the cap is allowed: %+v", sr)
	}
}

func TestGzip_GarbageIsItsOwnFailure(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("not gzip at all"), consumeEncoding: "gzip"}
	sr := consumeGz(t, f, "", scenario.AMQPStepWant{Broker: accepts})
	if sr.Status != "failed" || !strings.HasPrefix(sr.Observed, "delivered message is not valid gzip:") {
		t.Errorf("garbage = %+v", sr)
	}
}

func TestGzip_QueueIsEmptyIsNotHiddenByABadBody(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("not gzip"), consumeEncoding: "gzip"}
	sr := consumeGz(t, f, "", scenario.AMQPStepWant{Empty: true})
	if sr.Status != "failed" || !strings.Contains(sr.Observed, "a message arrived") {
		t.Errorf("`queue is empty` must say a message arrived, not a gzip error: %+v", sr)
	}
}

func TestGzip_ThenAvro(t *testing.T) {
	schema := mustPingSchema(t)
	raw, err := avro.Marshal(schema.Avro(), map[string]any{"id": "abc", "seq": int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write(raw)
	_ = w.Close()
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: b.Bytes(), consumeEncoding: "gzip"}
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "lab.ping", ConsumeSchema: schema}
	sr := runOneWant(t, spec, scenario.AMQPStepWant{Broker: accepts, Body: bodyEq("id", "abc")}, f)
	if sr.Status != "passed" {
		t.Errorf("gunzip THEN avro decode must pass: %+v", sr)
	}
	// the other order would fail: avro-decoding the compressed bytes
	f2 := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: b.Bytes()}
	sr = runOneWant(t, spec, scenario.AMQPStepWant{Broker: accepts}, f2)
	if sr.Status != "failed" || !strings.HasPrefix(sr.Observed, "delivered message does not decode as ping:") {
		t.Errorf("without gunzip the compressed bytes do not decode: %+v", sr)
	}
}
