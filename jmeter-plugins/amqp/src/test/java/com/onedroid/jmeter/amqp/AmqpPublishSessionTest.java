package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/** S5's publish half: with `session` the sampler uses the session's channel and stamps; without it, nothing changes. */
class AmqpPublishSessionTest {

    private Channel pub;
    private Session session;

    @BeforeEach
    void wire() throws Exception {
        SessionRegistry.closeAll();
        pub = mock(Channel.class);
        when(pub.isOpen()).thenReturn(true);
        when(pub.waitForConfirms(anyLong())).thenReturn(true);
        Connection conn = mock(Connection.class);
        when(conn.isOpen()).thenReturn(true);
        session = new Session("run1-s1-1", "q", "amq.direct", "rk", "classic", 100, false, conn, pub,
                mock(Channel.class), new BlockState(), new String[0]);
        SessionRegistry.putIfAbsent(session);
    }

    @AfterEach
    void unwire() {
        SessionRegistry.closeAll();
    }

    private static JavaSamplerContext ctx(String confirm, String session) {
        return TestCtx.params().with("exchange", "amq.direct").with("routing_key", "rk")
                .with("confirm", confirm).with("session", session).with("label", "t-amqp-publish").build();
    }

    @Test
    void withASessionItPublishesOnTheSessionChannelAndStampsTheSendTimeAndTheRun() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        JavaSamplerContext c = ctx("each", "run1-s1-1");
        s.setupTest(c); // must NOT open a connection of its own (there is no broker here: it would fail)
        long before = System.nanoTime();
        SampleResult r = s.runTest(c);
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        assertTrue(r.getResponseMessage().matches("published \\d+B confirm=each us=\\d+"), r.getResponseMessage());

        ArgumentCaptor<AMQP.BasicProperties> props = ArgumentCaptor.forClass(AMQP.BasicProperties.class);
        verify(pub).basicPublish(eq("amq.direct"), eq("rk"), props.capture(), any(byte[].class));
        Object sent = props.getValue().getHeaders().get("x-argus-sent-ns");
        assertNotNull(sent);
        assertTrue((Long) sent >= before && (Long) sent <= System.nanoTime(), "the stamp is nanoTime at publish");
        assertEquals("run1-s1-1", props.getValue().getHeaders().get("x-argus-run").toString());
        verify(pub).confirmSelect(); // once, on the session's publisher channel
        s.runTest(c);
        verify(pub).confirmSelect(); // still once
    }

    @Test
    void withoutASessionNoHeaderIsAddedAndTheLegacyPathIsUnchanged() throws Exception {
        AmqpPublishSampler s = new AmqpPublishSampler();
        s.setChannelForTesting(pub);
        JavaSamplerContext c = ctx("off", "");
        SampleResult r = s.runTest(c);
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        ArgumentCaptor<AMQP.BasicProperties> props = ArgumentCaptor.forClass(AMQP.BasicProperties.class);
        verify(pub).basicPublish(eq("amq.direct"), eq("rk"), props.capture(), any(byte[].class));
        assertNull(props.getValue().getHeaders(), "legacy bytes on the wire: no extra header");
    }

    @Test
    void aSessionThatIsNotUpIsA500NamingTheCauseAndPublishesNothing() throws Exception {
        SessionRegistry.closeAll();
        SampleResult r = new AmqpPublishSampler().runTest(ctx("each", "run1-s1-1"));
        assertEquals("500", r.getResponseCode());
        assertTrue(r.getResponseMessage().contains("no open session"), r.getResponseMessage());
        verify(pub, never()).basicPublish(any(String.class), any(String.class), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    @Test
    void theSessionsBlockStateGovernsThePublish() {
        session.blockState.onBlocked("memory alarm");
        SampleResult r = new AmqpPublishSampler().runTest(ctx("each", "run1-s1-1"));
        assertEquals("503", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("blocked by broker: memory alarm (since="), r.getResponseMessage());
    }
}
