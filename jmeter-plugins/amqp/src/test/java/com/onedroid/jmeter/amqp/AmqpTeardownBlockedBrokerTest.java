package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * with a broker that has BLOCKED publishers, the AMQP Load sampler's teardown must return
 * within a bound that does not grow with the number of sessions, and must leave no connection open (an open
 * connection keeps its non-daemon NIO threads alive, and a JMeter JVM does not exit while one is alive, so the
 * Go executor had to SIGKILL it at its backstop).
 *
 * The fake is {@link DeafBrokerFake}: after the block it READS NOTHING, like a real broker under a memory alarm,
 * so a queue.delete is never answered and a connection.close never gets its close-ok. Loopback only.
 */
class AmqpTeardownBlockedBrokerTest {

    /**
     * The bound a teardown must meet however many sessions there are: the queue-delete wait plus the close wait
     * (two CLOSE_TIMEOUT_MS, in parallel across sessions) plus slack. 50 sessions closed serially would be
     * 50 x 3000 ms = 150 s.
     */
    private static final long TEARDOWN_BOUND_MS = 2L * AmqpPublishSampler.CLOSE_TIMEOUT_MS + 4000L;

    private DeafBrokerFake broker;

    @AfterEach
    void stop() {
        SessionRegistry.closeAll();
        if (broker != null) {
            broker.close();
        }
    }

    private JavaSamplerContext setupCtx(String uri, int i) {
        return TestCtx.params()
                .with("amqp_uri", uri)
                .with("session", "bt-" + i)
                .with("queue", "q-bt-" + i)
                .with("routing_key", "rk-bt-" + i)
                .with("connect_timeout_ms", "10000")
                .buildFor(new AmqpSessionSetupSampler().getDefaultParameters());
    }

    private List<Session> openSessions(int n) {
        List<Session> sessions = new ArrayList<Session>();
        AmqpSessionSetupSampler setup = new AmqpSessionSetupSampler();
        for (int i = 0; i < n; i++) {
            SampleResult r = setup.runTest(setupCtx(broker.uri(), i));
            assertTrue(r.isSuccessful(), "session " + i + " did not come up: " + r.getResponseCode() + " " + r.getResponseMessage());
            sessions.add(SessionRegistry.get("bt-" + i));
        }
        assertEquals(n, SessionRegistry.size());
        return sessions;
    }

    /** One unconfirmed publish per session: the fake blocks that connection (announced or not) and goes deaf. */
    private void blockEverySession(List<Session> sessions, boolean announced) throws Exception {
        for (Session s : sessions) {
            s.publisherChannel.basicPublish("amq.direct", "rk", null, new byte[16]);
        }
        assertTrue(broker.awaitBlocked(sessions.size(), 20000), "only " + broker.blockedConnections() + " of "
                + sessions.size() + " connections reached the block");
        if (announced) {
            long deadline = System.currentTimeMillis() + 10000;
            for (Session s : sessions) {
                while (!s.blockState.isBlocked() && System.currentTimeMillis() < deadline) {
                    Thread.sleep(10);
                }
                assertTrue(s.blockState.isBlocked(), "the client never saw connection.blocked for " + s.key);
            }
        }
    }

    private long teardownMs() {
        long t0 = System.nanoTime();
        new AmqpSessionSetupSampler().teardownTest(setupCtx(broker.uri(), 0));
        return (System.nanoTime() - t0) / 1_000_000L;
    }

    private static String nonDaemonThreads() {
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<Thread, StackTraceElement[]> e : Thread.getAllStackTraces().entrySet()) {
            Thread t = e.getKey();
            if (!t.isDaemon() && t.isAlive()) {
                sb.append("\n  non-daemon thread: ").append(t.getName());
            }
        }
        return sb.toString();
    }

    private static String stuckCloseThreads() {
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<Thread, StackTraceElement[]> e : Thread.getAllStackTraces().entrySet()) {
            if (e.getKey().getName().startsWith("amqp-session-close") || e.getKey().getName().startsWith("amqp-queue-delete")) {
                sb.append("\n  ").append(e.getKey().getName()).append(" at ");
                StackTraceElement[] st = e.getValue();
                for (int i = 0; i < st.length && i < 12; i++) {
                    if (st[i].getClassName().startsWith("com.onedroid") || st[i].getClassName().startsWith("com.rabbitmq.client.impl.ChannelN")) {
                        sb.append("\n      ").append(st[i]);
                    }
                }
            }
        }
        return sb.toString();
    }

    private void assertNoSessionConnectionIsOpen(List<Session> sessions, long elapsedMs) {
        int open = 0;
        for (Session s : sessions) {
            if (s.connection.isOpen()) {
                open++;
            }
        }
        assertEquals(0, open, open + " of " + sessions.size() + " connections were still open after teardown (" + elapsedMs
                + "ms); an open connection keeps its non-daemon NIO threads, so the JVM cannot exit." + stuckCloseThreads()
                + nonDaemonThreads());
    }

    @Test
    void teardownOfManyBlockedSessionsIsBoundedNotNTimesTheCloseTimeout() throws Exception {
        broker = new DeafBrokerFake("low on memory");
        List<Session> sessions = openSessions(50);
        blockEverySession(sessions, true);

        long ms = teardownMs();
        System.out.println("TEARDOWN[50 blocked sessions]: " + ms + "ms (bound " + TEARDOWN_BOUND_MS + "ms; serial would be "
                + 50 * AmqpPublishSampler.CLOSE_TIMEOUT_MS + "ms)");

        assertTrue(ms < TEARDOWN_BOUND_MS, "teardown of 50 blocked sessions took " + ms + "ms, bound " + TEARDOWN_BOUND_MS + "ms"
                + stuckCloseThreads());
        assertNoSessionConnectionIsOpen(sessions, ms);
        assertEquals(0, SessionRegistry.size());
    }

    /**
     * The property the executor needs: after teardown NO connection is open, whatever time it took. An open
     * connection keeps the shared NIO loop's non-daemon threads alive and JMeter's JVM does not exit.
     */
    @Test
    void teardownLeavesNoBlockedSessionConnectionOpen() throws Exception {
        broker = new DeafBrokerFake("low on memory");
        List<Session> sessions = openSessions(20);
        blockEverySession(sessions, true);

        long ms = teardownMs();
        System.out.println("TEARDOWN[20 blocked sessions, open-connection check]: " + ms + "ms");

        assertNoSessionConnectionIsOpen(sessions, ms);
    }

    @Test
    void teardownOfManySessionsTheBrokerWentDeafOnWithoutAnnouncingIsBounded() throws Exception {
        broker = new DeafBrokerFake("unannounced", false);
        List<Session> sessions = openSessions(30);
        blockEverySession(sessions, false);

        long ms = teardownMs();
        System.out.println("TEARDOWN[30 silently deaf sessions]: " + ms + "ms (bound " + TEARDOWN_BOUND_MS + "ms)");

        assertTrue(ms < TEARDOWN_BOUND_MS, "teardown of 30 silently deaf sessions took " + ms + "ms, bound " + TEARDOWN_BOUND_MS + "ms"
                + stuckCloseThreads());
        assertNoSessionConnectionIsOpen(sessions, ms);
    }

    @Test
    void aHealthyBrokerStillGetsEverySessionsQueueDeleted() throws Exception {
        broker = new DeafBrokerFake("never blocked");
        List<Session> sessions = openSessions(20);

        long ms = teardownMs();
        System.out.println("TEARDOWN[20 healthy sessions]: " + ms + "ms");

        assertEquals(20, broker.queueDeletesSeen(), "a healthy teardown must still delete each session's own queue");
        assertNoSessionConnectionIsOpen(sessions, ms);
        assertTrue(ms < AmqpPublishSampler.CLOSE_TIMEOUT_MS, "a healthy teardown must not wait out the bound: " + ms + "ms");
    }
}
