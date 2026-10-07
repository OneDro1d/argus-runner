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
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpPubSubSampler}.
 *
 * <p>Pattern mirrors {@link AmqpFanoutSamplerTest}: bypass setupTest() and
 * inject a mock {@link Channel} via a package-private hook. Tests cover
 * happy path (all match, none no-match), routing-violation (409 on
 * no-match queue receiving), missing-match (408 on match queue timing
 * out), both-failures-409-wins, settle window for negative verification,
 * publish envelope wiring, config validation (blank exchange / blank
 * routing_key / both-lists-empty), non-match drain, channel-not-open, and
 * publish IOException paths.
 */
class AmqpPubSubSamplerTest {

    private AmqpPubSubSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpPubSubSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
    }

    // --- happy path -----------------------------------------------------

    @Test
    void runTest_returns200_whenAllMatchReceiveAndNoMatchStayEmpty() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.match-1"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        when(channel.basicGet(eq("q.match-2"), eq(true)))
                .thenAnswer(inv -> matchingReply(2L, correlationId.get()));
        // No-match queues always empty — correct broker routing.
        when(channel.basicGet(eq("q.nomatch-1"), eq(true))).thenReturn(null);
        when(channel.basicGet(eq("q.nomatch-2"), eq(true))).thenReturn(null);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.service-a.info")
                .with("match_queues", "q.match-1, q.match-2")
                .with("no_match_queues", "q.nomatch-1, q.nomatch-2")
                .with("timeout_ms", "1000")
                .with("settle_ms", "100")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("2 match queues received"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("2 no-match queues stayed empty"),
                result.getResponseMessage());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("matchCount=2"), headers);
        assertTrue(headers.contains("noMatchCount=2"), headers);
        assertTrue(headers.contains("queue.0.role=match"), headers);
        assertTrue(headers.contains("queue.0.passed=true"), headers);
        assertTrue(headers.contains("queue.2.role=no_match"), headers);
        assertTrue(headers.contains("queue.2.received=false"), headers);
        assertTrue(headers.contains("queue.2.passed=true"), headers);
    }

    @Test
    void runTest_publishesWithConcreteRoutingKey() throws Exception {
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

        when(channel.basicGet(eq("q.hit"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, capturedProps.get().getCorrelationId()));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.audit.critical")
                .with("match_queues", "q.hit")
                .with("message_body", "payload")
                .with("content_type", "application/json")
                .with("timeout_ms", "500")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("example.topic", capturedExchange.get());
        assertEquals("logs.audit.critical", capturedRoutingKey.get(),
                "topic exchanges require a concrete routing_key at publish time");
        assertEquals("payload", new String(capturedBody.get(), StandardCharsets.UTF_8));

        AMQP.BasicProperties props = capturedProps.get();
        assertNotNull(props);
        assertNotNull(props.getCorrelationId());
        assertFalse(props.getCorrelationId().isEmpty());
        assertEquals("application/json", props.getContentType());
    }

    // --- routing violation (409) ----------------------------------------

    @Test
    void runTest_returns409_whenNoMatchQueueReceivesMessage() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.match"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        // Routing bug: q.shouldnt-match is wired with a pattern that
        // accidentally matches — broker sends it the message.
        when(channel.basicGet(eq("q.shouldnt-match"), eq(true)))
                .thenAnswer(inv -> matchingReply(99L, correlationId.get()));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.match")
                .with("no_match_queues", "q.shouldnt-match")
                .with("timeout_ms", "500")
                .with("settle_ms", "100")));

        assertFalse(result.isSuccessful());
        assertEquals("409", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("routing violation"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("q.shouldnt-match"),
                result.getResponseMessage());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queue.1.role=no_match"), headers);
        assertTrue(headers.contains("queue.1.received=true"), headers);
        assertTrue(headers.contains("queue.1.passed=false"), headers);
    }

    @Test
    void runTest_returns409_whenBothMissingMatchAndUnexpectedMatch_409WinsOver408() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        // Match queue that never receives → would be 408 alone.
        when(channel.basicGet(eq("q.missing-match"), eq(true))).thenReturn(null);
        // No-match queue that DOES receive → would be 409 alone.
        when(channel.basicGet(eq("q.unexpected-hit"), eq(true)))
                .thenAnswer(inv -> matchingReply(2L, correlationId.get()));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.missing-match")
                .with("no_match_queues", "q.unexpected-hit")
                .with("timeout_ms", "150")
                .with("settle_ms", "50")));

        assertFalse(result.isSuccessful());
        assertEquals("409", result.getResponseCode(),
                "409 (routing violation) must win over 408 (timeout) when both conditions fire");
        assertTrue(result.getResponseMessage().contains("q.unexpected-hit"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("also missing=[q.missing-match]"),
                result.getResponseMessage());
    }

    // --- missing match (408) --------------------------------------------

    @Test
    void runTest_returns408_whenMatchQueueNeverReceives() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.ok"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        when(channel.basicGet(eq("q.broken"), eq(true))).thenReturn(null);
        // No-match queue correctly stays empty.
        when(channel.basicGet(eq("q.nomatch"), eq(true))).thenReturn(null);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.ok, q.broken")
                .with("no_match_queues", "q.nomatch")
                .with("timeout_ms", "150")));

        assertFalse(result.isSuccessful());
        assertEquals("408", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("1/2 match queues received"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("missing=[q.broken]"),
                result.getResponseMessage());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queue.1.name=q.broken"), headers);
        assertTrue(headers.contains("queue.1.received=false"), headers);
        assertTrue(headers.contains("queue.1.passed=false"), headers);
    }

    // --- settle window for negative verification ------------------------

    @Test
    void runTest_holdsSettleWindow_afterAllMatchesHit_toConfirmNoMatchStaysEmpty() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.match"), eq(true)))
                .thenAnswer(inv -> matchingReply(1L, correlationId.get()));
        when(channel.basicGet(eq("q.nomatch"), eq(true))).thenReturn(null);

        long start = System.currentTimeMillis();
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.match")
                .with("no_match_queues", "q.nomatch")
                .with("timeout_ms", "2000")
                .with("settle_ms", "250")));
        long elapsed = System.currentTimeMillis() - start;

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());
        assertTrue(elapsed >= 200,
                "sample must hold the settle window (~250ms); elapsed=" + elapsed + "ms");
    }

    // --- non-match drain ------------------------------------------------

    @Test
    void runTest_drainsNonMatchingMessages_onMatchQueue() throws Exception {
        AtomicReference<String> correlationId = new AtomicReference<>();
        captureCorrelationIdOnPublish(correlationId);

        when(channel.basicGet(eq("q.shared"), eq(true)))
                .thenAnswer(new org.mockito.stubbing.Answer<GetResponse>() {
                    private int call = 0;
                    @Override
                    public GetResponse answer(org.mockito.invocation.InvocationOnMock inv) {
                        call++;
                        if (call == 1) return makeReply(100L, "old-id-1", "stale-1");
                        if (call == 2) return makeReply(101L, "old-id-2", "stale-2");
                        return matchingReply(102L, correlationId.get());
                    }
                });

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.shared")
                .with("timeout_ms", "2000")
                .with("settle_ms", "50")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());

        String headers = result.getResponseHeaders();
        assertTrue(headers.contains("queue.0.received=true"), headers);
        assertTrue(headers.contains("queue.0.nonMatchDrained=2"),
                "expected 2 non-match drains before the match, headers=\n" + headers);
    }

    // --- failure paths --------------------------------------------------

    @Test
    void runTest_returns500_whenChannelNotOpen_withWellDefinedTiming() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("channel is not open"),
                result.getResponseMessage());
        assertTrue(result.getTime() >= 0,
                "sample time must be well-defined on 500 path, was " + result.getTime());

        verify(channel, never()).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, never()).basicGet(anyString(), anyBoolean());
    }

    @Test
    void runTest_returns500_onBasicPublishFailure() throws Exception {
        doAnswer(inv -> {
            throw new IOException("broker unreachable");
        }).when(channel).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("broker unreachable"),
                result.getResponseMessage());
    }

    // --- config validation ----------------------------------------------

    @Test
    void runTest_fails400_whenExchangeBlank() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "")
                .with("routing_key", "logs.info")
                .with("match_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("exchange is required"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));
    }

    @Test
    void runTest_fails400_whenRoutingKeyBlank() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "   ")
                .with("match_queues", "q.a")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("routing_key is required"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));
    }

    @Test
    void runTest_fails400_whenBothQueueListsEmpty_withoutTouchingBroker() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("match_queues", "")
                .with("no_match_queues", "")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("at least one"),
                result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));
    }

    // --- only-negative-tests path ---------------------------------------

    @Test
    void runTest_returns200_whenOnlyNoMatchQueuesSpecified_andTheyStayEmpty() throws Exception {
        // Pure negative test: only no_match_queues, no match_queues.
        // Valid use case: verifying that a specific routing key doesn't
        // accidentally land on a queue that shouldn't be listening.
        when(channel.basicGet(anyString(), eq(true))).thenReturn(null);

        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "example.topic")
                .with("routing_key", "logs.info")
                .with("no_match_queues", "q.not-me, q.also-not-me")
                .with("timeout_ms", "500")
                .with("settle_ms", "100")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("0 match queues received"),
                result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("2 no-match queues stayed empty"),
                result.getResponseMessage());
    }

    // --- parseQueueList -------------------------------------------------

    @Test
    void parseQueueList_trimsTokensAndSkipsEmpty() {
        List<String> parsed = AmqpPubSubSampler.parseQueueList("q.one, q.two ,,q.three,   ");
        assertEquals(Arrays.asList("q.one", "q.two", "q.three"), parsed);
    }

    @Test
    void parseQueueList_emptyInputReturnsEmptyList() {
        assertTrue(AmqpPubSubSampler.parseQueueList("").isEmpty());
        assertTrue(AmqpPubSubSampler.parseQueueList(null).isEmpty());
    }

    // --- helpers --------------------------------------------------------

    private void captureCorrelationIdOnPublish(AtomicReference<String> out) throws IOException {
        doAnswer(inv -> {
            AMQP.BasicProperties props = inv.getArgument(2);
            out.set(props.getCorrelationId());
            return null;
        }).when(channel).basicPublish(anyString(), anyString(),
                any(AMQP.BasicProperties.class), any(byte[].class));
    }

    private static GetResponse matchingReply(long deliveryTag, String correlationId) {
        return makeReply(deliveryTag, correlationId, "matched");
    }

    private static GetResponse makeReply(long deliveryTag, String correlationId, String body) {
        Envelope env = new Envelope(deliveryTag, false, /*exchange*/ "example.topic",
                /*routingKey*/ "logs.info");
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
        Arguments defaults = new AmqpPubSubSampler().getDefaultParameters();
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
