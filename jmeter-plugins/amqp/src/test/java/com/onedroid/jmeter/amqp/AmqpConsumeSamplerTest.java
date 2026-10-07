package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Envelope;
import com.rabbitmq.client.GetResponse;
import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.anyBoolean;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.atLeastOnce;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpConsumeSampler}.
 *
 * Strategy: the sampler's setupTest() opens a real AMQP Connection/Channel which
 * is impractical to mock end-to-end, so these tests bypass setupTest() and inject
 * a mock Channel directly via the package-private {@code setChannelForTesting()}
 * hook. This is the tightest scope that still exercises the full runTest() logic
 * including the poll loop, filter predicates, ack/nack paths, and SampleResult
 * wiring.
 */
class AmqpConsumeSamplerTest {

    private AmqpConsumeSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpConsumeSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
    }

    // --- #1 successful consume (no filter) ---------------------------------

    @Test
    void runTest_returnsMessageBody_onSuccessfulConsume_autoAckTrue() throws Exception {
        GetResponse response = buildResponse(
                /*deliveryTag*/ 7L,
                /*routingKey*/ "example.incoming",
                /*exchange*/ "example",
                /*correlationId*/ "abc-123",
                /*contentType*/ "application/json",
                /*body*/ "{\"ok\":true}");

        when(channel.basicGet(eq("q.in"), anyBoolean())).thenReturn(response);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q.in")
                .with("auto_ack", "true")
                .with("timeout_ms", "1000"));

        SampleResult result = sampler.runTest(ctx);

        assertTrue(result.isSuccessful(), "expected success");
        assertEquals("200", result.getResponseCode());
        assertArrayEquals("{\"ok\":true}".getBytes(StandardCharsets.UTF_8), result.getResponseData());
        assertEquals("application/json", result.getContentType());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("deliveryTag=7"), headers);
        assertTrue(headers.contains("routingKey=example.incoming"), headers);
        assertTrue(headers.contains("correlationId=abc-123"), headers);
        assertTrue(headers.contains("exchange=example"), headers);

        // auto_ack=true → basicGet called with autoAck=true, no explicit ack/nack
        verify(channel).basicGet("q.in", true);
        verify(channel, never()).basicAck(anyLong(), anyBoolean());
        verify(channel, never()).basicNack(anyLong(), anyBoolean(), anyBoolean());
    }

    @Test
    void runTest_emitsExplicitAck_whenAutoAckFalse_andNoFilter() throws Exception {
        GetResponse response = buildResponse(11L, "k", "", null, "text/plain", "hi");
        when(channel.basicGet(eq("q"), anyBoolean())).thenReturn(response);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q")
                .with("auto_ack", "false")
                .with("timeout_ms", "500"));

        SampleResult result = sampler.runTest(ctx);

        assertTrue(result.isSuccessful());
        verify(channel).basicGet("q", false);
        verify(channel).basicAck(11L, false);
    }

    // --- #2 empty queue timeout --------------------------------------------

    @Test
    void runTest_returns408_whenQueueEmptyForTimeoutWindow() throws Exception {
        when(channel.basicGet(eq("q.empty"), anyBoolean())).thenReturn(null);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q.empty")
                // short timeout so the test stays fast
                .with("timeout_ms", "150"));

        long start = System.currentTimeMillis();
        SampleResult result = sampler.runTest(ctx);
        long elapsed = System.currentTimeMillis() - start;

        assertFalse(result.isSuccessful());
        assertEquals("408", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("no message"), result.getResponseMessage());
        assertTrue(elapsed >= 150, "should have waited at least the timeout budget, was " + elapsed + "ms");
        // polled multiple times during the wait
        verify(channel, atLeastOnce()).basicGet(eq("q.empty"), anyBoolean());
    }

    // --- #3 correlationId matching -----------------------------------------

    @Test
    void runTest_pollsPastNonMatchingMessages_andAcksMatchingOne() throws Exception {
        GetResponse wrong = buildResponse(1L, "k", "", "other-id", "application/json", "nope");
        GetResponse right = buildResponse(2L, "k", "", "target-id", "application/json", "yes");

        // Serve wrong first, then right.
        when(channel.basicGet(eq("q"), anyBoolean())).thenReturn(wrong, right);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q")
                .with("auto_ack", "true") // filtering forces manual ack regardless
                .with("expected_correlation_id", "target-id")
                .with("timeout_ms", "2000"));

        SampleResult result = sampler.runTest(ctx);

        assertTrue(result.isSuccessful());
        assertArrayEquals("yes".getBytes(StandardCharsets.UTF_8), result.getResponseData());

        // Filter mode forces internal autoAck=false so we can requeue non-matches.
        verify(channel, times(2)).basicGet("q", false);
        // Matching message gets acked
        verify(channel).basicAck(2L, false);
        // Non-matching message gets requeued (not discarded)
        verify(channel).basicNack(1L, false, true);
    }

    @Test
    void runTest_returns408_whenCorrelationIdNeverMatchesWithinTimeout() throws Exception {
        GetResponse wrong = buildResponse(1L, "k", "", "other-id", "application/json", "nope");
        // Always returns the wrong one; deadline is the only exit.
        when(channel.basicGet(eq("q"), anyBoolean())).thenReturn(wrong, (GetResponse) null);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q")
                .with("expected_correlation_id", "never-arriving")
                .with("timeout_ms", "150"));

        SampleResult result = sampler.runTest(ctx);

        assertFalse(result.isSuccessful());
        assertEquals("408", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("no matching"), result.getResponseMessage());
        // Non-matching message was requeued on exit
        verify(channel, atLeastOnce()).basicNack(anyLong(), eq(false), eq(true));
    }

    // --- routing key filter (additional; same filter path) -----------------

    @Test
    void runTest_filtersByRoutingKey_whenSet() throws Exception {
        GetResponse wrongKey = buildResponse(1L, "k.wrong", "", null, "text/plain", "a");
        GetResponse rightKey = buildResponse(2L, "k.right", "", null, "text/plain", "b");
        when(channel.basicGet(eq("q"), anyBoolean())).thenReturn(wrongKey, rightKey);

        JavaSamplerContext ctx = ctx(params()
                .with("queue_name", "q")
                .with("expected_routing_key", "k.right")
                .with("timeout_ms", "500"));

        SampleResult result = sampler.runTest(ctx);

        assertTrue(result.isSuccessful());
        assertArrayEquals("b".getBytes(StandardCharsets.UTF_8), result.getResponseData());
        verify(channel).basicAck(2L, false);
        verify(channel).basicNack(1L, false, true);
    }

    // --- setup failure path -------------------------------------------------

    @Test
    void runTest_returns500_whenChannelNotOpen() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        JavaSamplerContext ctx = ctx(params().with("queue_name", "q"));

        SampleResult result = sampler.runTest(ctx);

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertNotNull(result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("channel is not open"), result.getResponseMessage());
    }

    // --- validation --------------------------------------------------------

    @Test
    void runTest_returns400_whenQueueNameBlank_withoutTouchingChannel() throws Exception {
        JavaSamplerContext ctx = ctx(params().with("queue_name", "   "));

        SampleResult result = sampler.runTest(ctx);

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("queue_name is required"), result.getResponseMessage());
        // No AMQP calls should have been made for a config error.
        verify(channel, never()).basicGet(anyString(), anyBoolean());
        verify(channel, never()).basicAck(anyLong(), anyBoolean());
        verify(channel, never()).basicNack(anyLong(), anyBoolean(), anyBoolean());
    }

    // --- helpers -----------------------------------------------------------

    private static GetResponse buildResponse(long deliveryTag,
                                             String routingKey,
                                             String exchange,
                                             String correlationId,
                                             String contentType,
                                             String body) {
        Envelope env = new Envelope(deliveryTag, false, exchange, routingKey);
        AMQP.BasicProperties.Builder props = new AMQP.BasicProperties.Builder()
                .contentType(contentType);
        if (correlationId != null) {
            props.correlationId(correlationId);
        }
        byte[] bodyBytes = body == null ? new byte[0] : body.getBytes(StandardCharsets.UTF_8);
        return new GetResponse(env, props.build(), bodyBytes, /*messageCount*/ 0);
    }

    private static ParamBuilder params() {
        return new ParamBuilder();
    }

    private static JavaSamplerContext ctx(ParamBuilder p) {
        Arguments args = new AmqpConsumeSampler().getDefaultParameters();
        // Override defaults with test-specific values.
        Map<String, String> overrides = p.build();
        Arguments merged = new Arguments();
        // Apply defaults first
        for (int i = 0; i < args.getArgumentCount(); i++) {
            String name = args.getArgument(i).getName();
            String value = overrides.containsKey(name) ? overrides.get(name) : args.getArgument(i).getValue();
            merged.addArgument(name, value);
        }
        // Also include any overrides not in the defaults set
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
