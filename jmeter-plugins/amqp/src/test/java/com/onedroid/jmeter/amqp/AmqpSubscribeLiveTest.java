package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * S5 against the mission's own throwaway RabbitMQ: real publish -> real broker -> real consumer, through the
 * session registry exactly as the template drives it. Each test owns its run id, so its queue is its own.
 */
@Tag("live")
class AmqpSubscribeLiveTest {

    private static final Pattern US = Pattern.compile("\\bus=(\\d+)");
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

    private static long us(SampleResult r) {
        Matcher m = US.matcher(r.getResponseMessage());
        assertTrue(m.find(), "a delivery row carries us=: " + r.getResponseMessage());
        return Long.parseLong(m.group(1));
    }

    private static long percentile(List<Long> sorted, int p) { // nearest rank, as the Go side
        int idx = (p * sorted.size() + 99) / 100 - 1;
        return sorted.get(Math.max(0, Math.min(sorted.size() - 1, idx)));
    }

    @Test
    void perMessageLatencyIsReal() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        AmqpSessionSetupSampler setup = new AmqpSessionSetupSampler();
        SampleResult up = setup.runTest(live.setup(run, 1));
        assertTrue(up.isSuccessful(), up.getResponseMessage());

        AmqpPublishSampler pub = new AmqpPublishSampler();
        JavaSamplerContext pc = live.publish(run, 1);
        pub.setupTest(pc);
        AmqpSubscribeSampler sub = new AmqpSubscribeSampler();
        JavaSamplerContext sc = LiveSessions.subscribe(run, 1);

        int n = 60;
        List<Long> lat = new ArrayList<Long>();
        Set<Long> distinct = new HashSet<Long>();
        long firstStamp = -1;
        for (int i = 0; i < n; i++) {
            SampleResult p = pub.runTest(pc);
            assertTrue(p.isSuccessful(), p.getResponseCode() + " " + p.getResponseMessage());
            SampleResult d = sub.runTest(sc);
            assertTrue(d.isSuccessful(), d.getResponseCode() + " " + d.getResponseMessage());
            assertTrue(d.getResponseMessage().matches("delivered 1000B us=\\d+ redelivered=false"), d.getResponseMessage());
            if (i == 0) {
                System.out.println("JTL-responseMessage[live-subscribe-delivery]: " + d.getResponseCode() + " " + d.getResponseMessage()
                        + " (label=" + d.getSampleLabel() + ")");
                firstStamp = d.getStartTime();
            }
            long u = us(d);
            assertTrue(u > 0, "a real round trip is not zero");
            lat.add(u);
            distinct.add(u);
        }
        assertTrue(firstStamp > 0 && firstStamp <= System.currentTimeMillis(), "the JTL timeStamp is the send time");
        Collections.sort(lat);
        long p50 = percentile(lat, 50), p95 = percentile(lat, 95), p99 = percentile(lat, 99);
        System.out.println("JTL-latency[live-subscribe]: n=" + n + " distinct=" + distinct.size() + " min=" + lat.get(0)
                + "us p50=" + p50 + "us p95=" + p95 + "us p99=" + p99 + "us max=" + lat.get(n - 1) + "us");
        assertTrue(distinct.size() >= n / 2, "N messages must give N measurements, not one constant: distinct=" + distinct.size());
        assertTrue(p50 <= p95 && p95 <= p99 && p99 <= lat.get(n - 1));
        assertEquals(0, SessionRegistry.get(LiveSessions.key(run, 1)).foreign.get());
    }

    @Test
    void noDeliveryIsA408() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        assertTrue(new AmqpSessionSetupSampler().runTest(live.setup(run, 1)).isSuccessful());
        SampleResult r = new AmqpSubscribeSampler().runTest(LiveSessions.subscribe(run, 1, "timeout_ms", "400"));
        assertTrue(!r.isSuccessful());
        assertEquals("408", r.getResponseCode());
        assertEquals("no delivery within 400ms", r.getResponseMessage());
        System.out.println("JTL-responseMessage[live-subscribe-timeout]: " + r.getResponseCode() + " " + r.getResponseMessage()
                + " (elapsed=" + r.getTime() + "ms)");
        assertTrue(r.getTime() >= 350, "it must have waited: " + r.getTime());
    }

    /** POSITIVE CONTROL: a message stamped 250 ms in the past reports at least that lag: the arithmetic is not a constant. */
    @Test
    void stampedInThePastShowsTheLag() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        assertTrue(new AmqpSessionSetupSampler().runTest(live.setup(run, 1)).isSuccessful());
        Session s = SessionRegistry.get(LiveSessions.key(run, 1));

        Map<String, Object> headers = new HashMap<String, Object>();
        headers.put(AmqpPublishSampler.HEADER_SENT_NS, System.nanoTime() - 250_000_000L);
        headers.put(AmqpPublishSampler.HEADER_RUN, s.run);
        s.publisherChannel.basicPublish("amq.direct", LiveSessions.routingKey(run, 1),
                new AMQP.BasicProperties.Builder().headers(headers).build(), "late".getBytes(StandardCharsets.UTF_8));

        SampleResult d = new AmqpSubscribeSampler().runTest(LiveSessions.subscribe(run, 1));
        assertTrue(d.isSuccessful(), d.getResponseMessage());
        long u = us(d);
        System.out.println("JTL-responseMessage[live-stamped-250ms-in-the-past]: " + d.getResponseCode() + " " + d.getResponseMessage());
        assertTrue(u >= 250_000, "us=" + u);
        assertTrue(u < 5_000_000, "and not absurd: us=" + u);
    }

    @Test
    void aForeignRunHeaderIsSkippedAndNeverALatency() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        assertTrue(new AmqpSessionSetupSampler().runTest(live.setup(run, 1)).isSuccessful());
        Session s = SessionRegistry.get(LiveSessions.key(run, 1));
        Map<String, Object> headers = new HashMap<String, Object>();
        headers.put(AmqpPublishSampler.HEADER_SENT_NS, System.nanoTime());
        headers.put(AmqpPublishSampler.HEADER_RUN, "someone-elses-run");
        s.publisherChannel.basicPublish("amq.direct", LiveSessions.routingKey(run, 1),
                new AMQP.BasicProperties.Builder().headers(headers).build(), "x".getBytes(StandardCharsets.UTF_8));
        SampleResult r = new AmqpSubscribeSampler().runTest(LiveSessions.subscribe(run, 1, "timeout_ms", "800"));
        assertEquals("408", r.getResponseCode(), r.getResponseMessage());
        assertEquals(1, s.foreign.get());
    }

    @Test
    void manualAckRespectsPrefetch() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        String q = LiveSessions.queueName(run, 1);
        assertTrue(new AmqpSessionSetupSampler().runTest(live.setup(run, 1, "prefetch", "1", "ack", "manual")).isSuccessful());
        Session s = SessionRegistry.get(LiveSessions.key(run, 1));
        AmqpPublishSampler pub = new AmqpPublishSampler();
        JavaSamplerContext pc = live.publish(run, 1);
        pub.setupTest(pc);
        for (int i = 0; i < 5; i++) {
            assertTrue(pub.runTest(pc).isSuccessful());
        }
        Thread.sleep(1000);
        assertEquals(1, s.inbox.size(), "prefetch=1 + manual ack: only ONE message may be in flight before an ack");

        // the broker agrees (management stats refresh every ~5s): never more than 1 unacknowledged
        int maxUnacked = 0;
        long end = System.currentTimeMillis() + 20000;
        while (System.currentTimeMillis() < end && maxUnacked < 1) {
            maxUnacked = Math.max(maxUnacked, field(broker.queueJson(q), "messages_unacknowledged"));
            Thread.sleep(500);
        }
        assertEquals(1, maxUnacked, "the broker reports exactly one unacknowledged message");
        assertTrue(field(broker.queueJson(q), "messages_unacknowledged") <= 1);

        // taking (and acking) lets the next one through: all 5 arrive, one at a time
        AmqpSubscribeSampler sub = new AmqpSubscribeSampler();
        JavaSamplerContext sc = LiveSessions.subscribe(run, 1, "ack", "manual");
        for (int i = 0; i < 5; i++) {
            SampleResult d = sub.runTest(sc);
            assertTrue(d.isSuccessful(), "delivery " + (i + 1) + ": " + d.getResponseMessage());
            assertTrue(s.inbox.size() <= 1, "never more than one in flight");
        }
    }

    /** NEGATIVE CONTROL for the test above: with a big prefetch the same 5 messages ARE all in flight. */
    @Test
    void aLargePrefetchPutsEverythingInFlight() throws Exception {
        live = new LiveSessions(broker);
        String run = LiveSessions.newRunId();
        assertTrue(new AmqpSessionSetupSampler().runTest(live.setup(run, 1, "prefetch", "100", "ack", "manual")).isSuccessful());
        Session s = SessionRegistry.get(LiveSessions.key(run, 1));
        AmqpPublishSampler pub = new AmqpPublishSampler();
        JavaSamplerContext pc = live.publish(run, 1);
        pub.setupTest(pc);
        for (int i = 0; i < 5; i++) {
            assertTrue(pub.runTest(pc).isSuccessful());
        }
        Thread.sleep(1000);
        assertEquals(5, s.inbox.size());
    }

    private static int field(String json, String name) {
        Matcher m = Pattern.compile("\"" + name + "\"\\s*:\\s*(\\d+)").matcher(json == null ? "" : json);
        return m.find() ? Integer.parseInt(m.group(1)) : 0;
    }
}
