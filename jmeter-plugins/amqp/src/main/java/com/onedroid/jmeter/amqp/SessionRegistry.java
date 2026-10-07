package com.onedroid.jmeter.amqp;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * The static registry of load sessions, keyed by the {@code session} string the template gives to the
 * session setup sampler, the publisher and the subscriber of one JMeter thread (design §2.C, fact 1).
 *
 * JMeter calls {@code teardownTest} on a FRESH client instance at test end, so nothing per-instance can
 * close a session: {@link #closeAll()} closes whatever is registered, and is idempotent.
 */
final class SessionRegistry {

    private static final ConcurrentMap<String, Session> SESSIONS = new ConcurrentHashMap<String, Session>();

    private SessionRegistry() {
    }

    static Session get(String key) {
        return key == null ? null : SESSIONS.get(key);
    }

    /** Registers {@code s} unless an open session already holds the key; returns the session that holds it. */
    static Session putIfAbsent(Session s) {
        while (true) {
            Session prev = SESSIONS.putIfAbsent(s.key, s);
            if (prev == null) {
                return s;
            }
            if (prev.isOpen()) {
                return prev;
            }
            SESSIONS.remove(s.key, prev); // a dead session under the key: replace it
        }
    }

    static void remove(Session s) {
        SESSIONS.remove(s.key, s);
    }

    static int size() {
        return SESSIONS.size();
    }

    /** Closes every registered session once (queue delete + connection close, each bounded), then forgets them. */
    static int closeAll() {
        final List<Session> all = new ArrayList<Session>(SESSIONS.values());
        SESSIONS.clear();
        if (all.isEmpty()) {
            return 0;
        }
        final AtomicInteger closed = new AtomicInteger();
        final CountDownLatch done = new CountDownLatch(all.size());
        int n = 0;
        for (final Session s : all) {
            // One daemon thread per session. A blocked broker answers nothing, so every
            // session spends its whole bound; only running them all at once keeps the total independent of how
            // many there are. (16 workers closing 500 blocked sessions in turn was minutes, and a session the
            // workers never reached was never aborted, which kept the JVM alive.)
            Thread t = new Thread(new Runnable() {
                @Override
                public void run() {
                    try {
                        if (s.close(AmqpPublishSampler.CLOSE_TIMEOUT_MS)) {
                            closed.incrementAndGet();
                        }
                    } finally {
                        done.countDown();
                    }
                }
            }, "amqp-session-close-" + (++n));
            t.setDaemon(true);
            t.start();
        }
        try {
            // bounded as a whole: the test must still end if a closer is stuck
            done.await(CLOSE_ALL_BOUND_MS, TimeUnit.MILLISECONDS);
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
        }
        return closed.get();
    }

    /**
     * The longest {@link #closeAll()} waits for its closers: one session's queue-delete wait plus its close
     * wait ({@link Session#close}), plus slack. Sessions close in parallel, so it does not grow with their number.
     */
    static final long CLOSE_ALL_BOUND_MS = 2L * AmqpPublishSampler.CLOSE_TIMEOUT_MS + 2000L;
}
