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

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyBoolean;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.atLeast;
import static org.mockito.Mockito.doAnswer;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpRpcSampler}.
 *
 * Pattern follows {@link AmqpConsumeSamplerTest} and
 * {@link AmqpQueueDepthSamplerTest}: bypass setupTest() and inject a mock
 * {@link Channel} + a fixed reply-queue name via package-private hooks.
 *
 * Covered scenarios:
 *   - Happy path: publish + matching reply → 200 with body + correlationId
 *     preserved in responseHeaders; verify basicAck fired
 *   - Publish envelope wiring: exchange, routing_key, replyTo, correlationId,
 *     contentType all round-trip correctly
 *   - Stale reply (different correlationId) discarded, matching reply then
 *     consumed — verifies ack+discard of non-match path
 *   - Timeout (null GetResponse) → 408 with correlationId in message
 *   - Channel not open → 500 with well-defined timing (CAT-2614 pattern
 *     applied here too)
 *   - Missing routing_key → 400 without touching broker
 *   - Round-trip latency: sample time reflects the time between publish and
 *     matching reply (not just internal setup)
 */
class AmqpRpcSamplerTest {

    private static final String REPLY_QUEUE = "amq.gen-reply-queue-abc123";

    private AmqpRpcSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpRpcSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
        sampler.setReplyQueueNameForTesting(REPLY_QUEUE);
    }

    // --- happy path ------------------------------------------------------

    @Test
    void runTest_returnsReplyBody_onMatchingCorrelationId() throws Exception {
        // Capture the correlationId from basicPublish so we can mint a
        // matching reply.
        AtomicReference<String> publishedCorrelationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(publishedCorrelationId);

        // When basicGet is polled, return a reply with the same correlationId.
        when(channel.basicGet(eq(REPLY_QUEUE), eq(false)))
                .thenAnswer(inv -> makeReply(
                        /*deliveryTag*/ 1L,
                        /*correlationId*/ publishedCorrelationId.get(),
                        /*contentType*/ "application/json",
                        /*body*/ "{\"ok\":true}"));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.rpc")
                .with("routing_key", "rpc.request")
                .with("message_body", "{\"q\":\"ping\"}")
                .with("content_type", "application/json")
                .with("timeout_ms", "500")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
        assertArrayEquals("{\"ok\":true}".getBytes(StandardCharsets.UTF_8), result.getResponseData());
        assertEquals("application/json", result.getContentType());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("requestCorrelationId=" + publishedCorrelationId.get()), headers);
        assertTrue(headers.contains("deliveryTag=1"), headers);

        // Reply was ack'd
        verify(channel).basicAck(1L, false);
        // No non-match discard happened
        verify(channel, times(1)).basicAck(anyLong(), anyBoolean());
    }

    @Test
    void runTest_publishesWithCorrectEnvelope() throws Exception {
        // Capture full BasicProperties for inspection.
        AtomicReference<AMQP.BasicProperties> capturedProps = new AtomicReference<>();
        AtomicReference<String> capturedExchange = new AtomicReference<>();
        AtomicReference<String> capturedRoutingKey = new AtomicReference<>();
        AtomicReference<byte[]> capturedBody = new AtomicReference<>();

        doAnswer(inv -> {
            capturedExchange.set(inv.getArgument(0));
            capturedRoutingKey.set(inv.getArgument(1));
            capturedProps.set(inv.getArgument(2));
            capturedBody.set(inv.getArgument(3));
            return null;
        }).when(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));

        // Return a matching reply so runTest finishes quickly.
        when(channel.basicGet(eq(REPLY_QUEUE), eq(false)))
                .thenAnswer(inv -> makeReply(1L, capturedProps.get().getCorrelationId(),
                        "application/json", "ok"));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.rpc")
                .with("routing_key", "rpc.echo")
                .with("message_body", "hello")
                .with("content_type", "text/plain")
                .with("timeout_ms", "500")));

        assertTrue(result.isSuccessful());
        assertEquals("example.rpc", capturedExchange.get());
        assertEquals("rpc.echo", capturedRoutingKey.get());
        assertArrayEquals("hello".getBytes(StandardCharsets.UTF_8), capturedBody.get());

        AMQP.BasicProperties props = capturedProps.get();
        assertNotNull(props);
        assertEquals(REPLY_QUEUE, props.getReplyTo(), "replyTo should be the declared reply queue");
        assertEquals("text/plain", props.getContentType());
        assertNotNull(props.getCorrelationId(), "correlationId must be set");
        assertFalse(props.getCorrelationId().isEmpty());
    }

    // --- stale-reply discard --------------------------------------------

    @Test
    void runTest_discardsStaleReply_thenConsumesMatchingReply() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        // First poll: a reply from a prior iteration (different correlationId).
        // Second poll: the matching reply for this request.
        when(channel.basicGet(eq(REPLY_QUEUE), eq(false)))
                .thenAnswer(new org.mockito.stubbing.Answer<GetResponse>() {
                    private int call = 0;
                    @Override
                    public GetResponse answer(org.mockito.invocation.InvocationOnMock inv) {
                        call++;
                        if (call == 1) {
                            return makeReply(100L, "stale-old-id", "text/plain", "stale");
                        }
                        return makeReply(200L, correlationId.get(), "text/plain", "fresh");
                    }
                });

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "rpc.request")
                .with("timeout_ms", "500")));

        assertTrue(result.isSuccessful());
        assertArrayEquals("fresh".getBytes(StandardCharsets.UTF_8), result.getResponseData());

        // Stale message ack'd (discarded)
        verify(channel).basicAck(100L, false);
        // Matching message ack'd
        verify(channel).basicAck(200L, false);
    }

    // --- timeout ---------------------------------------------------------

    @Test
    void runTest_returns408_whenReplyNeverArrives() throws Exception {
        // Queue always empty → runTest polls until deadline.
        when(channel.basicGet(eq(REPLY_QUEUE), eq(false))).thenReturn(null);

        long start = System.currentTimeMillis();
        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "rpc.request")
                // Short timeout keeps the test fast.
                .with("timeout_ms", "150")));
        long elapsed = System.currentTimeMillis() - start;

        assertFalse(result.isSuccessful());
        assertEquals("408", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("RPC timeout"), result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("correlationId="), result.getResponseMessage());
        assertTrue(elapsed >= 150, "should have waited at least the timeout budget, was " + elapsed + "ms");

        // Publish still happened once.
        verify(channel, times(1)).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        // We polled multiple times during the wait.
        verify(channel, atLeast(2)).basicGet(eq(REPLY_QUEUE), eq(false));
    }

    @Test
    void runTest_sampleTimeReflectsRoundTripLatency() throws Exception {
        // Simulate a ~200ms round trip by returning null on the first poll,
        // then a matching reply after the delay.
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq(REPLY_QUEUE), eq(false)))
                .thenAnswer(new org.mockito.stubbing.Answer<GetResponse>() {
                    private int call = 0;
                    @Override
                    public GetResponse answer(org.mockito.invocation.InvocationOnMock inv) throws Throwable {
                        call++;
                        if (call < 4) {
                            return null; // 3 polls * 50ms = ~150ms idle
                        }
                        return makeReply(1L, correlationId.get(), "text/plain", "reply");
                    }
                });

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "rpc.request")
                .with("timeout_ms", "2000")));

        assertTrue(result.isSuccessful());
        assertTrue(result.getTime() >= 100,
                "sample time should cover publish + poll wait; was " + result.getTime() + "ms");
    }

    // --- failure paths --------------------------------------------------

    @Test
    void runTest_returns500_whenChannelNotOpen_withWellDefinedTiming() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        SampleResult result = sampler.runTest(ctx(params().with("routing_key", "rpc.request")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("channel is not open"),
                result.getResponseMessage());

        // sampleStart-before-channel-check fix: timing is well-defined.
        assertTrue(result.getTime() >= 0,
                "sample time must be well-defined on 500 path, was " + result.getTime());

        // Nothing was published or polled.
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_returns500_whenReplyQueueNotDeclared() throws Exception {
        sampler.setReplyQueueNameForTesting(null);

        SampleResult result = sampler.runTest(ctx(params().with("routing_key", "rpc.request")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("Reply queue was not declared"),
                result.getResponseMessage());
    }

    @Test
    void runTest_returns500_onBasicPublishFailure() throws Exception {
        doAnswer(inv -> {
            throw new IOException("connection reset");
        }).when(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));

        SampleResult result = sampler.runTest(ctx(params().with("routing_key", "rpc.request")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("connection reset"),
                result.getResponseMessage());
    }

    // --- validation: missing routing_key --------------------------------

    @Test
    void runTest_fails400_whenRoutingKeyBlank_withoutTouchingBroker() throws Exception {
        SampleResult result = sampler.runTest(ctx(params().with("routing_key", "")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("routing_key is required"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_fails400_whenRoutingKeyWhitespaceOnly() throws Exception {
        SampleResult result = sampler.runTest(ctx(params().with("routing_key", "   ")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    // --- helpers --------------------------------------------------------

    private void captureCorrelationIdOnPublish(AtomicReference<String> out) throws IOException {
        doAnswer(inv -> {
            AMQP.BasicProperties props = inv.getArgument(2);
            out.set(props.getCorrelationId());
            return null;
        }).when(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    private static GetResponse makeReply(long deliveryTag,
                                          String correlationId,
                                          String contentType,
                                          String body) {
        Envelope env = new Envelope(deliveryTag, false, /*exchange*/ "",
                /*routingKey*/ "amq.gen-reply-queue-abc123");
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
        Arguments args = new AmqpRpcSampler().getDefaultParameters();
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
