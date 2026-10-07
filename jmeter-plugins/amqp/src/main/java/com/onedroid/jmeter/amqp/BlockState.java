package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.BlockedListener;

import java.util.function.LongSupplier;

/**
 * What the broker has told one connection about {@code connection.blocked} / {@code connection.unblocked}
 * (RabbitMQ sends them when a memory or disk alarm blocks publishers). The Java twin of the Go engine's
 * {@code watchBlocked} / {@code blockedNote} (internal/amqpengine/guard.go): the sampler asks this
 * object, before it publishes, whether the connection is blocked.
 *
 * Thread-safety: the listener callbacks run on the client's connection thread, the sampler reads on the
 * JMeter thread, so every accessor is synchronized.
 */
final class BlockState {

    private final LongSupplier clock;
    private boolean blocked;
    private String reason = "";
    private long sinceMs;
    private long totalClosedMs;
    private int periods;
    /** Length of the last finished block, until the next successful sample reports it once; -1 = none. */
    private long pendingWasBlockedMs = -1;

    BlockState() {
        this(System::currentTimeMillis);
    }

    BlockState(LongSupplier clockMs) {
        this.clock = clockMs;
    }

    /** The listener to register with {@code Connection.addBlockedListener}. */
    BlockedListener listener() {
        return new BlockedListener() {
            @Override
            public void handleBlocked(String why) {
                onBlocked(why);
            }

            @Override
            public void handleUnblocked() {
                onUnblocked();
            }
        };
    }

    synchronized void onBlocked(String why) {
        if (blocked) {
            reason = normalize(why);
            return;
        }
        blocked = true;
        reason = normalize(why);
        sinceMs = clock.getAsLong();
        periods++;
    }

    synchronized void onUnblocked() {
        if (!blocked) {
            return;
        }
        long ms = Math.max(0, clock.getAsLong() - sinceMs);
        blocked = false;
        totalClosedMs += ms;
        pendingWasBlockedMs = ms;
    }

    synchronized boolean isBlocked() {
        return blocked;
    }

    synchronized String reason() {
        return reason;
    }

    synchronized long sinceMs() {
        return sinceMs;
    }

    synchronized int periods() {
        return periods;
    }

    /** Total time spent blocked, an open period included. */
    synchronized long totalBlockedMs() {
        return totalClosedMs + (blocked ? Math.max(0, clock.getAsLong() - sinceMs) : 0);
    }

    /** Returns the length of the last finished block once, then -1 until the next one finishes. */
    synchronized long takeWasBlockedMs() {
        long v = pendingWasBlockedMs;
        pendingWasBlockedMs = -1;
        return v;
    }

    /** The sample message while blocked; the Go side reads {@code since=<epoch ms>} from it. */
    synchronized String blockedMessage() {
        return "blocked by broker: " + reason + " (since=" + sinceMs + ")";
    }

    private static String normalize(String why) {
        return why == null || why.trim().isEmpty() ? "no reason given" : why.trim();
    }
}
