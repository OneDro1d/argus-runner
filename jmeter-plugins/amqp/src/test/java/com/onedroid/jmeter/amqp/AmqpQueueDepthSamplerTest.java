package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.atLeastOnce;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpQueueDepthSampler}.
 *
 * Strategy matches the pattern in {@link AmqpConsumeSamplerTest}: bypass
 * setupTest() and inject a mock {@link Channel} (and when relevant, a mock
 * {@link Connection}) via the package-private test hooks. Tests cover:
 *
 *   - Snapshot success (no thresholds)
 *   - Success with each threshold type met, and with both together
 *   - 412 when min threshold violated
 *   - 412 when max threshold violated
 *   - 404 when the queue does not exist (IOException from passive declare)
 *     and verification that the channel is reopened from the connection
 *   - 500 when the channel is not open (exercises the sampleStart-before-
 *     channel-check fix so timing is well-defined on the failure path)
 *   - 400 when queue_name is missing / blank (broker never contacted)
 *   - Exhaustive behavior of the package-private helpers
 *     ({@code checkThresholds}, {@code parseIntOrUnset}).
 */
class AmqpQueueDepthSamplerTest {

    private AmqpQueueDepthSampler sampler;
    private Channel channel;
    private Connection connection;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpQueueDepthSampler();
        channel = mock(Channel.class);
        connection = mock(Connection.class);
        when(channel.isOpen()).thenReturn(true);
        when(connection.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
        sampler.setConnectionForTesting(connection);
    }

    // --- snapshot success (no thresholds) ---------------------------------

    @Test
    void runTest_returnsMessageCount_onSuccessfulSnapshot() throws Exception {
        AMQP.Queue.DeclareOk ok = declareOk(42, 2);
        when(channel.queueDeclarePassive("q.depth")).thenReturn(ok);

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q.depth")));

        assertTrue(result.isSuccessful(), "expected 200 success");
        assertEquals("200", result.getResponseCode());
        assertArrayEquals("42".getBytes(StandardCharsets.UTF_8), result.getResponseData());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queueName: q.depth"), headers);
        assertTrue(headers.contains("messageCount: 42"), headers);
        assertTrue(headers.contains("consumerCount: 2"), headers);
    }

    // --- thresholds: success cases ----------------------------------------

    @Test
    void runTest_passes_whenDepthAtOrAboveMin() throws Exception {
        when(channel.queueDeclarePassive("q")).thenReturn(declareOk(100, 1));

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q")
                .with("expected_min_depth", "50")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
    }

    @Test
    void runTest_passes_whenDepthAtOrBelowMax() throws Exception {
        when(channel.queueDeclarePassive("q")).thenReturn(declareOk(5, 1));

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q")
                .with("expected_max_depth", "10")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
    }

    @Test
    void runTest_passes_whenDepthWithinBothBounds() throws Exception {
        when(channel.queueDeclarePassive("q")).thenReturn(declareOk(50, 3));

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q")
                .with("expected_min_depth", "10")
                .with("expected_max_depth", "100")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
    }

    // --- thresholds: failure cases ----------------------------------------

    @Test
    void runTest_fails412_whenDepthBelowMin() throws Exception {
        when(channel.queueDeclarePassive("q")).thenReturn(declareOk(5, 0));

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q")
                .with("expected_min_depth", "10")));

        assertFalse(result.isSuccessful());
        assertEquals("412", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("expected_min_depth=10"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("actual=5"),
                result.getResponseMessage());
    }

    @Test
    void runTest_fails412_whenDepthAboveMax() throws Exception {
        when(channel.queueDeclarePassive("q")).thenReturn(declareOk(500, 0));

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q")
                .with("expected_max_depth", "100")));

        assertFalse(result.isSuccessful());
        assertEquals("412", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("expected_max_depth=100"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("actual=500"),
                result.getResponseMessage());
    }

    // --- 404: queue doesn't exist -----------------------------------------

    @Test
    void runTest_fails404_whenQueueMissing_andReopensChannel() throws Exception {
        when(channel.queueDeclarePassive("q.missing"))
                .thenThrow(new IOException("NOT_FOUND - no queue 'q.missing'"));

        // After the 404, the sampler should ask the still-open connection
        // for a fresh channel so subsequent iterations don't silently fail.
        Channel replacement = mock(Channel.class);
        when(replacement.isOpen()).thenReturn(true);
        when(connection.createChannel()).thenReturn(replacement);

        SampleResult result = sampler.runTest(ctx(params()
                .with("queue_name", "q.missing")));

        assertFalse(result.isSuccessful());
        assertEquals("404", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("Queue lookup failed"),
                result.getResponseMessage());
        verify(connection, atLeastOnce()).createChannel();
    }

    // --- 500: channel not open (Howard's sampleStart/End timing nit) ------

    @Test
    void runTest_fails500_whenChannelNotOpen_withWellDefinedTiming() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        SampleResult result = sampler.runTest(ctx(params().with("queue_name", "q")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertNotNull(result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("channel is not open"),
                result.getResponseMessage());

        // The sampleStart-before-channel-check fix means sample timing is
        // defined (>=0 elapsed) even on this early-throw path. Prior to the
        // fix, sampleEnd was called without sampleStart → timing undefined.
        assertTrue(result.getTime() >= 0,
                "sample time must be well-defined on 500 path, was " + result.getTime());

        // Broker was never contacted because channel was closed from the start.
        verify(channel, never()).queueDeclarePassive(anyString());
    }

    // --- 400: queue_name missing ------------------------------------------

    @Test
    void runTest_fails400_whenQueueNameBlank_withoutTouchingBroker() throws Exception {
        SampleResult result = sampler.runTest(ctx(params().with("queue_name", "")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("queue_name is required"),
                result.getResponseMessage());
        verify(channel, never()).queueDeclarePassive(anyString());
    }

    @Test
    void runTest_fails400_whenQueueNameWhitespaceOnly() throws Exception {
        SampleResult result = sampler.runTest(ctx(params().with("queue_name", "   ")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        verify(channel, never()).queueDeclarePassive(anyString());
    }

    // --- helpers: checkThresholds -----------------------------------------

    @Test
    void checkThresholds_returnsNull_whenNeitherThresholdSet() {
        // -1 is the UNSET_THRESHOLD sentinel
        assertNull(AmqpQueueDepthSampler.checkThresholds(0, -1, -1));
        assertNull(AmqpQueueDepthSampler.checkThresholds(999, -1, -1));
    }

    @Test
    void checkThresholds_returnsNull_whenExactlyAtBounds() {
        assertNull(AmqpQueueDepthSampler.checkThresholds(10, 10, -1), "actual == min is OK");
        assertNull(AmqpQueueDepthSampler.checkThresholds(10, -1, 10), "actual == max is OK");
        assertNull(AmqpQueueDepthSampler.checkThresholds(10, 5, 20), "within both");
    }

    @Test
    void checkThresholds_flagsBelowMinBeforeAboveMax() {
        // If both are configured and actual is below min AND above max
        // (nonsensical config but let's be deterministic), min wins.
        String msg = AmqpQueueDepthSampler.checkThresholds(5, 10, 0);
        assertNotNull(msg);
        assertTrue(msg.contains("expected_min_depth=10"), msg);
    }

    @Test
    void checkThresholds_flagsAboveMax_whenAboveConfiguredMax() {
        String msg = AmqpQueueDepthSampler.checkThresholds(500, -1, 100);
        assertNotNull(msg);
        assertTrue(msg.contains("expected_max_depth=100"), msg);
    }

    // --- helpers: parseIntOrUnset -----------------------------------------

    @Test
    void parseIntOrUnset_unsetForBlankOrInvalid() {
        assertEquals(-1, AmqpQueueDepthSampler.parseIntOrUnset(null));
        assertEquals(-1, AmqpQueueDepthSampler.parseIntOrUnset(""));
        assertEquals(-1, AmqpQueueDepthSampler.parseIntOrUnset("   "));
        assertEquals(-1, AmqpQueueDepthSampler.parseIntOrUnset("not-a-number"));
        assertEquals(-1, AmqpQueueDepthSampler.parseIntOrUnset("12.5"));
    }

    @Test
    void parseIntOrUnset_parsesValidIntegers() {
        assertEquals(0, AmqpQueueDepthSampler.parseIntOrUnset("0"));
        assertEquals(42, AmqpQueueDepthSampler.parseIntOrUnset("42"));
        assertEquals(42, AmqpQueueDepthSampler.parseIntOrUnset("  42  "), "trims whitespace");
        assertEquals(-7, AmqpQueueDepthSampler.parseIntOrUnset("-7"), "negative parsed");
    }

    // --- helpers ----------------------------------------------------------

    /**
     * Build a real {@link AMQP.Queue.DeclareOk} using its public Builder.
     * Mockito-mocking it fails because the generated accessors are final;
     * the Builder is the canonical construction path and matches the
     * approach Howard uses for GetResponse in AmqpConsumeSamplerTest.
     */
    private static AMQP.Queue.DeclareOk declareOk(int messageCount, int consumerCount) {
        return new AMQP.Queue.DeclareOk.Builder()
                .queue("q")
                .messageCount(messageCount)
                .consumerCount(consumerCount)
                .build();
    }

    private static ParamBuilder params() {
        return new ParamBuilder();
    }

    private static JavaSamplerContext ctx(ParamBuilder p) {
        Arguments args = new AmqpQueueDepthSampler().getDefaultParameters();
        Map<String, String> overrides = p.build();
        Arguments merged = new Arguments();
        for (int i = 0; i < args.getArgumentCount(); i++) {
            String name = args.getArgument(i).getName();
            String value = overrides.containsKey(name) ? overrides.get(name) : args.getArgument(i).getValue();
            merged.addArgument(name, value);
        }
        for (Map.Entry<String, String> e : overrides.entrySet()) {
            boolean found = false;
            for (int i = 0; i < merged.getArgumentCount(); i++) {
                if (merged.getArgument(i).getName().equals(e.getKey())) {
                    found = true;
                    break;
                }
            }
            if (!found) {
                merged.addArgument(e.getKey(), e.getValue());
            }
        }
        return new JavaSamplerContext(merged);
    }

    private static final class ParamBuilder {
        private final Map<String, String> map = new HashMap<String, String>();

        ParamBuilder with(String name, String value) {
            map.put(name, value);
            return this;
        }

        Map<String, String> build() {
            return map;
        }
    }
}
