package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;

import java.util.UUID;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * S3 + S4 + S10 against the mission's own throwaway RabbitMQ (namespace argus-amqp-test, reached through a
 * loopback port-forward). Excluded from the default run; {@code mvn -Plive test} selects ONLY these and sets
 * ARGUS_REQUIRE_AMQP=1 so a missing broker FAILS (see {@link AmqpLiveHarness} for every rule).
 *
 * Nothing here creates a queue or an exchange: publishes go to the default exchange with a routing key no
 * queue has, so the broker drops them and leaves no residue on the shared broker.
 */
@Tag("live")
class AmqpPublishLiveTest {

    private static final int CONFIRM_TIMEOUT_MS = 3000;
    private static AmqpLiveHarness broker;

    @BeforeAll
    static void connect() throws Exception {
        broker = AmqpLiveHarness.acquire();
        // start from a known state: a leftover alarm from an earlier crashed run would blind every test
        if (broker.memAlarm()) {
            broker.alarmOff();
        }
    }

    @AfterAll
    static void disconnect() throws Exception {
        if (broker != null) {
            try {
                broker.alarmOff(); // safety: whatever happened above, hand the broker back with the alarm OFF
            } finally {
                broker.close();
            }
        }
    }

    private static JavaSamplerContext ctx(String confirm, String label) {
        return TestCtx.params()
                .with("amqp_uri", broker.samplerUri())
                .with("exchange", "")
                .with("routing_key", "argus-b-live-" + UUID.randomUUID() + "-nobody-binds-this")
                .with("confirm", confirm)
                .with("confirm_timeout_ms", String.valueOf(CONFIRM_TIMEOUT_MS))
                .with("label", label)
                .build();
    }

    private static void awaitBlocked(AmqpPublishSampler s, long ms) throws Exception {
        long end = System.currentTimeMillis() + ms;
        while (!s.blockStateForTesting().isBlocked() && System.currentTimeMillis() < end) {
            Thread.sleep(50);
        }
    }

    @Test
    void confirmEachAgainstAHealthyBrokerCarriesMicroseconds() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("each", "live-amqp-publish");
        s.setupTest(c);
        try {
            SampleResult r = s.runTest(c);
            assertTrue(r.isSuccessful(), r.getResponseCode() + " " + r.getResponseMessage());
            assertTrue(r.getResponseMessage().matches("published \\d+B confirm=each us=\\d+"), r.getResponseMessage());
            System.out.println("JTL-responseMessage[live-confirm-each-success]: " + r.getResponseCode() + " " + r.getResponseMessage());

            JavaSamplerContext b = ctx("batch:3", "live-amqp-publish");
            SampleResult b1 = s.runTest(b);
            SampleResult b2 = s.runTest(b);
            SampleResult b3 = s.runTest(b);
            assertTrue(b1.isSuccessful() && b2.isSuccessful(), b1.getResponseMessage() + " / " + b2.getResponseMessage());
            assertTrue(b3.isSuccessful(), b3.getResponseCode() + " " + b3.getResponseMessage());
            assertTrue(b3.getResponseMessage().contains("batch-confirm us="), b3.getResponseMessage());
        } finally {
            s.teardownTest(c);
        }
    }

    /** S3 owns this one: ONE publish, nothing about the listener. */
    @Test
    void firstPublishAfterAlarmIsNotGreen() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("each", "live-amqp-publish");
        s.setupTest(c); // connect while the broker is healthy; the alarm comes after
        try {
            broker.alarmOn();
            SampleResult r = s.runTest(c);
            assertFalse(r.isSuccessful(), "a publish the blocked broker never confirmed must not be green: " + r.getResponseMessage());
            assertEquals("504", r.getResponseCode());
            assertTrue(r.getResponseMessage().startsWith("not confirmed within " + CONFIRM_TIMEOUT_MS + "ms"), r.getResponseMessage());
            System.out.println("JTL-responseMessage[live-confirm-timeout]: " + r.getResponseCode() + " " + r.getResponseMessage());
        } finally {
            broker.alarmOff();
            s.teardownTest(c);
        }
    }

    /** S4 owns this one: it does not assert on publish #1, and waits for the announcement before #2. */
    @Test
    void secondPublishFailsFastNamingTheBlock() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("each", "live-amqp-publish");
        s.setupTest(c);
        try {
            broker.alarmOn();
            s.runTest(c); // publish #1: reaches the broker, which then announces connection.blocked
            awaitBlocked(s, 5000); // no assertion: with the listener removed this simply times out

            long t0 = System.nanoTime();
            SampleResult r = s.runTest(c);
            long ms = (System.nanoTime() - t0) / 1_000_000L;

            assertFalse(r.isSuccessful());
            assertEquals("503", r.getResponseCode());
            assertTrue(r.getResponseMessage().startsWith("blocked by broker: "), r.getResponseMessage());
            assertTrue(r.getResponseMessage().contains("(since="), r.getResponseMessage());
            assertTrue(ms < CONFIRM_TIMEOUT_MS / 4, "must fail fast, took " + ms + "ms of a " + CONFIRM_TIMEOUT_MS + "ms confirm timeout");
            System.out.println("JTL-responseMessage[live-blocked]: " + r.getResponseCode() + " " + r.getResponseMessage());
        } finally {
            broker.alarmOff();
            s.teardownTest(c);
        }
    }

    /** The unblock edge is reported once, on the first success after the alarm clears. */
    @Test
    void afterTheAlarmClearsTheNextSampleSucceedsAndNamesTheBlockedWindow() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("each", "live-amqp-publish");
        s.setupTest(c);
        try {
            try {
                broker.alarmOn();
                s.runTest(c);
                awaitBlocked(s, 5000);
            } finally {
                broker.alarmOff();
            }
            long end = System.currentTimeMillis() + 10000;
            while (s.blockStateForTesting().isBlocked() && System.currentTimeMillis() < end) {
                Thread.sleep(50);
            }
            assertFalse(s.blockStateForTesting().isBlocked(), "the listener never saw connection.unblocked");
            SampleResult ok = s.runTest(c);
            assertTrue(ok.isSuccessful(), ok.getResponseCode() + " " + ok.getResponseMessage());
            assertTrue(ok.getResponseMessage().contains("(was blocked "), ok.getResponseMessage());
            System.out.println("JTL-responseMessage[live-after-unblock]: " + ok.getResponseCode() + " " + ok.getResponseMessage());
        } finally {
            s.teardownTest(c);
        }
    }

    /**
     * NEGATIVE CONTROL: with confirm=off a publish to a blocked broker is green. This is the blindness S3
     * exists to remove; if someone makes `off` fail, this test says so and the design should be re-read.
     */
    @Test
    void plainPublishToABlockedBrokerIsGreenWithConfirmOff() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("off", "live-amqp-publish");
        s.setupTest(c);
        try {
            broker.alarmOn();
            SampleResult r = s.runTest(c);
            assertTrue(r.isSuccessful(), "documented blindness: an unconfirmed hand-off to a blocked broker is green; got "
                    + r.getResponseCode() + " " + r.getResponseMessage());
            assertEquals("200", r.getResponseCode());
        } finally {
            broker.alarmOff();
            s.teardownTest(c);
        }
    }

    /** S10 on a REAL refusal: a wrong password makes the broker answer ACCESS_REFUSED. */
    @Test
    void authFailureNeverEchoesPassword() throws Exception {
        String wrong = "wrong-" + UUID.randomUUID();
        String uri = "amqp://" + broker.user() + ":" + wrong + "@" + broker.hostPort();
        // control: the wrong password is in the sampler's input
        assertTrue(uri.contains(wrong), "CONTROL: the secret must be in the input");

        try (LogCapture log = new LogCapture()) {
            AmqpPublishSampler s = new AmqpPublishSampler();
            JavaSamplerContext c = TestCtx.params().with("amqp_uri", uri).with("confirm", "each").with("label", "live-amqp-publish").build();
            s.setupTest(c);
            SampleResult r = s.runTest(c);

            assertFalse(r.isSuccessful());
            assertTrue(log.lines().size() > 0, "the refused connect must have been logged (else the log assertion is vacuous)");
            assertTrue(r.getResponseMessage().contains("ACCESS_REFUSED"), "expected the broker's own refusal in: " + r.getResponseMessage());
            String everything = r.getResponseMessage() + "\n" + r.getResponseDataAsString() + "\n"
                    + r.getFirstAssertionFailureMessage() + "\n" + log.text();
            for (String form : new String[] {wrong, Redact.urlEncode(wrong),
                    java.util.Base64.getEncoder().encodeToString((broker.user() + ":" + wrong).getBytes(java.nio.charset.StandardCharsets.UTF_8))}) {
                assertFalse(everything.contains(form), "the wrong password (or an encoding of it) leaked into the output");
            }
        }
    }
}
