package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.anyBoolean;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/** S5, no broker: a session built by hand, its inbox filled by hand. */
class AmqpSubscribeSamplerTest {

    private Channel consumer;
    private Session session;

    @BeforeEach
    void wire() {
        SessionRegistry.closeAll();
        consumer = mock(Channel.class);
        Connection conn = mock(Connection.class);
        when(conn.isOpen()).thenReturn(true);
        when(consumer.isOpen()).thenReturn(true);
        session = newSession(false);
    }

    @AfterEach
    void unwire() {
        SessionRegistry.closeAll();
    }

    private Session newSession(boolean autoAck) {
        Connection conn = mock(Connection.class);
        when(conn.isOpen()).thenReturn(true);
        Session s = new Session("run1-s1-1", "q", "amq.direct", "rk", "classic", 100, autoAck, conn,
                mock(Channel.class), consumer, new BlockState(), new String[0]);
        SessionRegistry.putIfAbsent(s);
        return s;
    }

    private static JavaSamplerContext ctx(String... kv) {
        TestCtx c = TestCtx.params().with("session", "run1-s1-1").with("timeout_ms", "300").with("label", "t-amqp-deliver");
        for (int i = 0; i < kv.length; i += 2) {
            c.with(kv[i], kv[i + 1]);
        }
        return c.buildFor(new AmqpSubscribeSampler().getDefaultParameters());
    }

    private void deliver(long tag, Long sentNs, String run, long receiptNs, boolean redelivered) {
        session.inbox.add(new Session.Delivery(tag, 1000, redelivered, sentNs, run, receiptNs, System.currentTimeMillis()));
    }

    @Test
    void aDeliveryIsOneRowWithItsLatencyInMicrosecondsAndTheSendTimeAsItsTimestamp() throws Exception {
        long now = System.nanoTime();
        deliver(11, now - 2_500_000L, session.run, now, false); // 2.5 ms in flight
        long before = System.currentTimeMillis();
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx());
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        assertEquals("t-amqp-deliver", r.getSampleLabel());
        assertEquals("200", r.getResponseCode());
        assertEquals("delivered 1000B us=2500 redelivered=false", r.getResponseMessage());
        assertEquals(2, r.getTime(), "elapsed is whole ms (2.5 truncated); the microseconds travel in the message");
        assertTrue(r.getStartTime() <= before, "the JTL timeStamp is the SEND time, not the take time");
        verify(consumer).basicAck(11L, false);
    }

    @Test
    void theLatencyIsTheRealDifferenceNotAConstant() {
        long now = System.nanoTime();
        deliver(1, now - 250_000_000L, session.run, now, true);
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx());
        assertEquals("delivered 1000B us=250000 redelivered=true", r.getResponseMessage());
    }

    @Test
    void noDeliveryIsA408NamingTheWait() {
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx("timeout_ms", "120"));
        assertFalse(r.isSuccessful());
        assertEquals("408", r.getResponseCode());
        assertEquals("no delivery within 120ms", r.getResponseMessage());
        assertTrue(r.getTime() >= 100, "it must actually have waited: " + r.getTime());
    }

    @Test
    void aForeignOrUnstampedDeliveryIsSkippedAckedAndNeverALatency() throws Exception {
        long now = System.nanoTime();
        deliver(1, now - 1_000_000L, "some-other-run", now, false);
        deliver(2, null, null, now, false);
        deliver(3, now - 1_000_000L, session.run, now, false);
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx());
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        assertEquals("delivered 1000B us=1000 redelivered=false", r.getResponseMessage());
        assertEquals(2, session.foreign.get());
        verify(consumer).basicAck(1L, false); // a foreign message must not eat the prefetch window
        verify(consumer).basicAck(2L, false);
        verify(consumer).basicAck(3L, false);
    }

    @Test
    void autoAckNeverAcks() throws Exception {
        SessionRegistry.closeAll();
        session = newSession(true);
        long now = System.nanoTime();
        deliver(5, now - 1_000_000L, session.run, now, false);
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx("ack", "auto"));
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        verify(consumer, never()).basicAck(anyLong(), anyBoolean());
    }

    @Test
    void anAckThatDisagreesWithTheSessionIsA400() {
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx("ack", "auto"));
        assertEquals("400", r.getResponseCode());
        assertTrue(r.getResponseMessage().contains("session consumes with ack=manual"), r.getResponseMessage());
    }

    @Test
    void aBlockedConnectionWithNothingWaitingIs503AtOnceAndNamesTheBlock() {
        session.blockState.onBlocked("memory alarm");
        long t0 = System.nanoTime();
        SampleResult r = new AmqpSubscribeSampler().runTest(ctx("timeout_ms", "5000"));
        long ms = (System.nanoTime() - t0) / 1_000_000L;
        assertEquals("503", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("blocked by broker: memory alarm (since="), r.getResponseMessage());
        assertTrue(ms < 1000, "must not wait the 5000ms timeout, took " + ms);
    }

    @Test
    void aMessageAlreadyWaitingIsStillDeliveredWhileBlocked() {
        session.blockState.onBlocked("memory alarm"); // a block stops PUBLISHERS; consumers keep receiving
        long now = System.nanoTime();
        deliver(1, now - 1_000_000L, session.run, now, false);
        assertTrue(new AmqpSubscribeSampler().runTest(ctx()).isSuccessful());
    }

    @Test
    void noSessionIsA500AndBadParametersAre400s() {
        SessionRegistry.closeAll();
        assertEquals("500", new AmqpSubscribeSampler().runTest(ctx()).getResponseCode());
        assertEquals("400", new AmqpSubscribeSampler().runTest(ctx("session", "")).getResponseCode());
        assertEquals("400", new AmqpSubscribeSampler().runTest(ctx("timeout_ms", "0")).getResponseCode());
        assertEquals("400", new AmqpSubscribeSampler().runTest(ctx("timeout_ms", "abc")).getResponseCode());
    }
}
