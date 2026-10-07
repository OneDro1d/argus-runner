package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.ShutdownSignalException;
import com.rabbitmq.client.impl.AMQImpl;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.util.concurrent.TimeoutException;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/** S3 mechanics against a mock Channel: each / batch / off wiring and the failure mapping. */
class AmqpPublishConfirmTest {

    private AmqpPublishSampler sampler;
    private Channel channel;

    @BeforeEach
    void setUp() throws Exception {
        sampler = new AmqpPublishSampler();
        channel = mock(Channel.class);
        when(channel.isOpen()).thenReturn(true);
        when(channel.waitForConfirms(anyLong())).thenReturn(true);
        sampler.setChannelForTesting(channel);
    }

    private SampleResult run(String confirm) {
        return sampler.runTest(TestCtx.params().with("confirm", confirm).with("confirm_timeout_ms", "1500")
                .with("label", "t-amqp-publish").build());
    }

    @Test
    void off_neverConfirms() throws Exception {
        SampleResult r = run("off");
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        assertEquals("t-amqp-publish", r.getSampleLabel());
        verify(channel, never()).confirmSelect();
        verify(channel, never()).waitForConfirms(anyLong());
        verify(channel, never()).waitForConfirmsOrDie(anyLong());
    }

    @Test
    void each_selectsOnceAndWaitsForEveryPublish() throws Exception {
        SampleResult a = run("each");
        SampleResult b = run("each");

        assertTrue(a.isSuccessful(), a.getResponseMessage());
        assertTrue(b.isSuccessful(), b.getResponseMessage());
        assertTrue(a.getResponseMessage().matches("published \\d+B confirm=each us=\\d+"), a.getResponseMessage());
        verify(channel, times(1)).confirmSelect(); // once, not once per sample
        verify(channel, times(2)).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
        verify(channel, times(2)).waitForConfirms(1500L);
    }

    @Test
    void each_nackIs502AndSaysTheBrokerGivesNoReason() throws Exception {
        when(channel.waitForConfirms(anyLong())).thenReturn(false);
        SampleResult r = run("each");
        assertFalse(r.isSuccessful());
        assertEquals("502", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("nacked by broker (basic.nack carries no reason; usual causes:"), r.getResponseMessage());
    }

    @Test
    void each_confirmTimeoutIs504() throws Exception {
        when(channel.waitForConfirms(anyLong())).thenThrow(new TimeoutException());
        SampleResult r = run("each");
        assertFalse(r.isSuccessful());
        assertEquals("504", r.getResponseCode());
        assertEquals("not confirmed within 1500ms", r.getResponseMessage());
        System.out.println("JTL-responseMessage[mock-timeout]: " + r.getResponseCode() + " " + r.getResponseMessage());
    }

    @Test
    void each_channelClosedByBrokerCarriesTheAmqpReplyCode() throws Exception {
        AMQP.Channel.Close close = new AMQImpl.Channel.Close(404, "NOT_FOUND - no exchange 'x'", 60, 40);
        when(channel.waitForConfirms(anyLong())).thenThrow(new ShutdownSignalException(false, false, close, "ch"));
        SampleResult r = run("each");
        assertFalse(r.isSuccessful());
        assertEquals("404", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("channel closed by broker: 404 NOT_FOUND"), r.getResponseMessage());
    }

    @Test
    void batch_onlyTheNthPublishWaits() throws Exception {
        SampleResult a = run("batch:3");
        SampleResult b = run("batch:3");
        verify(channel, never()).waitForConfirms(anyLong());
        SampleResult c = run("batch:3");

        assertTrue(a.getResponseMessage().endsWith("confirm=batch"), a.getResponseMessage());
        assertFalse(a.getResponseMessage().contains("us="), "no per-publish confirm figure for a batch row");
        assertTrue(b.isSuccessful());
        assertTrue(c.getResponseMessage().matches("published \\d+B confirm=batch batch-confirm us=\\d+"), c.getResponseMessage());
        verify(channel, times(1)).waitForConfirms(1500L);

        run("batch:3"); // the counter restarted: the 4th publish is the first of the next batch
        verify(channel, times(1)).waitForConfirms(anyLong());
    }

    @Test
    void badConfirmValuesAre400() throws Exception {
        for (String bad : new String[] {"sometimes", "batch:1", "batch:1001", "batch:x", "batch:"}) {
            SampleResult r = run(bad);
            assertEquals("400", r.getResponseCode(), bad);
            assertEquals("confirm must be off, each or batch:<n>", r.getResponseMessage(), bad);
        }
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }

    @Test
    void refusalPathStaysAndRejectsBatch() throws Exception {
        SampleResult r = sampler.runTest(TestCtx.params().with("confirm", "batch:5").with("expected_refusal_code", "406").build());
        assertEquals("400", r.getResponseCode());
        assertEquals("confirm must be off or each when expected_refusal_code is set", r.getResponseMessage());

        AMQP.Channel.Close close = new AMQImpl.Channel.Close(406, "PRECONDITION_FAILED", 60, 40);
        doThrow(new ShutdownSignalException(false, false, close, "ch")).when(channel).waitForConfirmsOrDie(anyLong());
        SampleResult ok = sampler.runTest(TestCtx.params().with("confirm", "each").with("expected_refusal_code", "406").build());
        assertTrue(ok.isSuccessful(), ok.getResponseMessage());
        assertEquals("406", ok.getResponseCode());
    }

    @Test
    void aBlockedConnectionFailsBeforeAnythingIsPublished() throws Exception {
        sampler.blockStateForTesting().onBlocked("low on memory");
        SampleResult r = run("off");
        assertFalse(r.isSuccessful());
        assertEquals("503", r.getResponseCode());
        assertTrue(r.getResponseMessage().startsWith("blocked by broker: low on memory (since="), r.getResponseMessage());
        verify(channel, never()).basicPublish(anyString(), anyString(), any(AMQP.BasicProperties.class), any(byte[].class));
    }
}
