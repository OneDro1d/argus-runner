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
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyBoolean;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.doAnswer;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpFanoutSampler}.
 *
 * <p>Pattern mirrors {@link AmqpRpcSamplerTest}: bypass setupTest() and
 * inject a mock {@link Channel} via a package-private hook. Tests cover
 * happy path (all queues match), deferred match (later polls), partial
 * success (one queue times out), non-match drain behavior, publish
 * envelope wiring, config validation, and failure paths.
 */
class AmqpFanoutSamplerTest {

    private AmqpFanoutSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpFanoutSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
    }

    // --- happy path ------------------------------------------------------

    @Test
    void runTest_returns200_whenAllQueuesMatchFirstPoll() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        // Each queue returns a matching reply on the first basicGet.
        when(channel.basicGet(eq("q.alpha"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        when(channel.basicGet(eq("q.beta"), eq(true)))
                .thenAnswer(inv -> matchingReply(2L, correlationId.get()));
        when(channel.basicGet(eq("q.gamma"), eq(true)))
                .thenAnswer(inv -> matchingReply(3L, correlationId.get()));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.alpha, q.beta, q.gamma")
                .with("message_body", "hello")
                .with("content_type", "text/plain")
                .with("timeout_ms", "1000")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("3/3 queues matched"),
                result.getResponseMessage());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("correlationId=" + correlationId.get()), headers);
        assertTrue(headers.contains("exchange=example.fanout"), headers);
        assertTrue(headers.contains("queueCount=3"), headers);
        assertTrue(headers.contains("queue.0.name=q.alpha"), headers);
        assertTrue(headers.contains("queue.1.name=q.beta"), headers);
        assertTrue(headers.contains("queue.2.name=q.gamma"), headers);
        assertTrue(headers.contains("queue.0.matched=true"), headers);
        assertTrue(headers.contains("queue.1.matched=true"), headers);
        assertTrue(headers.contains("queue.2.matched=true"), headers);
    }

    @Test
    void runTest_publishesWithCorrectEnvelope_routingKeyIsBlank() throws Exception {
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

        when(channel.basicGet(anyString(), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, capturedProps.get().getCorrelationId()));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.events")
                .with("verify_queues", "q.1")
                .with("message_body", "payload-123")
                .with("content_type", "application/json")
                .with("timeout_ms", "500")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("example.events", capturedExchange.get());
        assertEquals("", capturedRoutingKey.get(), "fanout exchanges ignore routing_key; expect blank");
        assertEquals("payload-123", new String(capturedBody.get(), StandardCharsets.UTF_8));

        AMQP.BasicProperties props = capturedProps.get();
        assertNotNull(props);
        assertNotNull(props.getCorrelationId());
        assertFalse(props.getCorrelationId().isEmpty());
        assertEquals("application/json", props.getContentType());
    }

    // --- deferred match --------------------------------------------------

    @Test
    void runTest_waitsAcrossPollSweeps_untilLaterQueueMatches() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        // Fast queue matches immediately.
        when(channel.basicGet(eq("q.fast"), eq(true)))
                .thenAnswer(inv -> matchingReply(10L, correlationId.get()));

        // Slow queue: null on first 2 sweeps, match on 3rd. At 50ms between
        // sweeps, that's ~100ms elapsed before the match is observed.
        when(channel.basicGet(eq("q.slow"), eq(true)))
                .thenAnswer(new org.mockito.stubbing.Answer<GetResponse>() {
                    private int call = 0;
                    @Override
                    public GetResponse answer(org.mockito.invocation.InvocationOnMock inv) {
                        call++;
                        if (call < 3) return null;
                        return matchingReply(20L, correlationId.get());
                    }
                });

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.fast, q.slow")
                .with("timeout_ms", "2000")));

        assertTrue(result.isSuccessful());
        assertEquals("200", result.getResponseCode());

        // Once q.fast matched (sweep 1), sampler must not poll it again on
        // subsequent sweeps — we only want to see it once.
        verify(channel, times(1)).basicGet(eq("q.fast"), eq(true));
        // q.slow polled at least 3 times (null, null, match).
        verify(channel, times(3)).basicGet(eq("q.slow"), eq(true));

        // Sample time reflects the deferred match delay.
        assertTrue(result.getTime() >= 75,
                "sample time should cover the poll wait, was " + result.getTime() + "ms");
    }

    // --- partial success / timeout --------------------------------------

    @Test
    void runTest_returns408_whenOneQueueNeverMatches() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.ok"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        // q.missing never returns anything.
        when(channel.basicGet(eq("q.missing"), eq(true))).thenReturn(null);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.ok, q.missing")
                .with("timeout_ms", "150")));

        assertFalse(result.isSuccessful());
        assertEquals("408", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("1/2 queues matched"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("missing=[q.missing]"),
                result.getResponseMessage());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queue.0.name=q.ok"), headers);
        assertTrue(headers.contains("queue.0.matched=true"), headers);
        assertTrue(headers.contains("queue.1.name=q.missing"), headers);
        assertTrue(headers.contains("queue.1.matched=false"), headers);
        assertTrue(headers.contains("queue.1.latencyMs=-1"), headers);
    }

    // --- non-match drain -------------------------------------------------

    @Test
    void runTest_drainsNonMatchingMessages_thenMatchesOurs() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        // First two polls return non-matching messages (drained via autoAck),
        // third poll returns our matching reply.
        when(channel.basicGet(eq("q.shared"), eq(true)))
                .thenAnswer(new org.mockito.stubbing.Answer<GetResponse>() {
                    private int call = 0;
                    @Override
                    public GetResponse answer(org.mockito.invocation.InvocationOnMock inv) {
                        call++;
                        if (call == 1) return makeReply(100L, "some-other-id-1", "hello-other-1");
                        if (call == 2) return makeReply(101L, "some-other-id-2", "hello-other-2");
                        return matchingReply(102L, correlationId.get());
                    }
                });

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.shared")
                .with("timeout_ms", "2000")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queue.0.matched=true"), headers);
        assertTrue(headers.contains("queue.0.nonMatchDrained=2"),
                "expected 2 non-match drains before the match, headers=\n" + headers);
    }

    // --- failure paths ---------------------------------------------------

    @Test
    void runTest_returns500_whenChannelNotOpen_withWellDefinedTiming() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("channel is not open"),
                result.getResponseMessage());
        assertTrue(result.getTime() >= 0,
                "sample time must be well-defined on 500 path, was " + result.getTime());

        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_returns500_onBasicPublishFailure() throws Exception {
        doAnswer(inv -> {
            throw new IOException("broker unreachable");
        }).when(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "q.a, q.b")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("broker unreachable"),
                result.getResponseMessage());
    }

    // --- config validation -----------------------------------------------

    @Test
    void runTest_fails400_whenExchangeBlank_withoutTouchingBroker() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "")
                .with("verify_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("exchange is required"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_fails400_whenVerifyQueuesEmpty_withoutTouchingBroker() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("verify_queues is required"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_fails400_whenVerifyQueuesOnlyWhitespaceAndCommas() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.fanout")
                .with("verify_queues", "  ,  ,,")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    // --- parseQueueList --------------------------------------------------

    @Test
    void parseQueueList_trimsTokensAndSkipsEmpty() {
        List<String> parsed = AmqpFanoutSampler.parseQueueList("q.one, q.two ,,q.three,   ");
        assertEquals(Arrays.asList("q.one", "q.two", "q.three"), parsed);
    }

    @Test
    void parseQueueList_emptyInputReturnsEmptyList() {
        assertTrue(AmqpFanoutSampler.parseQueueList("").isEmpty());
        assertTrue(AmqpFanoutSampler.parseQueueList(null).isEmpty());
    }

    // --- helpers --------------------------------------------------------

    private void captureCorrelationIdOnPublish(AtomicReference<String> out) throws IOException {
        doAnswer(inv -> {
            AMQP.BasicProperties props = inv.getArgument(2);
            out.set(props.getCorrelationId());
            return null;
        }).when(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    private static GetResponse matchingReply(long deliveryTag, String correlationId) {
        return makeReply(deliveryTag, correlationId, "matched");
    }

    private static GetResponse makeReply(long deliveryTag, String correlationId, String body) {
        Envelope env = new Envelope(deliveryTag, false, /*exchange*/ "example.fanout",
                /*routingKey*/ "");
        AMQP.BasicProperties.Builder props = new AMQP.BasicProperties.Builder()
                .contentType("text/plain");
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
        Arguments defaults = new AmqpFanoutSampler().getDefaultParameters();
        Map<String, String> overrides = p.build();
        Arguments merged = new Arguments();
        for (int i = 0; i < defaults.getArgumentCount(); i++) {
            String name = defaults.getArgument(i).getName();
            String value = overrides.containsKey(name)
                    ? overrides.get(name)
                    : defaults.getArgument(i).getValue();
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
