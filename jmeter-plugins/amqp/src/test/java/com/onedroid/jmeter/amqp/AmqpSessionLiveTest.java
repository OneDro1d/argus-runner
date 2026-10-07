package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * S6 against the mission's own throwaway RabbitMQ (see {@link AmqpLiveHarness} for every rule). Every queue
 * carries x-expires and is deleted by the teardown; the management API is the witness.
 */
@Tag("live")
class AmqpSessionLiveTest {

    private static AmqpLiveHarness broker;
    private LiveSessions live;

    @BeforeAll
    static void connect() throws Exception {
        broker = AmqpLiveHarness.acquire();
        if (broker.memAlarm()) {
            broker.alarmOff();
        }
    }

    @AfterAll
    static void disconnect() throws Exception {
        if (broker != null) {
            broker.close();
        }
    }

    @AfterEach
    void cleanup() {
        if (live != null) {
            live.cleanup();
        }
    }

    @Test
    void declaresQuorumQueueWithExpiresAndRunIdInName() throws Exception {
        live = new LiveSessions(broker);
        String run1 = LiveSessions.newRunId();
        String run2 = LiveSessions.newRunId();
        AmqpSessionSetupSampler s = new AmqpSessionSetupSampler();

        SampleResult r1 = s.runTest(live.setup(run1, 1, "queue_type", "quorum"));
        assertTrue(r1.isSuccessful(), r1.getResponseCode() + " " + r1.getResponseMessage());
        System.out.println("JTL-responseMessage[live-session-setup]: " + r1.getResponseCode() + " " + r1.getResponseMessage());
        String q1 = LiveSessions.queueName(run1, 1);
        assertTrue(q1.contains(run1) && r1.getResponseMessage().contains("queue=" + q1), r1.getResponseMessage());

        String json = broker.queueJson(q1);
        assertNotNull(json, "the queue must exist on the broker");
        assertTrue(json.replace(" ", "").contains("\"x-queue-type\":\"quorum\""), "quorum: " + json);
        assertTrue(json.replace(" ", "").contains("\"x-expires\":" + LiveSessions.EXPIRES_MS), "x-expires: " + json);
        String bindings = broker.mgmtGetOrNull("/api/queues/%2F/" + q1 + "/bindings").replace(" ", "");
        assertTrue(bindings.contains("\"source\":\"amq.direct\"") && bindings.contains("\"routing_key\":\"" + LiveSessions.routingKey(run1, 1) + "\""),
                "bound to the exchange with the per-session routing key: " + bindings);

        // a SECOND run id is a different queue, and the first is untouched
        SampleResult r2 = s.runTest(live.setup(run2, 1, "queue_type", "quorum"));
        assertTrue(r2.isSuccessful(), r2.getResponseMessage());
        String q2 = LiveSessions.queueName(run2, 1);
        assertNotEquals(q1, q2);
        assertNotNull(broker.queueJson(q1));
        assertNotNull(broker.queueJson(q2));
    }

    @Test
    void aClassicQueueIsNotQuorum() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        SampleResult r = new AmqpSessionSetupSampler().runTest(live.setup(run, 1));
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        String json = broker.queueJson(LiveSessions.queueName(run, 1)).replace(" ", "");
        assertTrue(!json.contains("\"x-queue-type\":\"quorum\""), json);
        assertTrue(json.contains("\"x-expires\":" + LiveSessions.EXPIRES_MS), json);
    }

    @Test
    void teardownDeletesTheQueue() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        String q = LiveSessions.queueName(run, 1);
        SampleResult r = new AmqpSessionSetupSampler().runTest(live.setup(run, 1));
        assertTrue(r.isSuccessful(), r.getResponseMessage());
        String json = broker.queueJson(q);
        assertNotNull(json);
        // the backstop for a killed run: the argument is on the queue
        assertTrue(json.replace(" ", "").contains("\"x-expires\":" + LiveSessions.EXPIRES_MS), json);

        new AmqpSessionSetupSampler().teardownTest(null); // JMeter's testEnded: a fresh instance
        long end = System.currentTimeMillis() + 10000;
        while (broker.queueJson(q) != null && System.currentTimeMillis() < end) {
            Thread.sleep(200);
        }
        assertNull(broker.queueJson(q), "teardown must delete the session's own queue");
        assertEquals(0, SessionRegistry.size());
    }

    @Test
    void aRefusedSetupCarriesTheReplyCodeAndLeavesNothingBehind() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        // binding to an exchange that does not exist: the broker closes the channel with 404
        JavaSamplerContext c = live.setup(run, 1, "exchange", "argus-no-such-exchange-" + run);
        SampleResult r = new AmqpSessionSetupSampler().runTest(c);
        assertTrue(!r.isSuccessful());
        assertEquals("404", r.getResponseCode(), r.getResponseMessage());
        System.out.println("JTL-responseMessage[live-session-setup-refused]: " + r.getResponseCode() + " " + r.getResponseMessage());
        assertNull(SessionRegistry.get(LiveSessions.key(run, 1)));
    }
}
