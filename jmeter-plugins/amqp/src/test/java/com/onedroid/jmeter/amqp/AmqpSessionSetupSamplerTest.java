package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ShutdownSignalException;
import com.rabbitmq.client.impl.AMQImpl;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import java.io.IOException;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyBoolean;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/** S6, no broker: the connection comes from a mock through the sampler's opener seam. */
class AmqpSessionSetupSamplerTest {

    private AmqpSessionSetupSampler.ConnectionOpener original;
    private Connection conn;
    private Channel publisher;
    private Channel consumer;
    private int opens;

    @BeforeEach
    void wire() throws Exception {
        original = AmqpSessionSetupSampler.opener;
        SessionRegistry.closeAll();
        conn = mock(Connection.class);
        publisher = mock(Channel.class);
        consumer = mock(Channel.class);
        when(conn.createChannel()).thenReturn(publisher, consumer);
        when(conn.isOpen()).thenReturn(true);
        when(consumer.isOpen()).thenReturn(true);
        opens = 0;
        AmqpSessionSetupSampler.opener = (uri, user, pw, t) -> {
            opens++;
            return conn;
        };
    }

    @AfterEach
    void unwire() {
        AmqpSessionSetupSampler.opener = original;
        SessionRegistry.closeAll();
    }

    private static JavaSamplerContext ctx(String... kv) {
        TestCtx c = TestCtx.params()
                .with("amqp_uri", "amqp://u:s3cr3t-pw@localhost:5672")
                .with("session", "run1-s1-1")
                .with("exchange", "amq.direct")
                .with("routing_key", "argus-load.run1.1.0001")
                .with("queue", "argus-load-run1-s1-0001")
                .with("queue_expires_ms", "120000")
                .with("label", "t-amqp-setup");
        for (int i = 0; i < kv.length; i += 2) {
            c.with(kv[i], kv[i + 1]);
        }
        return c.buildFor(new AmqpSessionSetupSampler().getDefaultParameters());
    }

    @SuppressWarnings("unchecked")
    @Test
    void declaresBindsSetsPrefetchAndArmsTheConsumerBeforeReturning() throws Exception {
        SampleResult r = new AmqpSessionSetupSampler().runTest(ctx("queue_type", "quorum", "prefetch", "7"));
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        assertEquals("t-amqp-setup", r.getSampleLabel());
        assertTrue(r.getResponseMessage().contains("queue=argus-load-run1-s1-0001"), r.getResponseMessage());
        assertTrue(r.getResponseMessage().matches(".*us=\\d+"), r.getResponseMessage());

        ArgumentCaptor<Map<String, Object>> args = ArgumentCaptor.forClass(Map.class);
        verify(consumer).queueDeclare(eq("argus-load-run1-s1-0001"), eq(true), eq(false), eq(false), args.capture());
        assertEquals("quorum", args.getValue().get("x-queue-type"));
        assertEquals(120000L, args.getValue().get("x-expires"));
        verify(consumer).queueBind("argus-load-run1-s1-0001", "amq.direct", "argus-load.run1.1.0001");
        verify(consumer).basicQos(7);
        verify(consumer).basicConsume(eq("argus-load-run1-s1-0001"), eq(false), any(com.rabbitmq.client.Consumer.class));
        // the consumer is armed on the CONSUMER channel, never on the publisher's
        verify(publisher, never()).basicConsume(anyString(), anyBoolean(), any(com.rabbitmq.client.Consumer.class));
        assertNotNull(SessionRegistry.get("run1-s1-1"));
    }

    @SuppressWarnings("unchecked")
    @Test
    void classicQueueIsNotQuorumAndAutoAckIsPassedThrough() throws Exception {
        SampleResult r = new AmqpSessionSetupSampler().runTest(ctx("ack", "auto", "queue_expires_ms", "0"));
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        ArgumentCaptor<Map<String, Object>> args = ArgumentCaptor.forClass(Map.class);
        verify(consumer).queueDeclare(anyString(), eq(false), eq(false), eq(false), args.capture());
        assertTrue(args.getValue() == null || !args.getValue().containsKey("x-queue-type"));
        verify(consumer).basicConsume(anyString(), eq(true), any(com.rabbitmq.client.Consumer.class));
    }

    @Test
    void aSecondSetupOfTheSameKeyReturnsTheSameSessionAndOpensNothing() {
        AmqpSessionSetupSampler s = new AmqpSessionSetupSampler();
        s.runTest(ctx());
        Session first = SessionRegistry.get("run1-s1-1");
        SampleResult again = s.runTest(ctx());
        assertTrue(again.isSuccessful());
        assertTrue(again.getResponseMessage().startsWith("session already up"), again.getResponseMessage());
        assertEquals(1, opens);
        assertTrue(first == SessionRegistry.get("run1-s1-1"));
    }

    @Test
    void parameterTyposAreLoud400sAndNothingConnects() {
        String[][] bad = {
                {"queue_type", "lazy"}, {"ack", "maybe"}, {"prefetch", "-1"}, {"prefetch", "x"},
                {"queue_expires_ms", "-5"}, {"connect_timeout_ms", "0"}, {"queue", ""}, {"routing_key", ""}, {"session", ""}};
        for (String[] kv : bad) {
            SampleResult r = new AmqpSessionSetupSampler().runTest(ctx(kv));
            assertFalse(r.isSuccessful(), kv[0]);
            assertEquals("400", r.getResponseCode(), kv[0] + ": " + r.getResponseMessage());
        }
        assertEquals(0, opens, "a bad parameter must never reach the network");
    }

    @Test
    void aBrokerRefusalCarriesItsReplyCodeAndTheHalfOpenConnectionIsAborted() throws Exception {
        AMQP.Channel.Close close = new AMQImpl.Channel.Close(406, "PRECONDITION_FAILED - inequivalent arg", 50, 10);
        doThrow(new IOException(new ShutdownSignalException(false, false, close, "ch")))
                .when(consumer).queueDeclare(anyString(), anyBoolean(), anyBoolean(), anyBoolean(), any());
        SampleResult r = new AmqpSessionSetupSampler().runTest(ctx());
        assertFalse(r.isSuccessful());
        assertEquals("406", r.getResponseCode());
        assertTrue(r.getResponseMessage().contains("PRECONDITION_FAILED"), r.getResponseMessage());
        verify(conn).abort(anyInt());
        assertNull(SessionRegistry.get("run1-s1-1"), "a failed setup must not leave a session behind");
    }

    @Test
    void aConnectFailureNeverEchoesThePassword() {
        AmqpSessionSetupSampler.opener = (uri, user, pw, t) -> {
            throw new IOException("could not connect to " + uri);
        };
        SampleResult r = new AmqpSessionSetupSampler().runTest(ctx());
        assertFalse(r.isSuccessful());
        assertEquals("500", r.getResponseCode());
        assertTrue(r.getResponseMessage().contains("could not connect"), "control: the error text is there");
        assertFalse(r.getResponseMessage().contains("s3cr3t-pw"), r.getResponseMessage());
    }

    @Test
    void teardownClosesEverySessionOnceEvenOnAFreshInstance() throws Exception {
        new AmqpSessionSetupSampler().runTest(ctx());
        // JMeter calls teardownTest on a NEW instance at test end
        new AmqpSessionSetupSampler().teardownTest(ctx());
        verify(consumer, times(1)).queueDelete("argus-load-run1-s1-0001");
        verify(conn, times(1)).abort(anyInt());
        new AmqpSessionSetupSampler().teardownTest(ctx()); // idempotent
        verify(conn, times(1)).abort(anyInt());
        assertEquals(0, SessionRegistry.size());
    }
}
