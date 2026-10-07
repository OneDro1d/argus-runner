package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * a thread inside {@code basicPublish} against a broker that blocked its connection and
 * stopped reading it must come back. JMeter does not interrupt a sampler mid-sample, so a sample that never
 * returns holds the thread, the test and (through its non-daemon threads) the JVM past the step's end.
 *
 * The session connection writes through the client's NIO write queue; when the peer stops reading it fills.
 * MEASURED while diagnosing (a thread dump of the stuck publisher: ArrayBlockingQueue.offer inside
 * SocketChannelFrameHandlerState.sendWriteRequest): the client already bounds that wait itself (its default
 * write-enqueuing timeout, 10 s), so a publish is NOT what holds the JVM. These two tests do not go red on
 * origin/dev; they pin the property (and the JTL shape of a blocked publish) so a later change cannot lose it.
 * Loopback only ({@link DeafBrokerFake}).
 */
class AmqpPublishDeafPeerTest {

    /** The client's write-queue wait is 10 s; the rest is the time to fill the queue, and slack. */
    private static final long RETURN_BOUND_MS = 25_000L;

    private DeafBrokerFake broker;

    @AfterEach
    void stop() {
        SessionRegistry.closeAll();
        if (broker != null) {
            broker.close();
        }
    }

    /** Publishes 64 KB unconfirmed messages in a thread until one is not successful; returns that sample. */
    private SampleResult floodUntilTheFirstFailure() throws Exception {
        AmqpSessionSetupSampler setup = new AmqpSessionSetupSampler();
        SampleResult up = setup.runTest(TestCtx.params().with("amqp_uri", broker.uri()).with("session", "dp-1")
                .with("queue", "q-dp-1").with("routing_key", "rk-dp-1").buildFor(setup.getDefaultParameters()));
        assertTrue(up.isSuccessful(), up.getResponseMessage());
        final AmqpPublishSampler pub = new AmqpPublishSampler();
        final JavaSamplerContext c = TestCtx.params().with("amqp_uri", broker.uri()).with("session", "dp-1")
                .with("confirm", "off").with("message_size_bytes", "65536").build();
        final AtomicReference<SampleResult> failed = new AtomicReference<SampleResult>();
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                for (int i = 0; i < 1_000_000 && failed.get() == null; i++) {
                    SampleResult r = pub.runTest(c);
                    if (!r.isSuccessful()) {
                        failed.set(r);
                    }
                }
            }
        }, "deaf-peer-publisher");
        t.setDaemon(true);
        t.start();
        t.join(RETURN_BOUND_MS);
        StringBuilder where = new StringBuilder();
        if (t.isAlive()) {
            for (StackTraceElement e : t.getStackTrace()) {
                where.append("\n    ").append(e);
            }
        }
        assertFalse(t.isAlive(), "a publish to a peer that stopped reading did not return within " + RETURN_BOUND_MS
                + "ms; the thread is parked at:" + where);
        assertNotNull(failed.get());
        return failed.get();
    }

    @Test
    void aPublishToAPeerThatStoppedReadingReturnsInsteadOfHoldingTheThread() throws Exception {
        broker = new DeafBrokerFake("never announced", false);
        SampleResult r = floodUntilTheFirstFailure();
        System.out.println("JTL-responseMessage[deaf-peer, no announcement]: " + r.getResponseCode() + " " + r.getResponseMessage());

        assertFalse(r.isSuccessful());
        assertEquals("500", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("Publish failed: "), r.getResponseMessage());
        assertFalse(r.getResponseMessage().contains("guest:guest"), "no credential in a response message");
    }

    @Test
    void whenTheBrokerAnnouncedTheBlockTheStuckPublishIsReportedAsBlocked() throws Exception {
        broker = new DeafBrokerFake("low on memory", true);
        SampleResult r = floodUntilTheFirstFailure();
        System.out.println("JTL-responseMessage[deaf-peer, announced]: " + r.getResponseCode() + " " + r.getResponseMessage());

        assertFalse(r.isSuccessful());
        assertEquals("503", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("blocked by broker: low on memory"), r.getResponseMessage());
        assertTrue(r.getResponseMessage().contains("(since="), r.getResponseMessage());
    }
}
