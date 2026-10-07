package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * S3 + S4 against a socket that behaves like a broker with a memory alarm (BlockedBrokerFake, the Java
 * port of the Go loopback fake). No broker, no credentials: it ALWAYS runs.
 *
 * The three blocked tests are built so a mutation of one feature is caught by the test that owns it:
 *  - firstPublishAfterAlarmIsNotGreen    owns S3 (the first blocked publish is unannounced; only the
 *                                        confirm that never arrives can fail it). It publishes ONCE and
 *                                        asserts nothing about the listener.
 *  - secondPublishFailsFastNamingTheBlock owns S4. It does not assert on publish #1, and it waits until
 *                                        the fake has announced the block before publish #2, so it does
 *                                        not depend on S3 consuming the timeout.
 */
class AmqpPublishBlockedLoopbackTest {

    private static final int CONFIRM_TIMEOUT_MS = 2000;

    private BlockedBrokerFake broker;
    private AmqpPublishSampler sampler;

    @BeforeEach
    void start() throws Exception {
        broker = new BlockedBrokerFake("low on memory");
        sampler = new AmqpPublishSampler();
        sampler.setupTest(ctx());
    }

    @AfterEach
    void stop() throws Exception {
        long t0 = System.nanoTime();
        sampler.teardownTest(ctx());
        long ms = (System.nanoTime() - t0) / 1_000_000L;
        broker.close();
        // teardown against a broker that never answers close-ok is bounded (CLOSE_TIMEOUT_MS + join slack)
        assertTrue(ms < AmqpPublishSampler.CLOSE_TIMEOUT_MS + 2500L, "teardown took " + ms + "ms");
    }

    private JavaSamplerContext ctx() {
        return TestCtx.params()
                .with("amqp_uri", broker.uri())
                .with("confirm", "each")
                .with("confirm_timeout_ms", String.valueOf(CONFIRM_TIMEOUT_MS))
                .with("label", "loopback-amqp-publish")
                .build();
    }

    @Test
    void firstPublishAfterAlarmIsNotGreen() throws Exception {
        SampleResult r = sampler.runTest(ctx());

        assertFalse(r.isSuccessful(), "a publish the broker never confirmed must not be green: " + r.getResponseMessage());
        assertEquals("504", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("not confirmed within " + CONFIRM_TIMEOUT_MS + "ms"), r.getResponseMessage());
        System.out.println("JTL-responseMessage[confirm-timeout]: " + r.getResponseCode() + " " + r.getResponseMessage());
    }

    @Test
    void secondPublishFailsFastNamingTheBlock() throws Exception {
        sampler.runTest(ctx()); // publish #1: reaches the fake, which then announces the block
        assertTrue(broker.awaitBlockedSent(5000), "the fake never announced the block");
        // the announcement is dispatched on the client's connection thread; give it a moment so this test
        // does not rely on publish #1 having spent the whole confirm timeout
        Thread.sleep(500);

        long t0 = System.nanoTime();
        SampleResult r = sampler.runTest(ctx());
        long ms = (System.nanoTime() - t0) / 1_000_000L;

        assertFalse(r.isSuccessful());
        assertEquals("503", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("blocked by broker: low on memory"), r.getResponseMessage());
        assertTrue(r.getResponseMessage().contains("(since="), r.getResponseMessage());
        assertTrue(ms < CONFIRM_TIMEOUT_MS / 4, "must fail fast, took " + ms + "ms of a " + CONFIRM_TIMEOUT_MS + "ms confirm timeout");
        assertEquals(1, broker.publishes(), "a blocked sample must not publish");
        System.out.println("JTL-responseMessage[blocked]: " + r.getResponseCode() + " " + r.getResponseMessage());
    }

    @Test
    void afterTheUnblockTheNextSampleSucceedsAndNamesTheBlockedWindow() throws Exception {
        sampler.runTest(ctx());
        assertTrue(broker.awaitBlockedSent(5000));
        Thread.sleep(500);
        assertEquals("503", sampler.runTest(ctx()).getResponseCode());

        broker.unblock();
        long deadline = System.currentTimeMillis() + 5000;
        while (sampler.blockStateForTesting().isBlocked() && System.currentTimeMillis() < deadline) {
            Thread.sleep(20);
        }
        assertFalse(sampler.blockStateForTesting().isBlocked(), "the listener never saw connection.unblocked");

        SampleResult ok = sampler.runTest(ctx());
        assertTrue(ok.isSuccessful(), ok.getResponseCode() + " " + ok.getResponseMessage());
        assertEquals("200", ok.getResponseCode());
        assertTrue(ok.getResponseMessage().contains("(was blocked "), "the unblock edge is reported once: " + ok.getResponseMessage());
        assertEquals(1, sampler.blockStateForTesting().periods());
        System.out.println("JTL-responseMessage[after-unblock]: " + ok.getResponseCode() + " " + ok.getResponseMessage());

        SampleResult next = sampler.runTest(ctx());
        assertTrue(next.isSuccessful(), next.getResponseMessage());
        assertFalse(next.getResponseMessage().contains("was blocked"), "the edge is reported once: " + next.getResponseMessage());
    }

    @Test
    void confirmEachSuccessCarriesMicroseconds() throws Exception {
        try (BlockedBrokerFake healthy = new BlockedBrokerFake("unused", false)) {
            AmqpPublishSampler s = new AmqpPublishSampler();
            JavaSamplerContext c = TestCtx.params().with("amqp_uri", healthy.uri()).with("confirm", "each")
                    .with("label", "loopback-amqp-publish").build();
            s.setupTest(c);
            try {
                SampleResult r = s.runTest(c);
                assertTrue(r.isSuccessful(), r.getResponseCode() + " " + r.getResponseMessage());
                assertTrue(r.getResponseMessage().matches("published \\d+B confirm=each us=\\d+"), r.getResponseMessage());
                System.out.println("JTL-responseMessage[confirm-each-success]: " + r.getResponseCode() + " " + r.getResponseMessage());
            } finally {
                s.teardownTest(c);
            }
        }
    }
}
