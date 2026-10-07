package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.ShutdownSignalException;
import com.rabbitmq.client.impl.AMQImpl;
import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import java.io.IOException;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.TimeoutException;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.doAnswer;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * Unit tests for {@link AmqpPublishSampler} — AC-D16 (expected AMQP refusal).
 *
 * Pattern mirrors {@link AmqpDirectReplySamplerTest}: bypass setupTest() and inject a mock
 * {@link Channel} via {@code setChannelForTesting}. A broker refusal is simulated by making
 * {@code channel.waitForConfirmsOrDie(long)} throw a {@link ShutdownSignalException} whose
 * {@code getReason()} is an {@code AMQP.Channel.Close} carrying a reply code/text — exactly what
 * amqp-client 5.22.0's {@code ChannelN.waitForConfirms} does when the broker closes the channel
 * for a protocol-level refusal (measured from the amqp-client 5.22.0 sources: {@code
 * ChannelN.waitForConfirms} checks {@code getCloseReason()} and rethrows it directly).
 *
 * Covered scenarios:
 *   - No `expected_refusal_code` declared: byte-for-byte the pre-AC-D16 publish path (200 on
 *     success, 500 on failure, no confirm-select, no wait-for-confirms).
 *   - Declared refusal + broker refuses with the EXACT declared code -> success, response code =
 *     the actual code.
 *   - Declared refusal + broker ACCEPTS the publish (no refusal) -> failure, never the declared
 *     code.
 *   - Declared refusal + broker refuses with a DIFFERENT code -> failure, observed text names the
 *     actual code only.
 *   - Declared refusal + no signal within the confirm timeout -> failure.
 *   - `user_id` is wired onto the published BasicProperties whether or not a refusal is declared.
 *   - A password given to the sampler is never echoed into a response message.
 */
class AmqpPublishSamplerTest {

    private AmqpPublishSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpPublishSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        sampler.setChannelForTesting(channel);
    }

    // --- unchanged behaviour: no expected_refusal_code -------------------

    @Test
    void runTest_publishesSuccessfully_whenNoRefusalDeclared() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("exchange", "orders")
                .with("routing_key", "orders.incoming")
                .with("message_body", "{\"a\":1}")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("200", result.getResponseCode());
        verify(channel).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        // no expected_refusal_code => the confirm-select path is never entered.
        verify(channel, never()).confirmSelect();
        verify(channel, never()).waitForConfirmsOrDie(anyLong());
    }

    @Test
    void runTest_returns500_whenChannelNotOpen_noRefusalDeclared() throws Exception {
        when(channel.isOpen()).thenReturn(false);

        SampleResult result = sampler.runTest(ctx(params()));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("channel is not open"), result.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    @Test
    void runTest_returns500_onBasicPublishFailure_noRefusalDeclared() throws Exception {
        doThrow(new IOException("connection reset")).when(channel)
                .basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));

        SampleResult result = sampler.runTest(ctx(params()));

        assertFalse(result.isSuccessful());
        assertEquals("500", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("connection reset"), result.getResponseMessage());
    }

    // --- declared refusal: broker refuses with the declared code ---------

    @Test
    void runTest_succeeds_whenBrokerRefusesWithDeclaredCode() throws Exception {
        doThrow(shutdownFor(406, "PRECONDITION_FAILED - user_id property set to 'guest' but authenticated user was 'svc'"))
                .when(channel).waitForConfirmsOrDie(anyLong());

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("user_id", "guest")
                .with("expected_refusal_code", "406")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("406", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("406"), result.getResponseMessage());
        verify(channel).confirmSelect();
    }

    @Test
    void runTest_succeeds_whenBrokerRefusesWithDeclaredCode_accessRefused() throws Exception {
        doThrow(shutdownFor(403, "ACCESS_REFUSED - publish access denied"))
                .when(channel).waitForConfirmsOrDie(anyLong());

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("expected_refusal_code", "403")));

        assertTrue(result.isSuccessful(), result.getResponseMessage());
        assertEquals("403", result.getResponseCode());
    }

    // --- declared refusal: mismatches --------------------------------------

    @Test
    void runTest_fails_whenBrokerAcceptsPublish_refusalDeclared() throws Exception {
        // waitForConfirmsOrDie returns normally: the broker acknowledged, no refusal happened.
        doAnswer(inv -> null).when(channel).waitForConfirmsOrDie(anyLong());

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("expected_refusal_code", "406")));

        assertFalse(result.isSuccessful());
        assertEquals("200", result.getResponseCode());
        assertTrue(result.getResponseMessage().toLowerCase().contains("accepted"), result.getResponseMessage());
        // reality-only (VR-C8): the failure text never repeats the declared code.
        assertFalse(result.getResponseMessage().contains("406"), result.getResponseMessage());
    }

    @Test
    void runTest_fails_whenBrokerRefusesWithDifferentCode() throws Exception {
        doThrow(shutdownFor(403, "ACCESS_REFUSED - publish access denied"))
                .when(channel).waitForConfirmsOrDie(anyLong());

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("expected_refusal_code", "406")));

        assertFalse(result.isSuccessful());
        assertEquals("403", result.getResponseCode());
        assertTrue(result.getResponseMessage().contains("403"), result.getResponseMessage());
        assertTrue(result.getResponseMessage().contains("REFUSAL-MISMATCH"), result.getResponseMessage());
        // reality-only: the mismatch message names the ACTUAL code, never the declared 406.
        assertFalse(result.getResponseMessage().contains("406"), result.getResponseMessage());
    }

    @Test
    void runTest_fails_onTimeout_refusalDeclared() throws Exception {
        doThrow(new TimeoutException()).when(channel).waitForConfirmsOrDie(anyLong());

        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("expected_refusal_code", "406")
                .with("confirm_timeout_ms", "150")));

        assertFalse(result.isSuccessful());
        assertTrue(result.getResponseMessage().contains("no refusal was observed"), result.getResponseMessage());
    }

    @Test
    void runTest_fails400_whenExpectedRefusalCodeNotNumeric() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("expected_refusal_code", "PRECONDITION_FAILED_TYPO")));

        assertFalse(result.isSuccessful());
        assertEquals("400", result.getResponseCode());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    // --- user_id wiring ----------------------------------------------------

    @Test
    void runTest_setsUserIdOnPublishedProperties_whenDeclared() throws Exception {
        doThrow(shutdownFor(406, "PRECONDITION_FAILED - user_id mismatch"))
                .when(channel).waitForConfirmsOrDie(anyLong());

        ArgumentCaptor<AMQP.BasicProperties> captor = ArgumentCaptor.forClass(AMQP.BasicProperties.class);

        sampler.runTest(ctx(params()
                .with("routing_key", "orders.incoming")
                .with("user_id", "guest")
                .with("expected_refusal_code", "406")));

        verify(channel).basicPublish(anyString(), anyString(), captor.capture(), any(byte[].class));
        assertEquals("guest", captor.getValue().getUserId());
    }

    @Test
    void runTest_omitsUserId_whenNotDeclared() throws Exception {
        ArgumentCaptor<AMQP.BasicProperties> captor = ArgumentCaptor.forClass(AMQP.BasicProperties.class);

        sampler.runTest(ctx(params().with("routing_key", "orders.incoming")));

        verify(channel).basicPublish(anyString(), anyString(), captor.capture(), any(byte[].class));
        assertNull(captor.getValue().getUserId());
    }

    // --- secrecy -------------------------------------------------------------

    @Test
    void runTest_neverEchoesPassword_onConfigError() throws Exception {
        SampleResult result = sampler.runTest(ctx(params()
                .with("password", "s3cr3t-broker-pw")
                .with("expected_refusal_code", "not-a-code")));

        assertFalse(result.isSuccessful());
        assertFalse(result.getResponseMessage().contains("s3cr3t-broker-pw"), result.getResponseMessage());
    }

    // --- helpers --------------------------------------------------------

    private static ShutdownSignalException shutdownFor(int replyCode, String replyText) {
        AMQP.Channel.Close close = new AMQImpl.Channel.Close(replyCode, replyText, 60, 40);
        return new ShutdownSignalException(false, false, close, "test-channel");
    }

    private static ParamBuilder params() {
        return new ParamBuilder();
    }

    private static JavaSamplerContext ctx(ParamBuilder p) {
        Arguments args = new AmqpPublishSampler().getDefaultParameters();
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
