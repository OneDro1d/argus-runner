package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;

import java.util.concurrent.BlockingQueue;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;

/**
 * One load session (PR-C, design §2.C): ONE AMQP connection with two channels -- a publisher channel and
 * a consumer channel -- plus the per-session queue and the inbox the consumer callback fills.
 *
 * It exists because JMeter's {@code JavaSampler} runs {@code setupTest} lazily per sampler instance and
 * {@code teardownTest} only at the end of the test, and the publisher and the subscriber are two separate
 * Java Request instances in one thread. They find each other through {@link SessionRegistry} by the
 * {@code session} string.
 */
final class Session {

    /** One delivery as the consumer callback saw it. Holds the size, never the body. */
    static final class Delivery {
        final long deliveryTag;
        final int sizeBytes;
        final boolean redelivered;
        /** {@code x-argus-sent-ns} as stamped by the publisher, or null when the message carries none. */
        final Long sentNs;
        /** {@code x-argus-run}, or null. */
        final String run;
        /** {@code System.nanoTime()} AT RECEIPT (in the consumer callback), not at take. */
        final long receiptNs;
        /** {@code System.currentTimeMillis()} at the same instant. */
        final long receiptMs;

        Delivery(long deliveryTag, int sizeBytes, boolean redelivered, Long sentNs, String run,
                 long receiptNs, long receiptMs) {
            this.deliveryTag = deliveryTag;
            this.sizeBytes = sizeBytes;
            this.redelivered = redelivered;
            this.sentNs = sentNs;
            this.run = run;
            this.receiptNs = receiptNs;
            this.receiptMs = receiptMs;
        }
    }

    final String key;
    /** The token every publish of this session stamps into {@code x-argus-run}; a delivery with another is foreign. */
    final String run;
    final String queue;
    final String exchange;
    final String routingKey;
    final String queueType;
    final int prefetch;
    final boolean autoAck;
    final Connection connection;
    final Channel publisherChannel;
    final Channel consumerChannel;
    final BlockState blockState;
    final String[] secrets;
    final BlockingQueue<Delivery> inbox = new LinkedBlockingQueue<Delivery>();
    final AtomicLong foreign = new AtomicLong();
    private final AtomicBoolean closed = new AtomicBoolean();
    private boolean confirmSelected;

    Session(String key, String queue, String exchange, String routingKey, String queueType, int prefetch,
            boolean autoAck, Connection connection, Channel publisherChannel, Channel consumerChannel,
            BlockState blockState, String[] secrets) {
        this.key = key;
        this.run = key;
        this.queue = queue;
        this.exchange = exchange;
        this.routingKey = routingKey;
        this.queueType = queueType;
        this.prefetch = prefetch;
        this.autoAck = autoAck;
        this.connection = connection;
        this.publisherChannel = publisherChannel;
        this.consumerChannel = consumerChannel;
        this.blockState = blockState;
        this.secrets = secrets;
    }

    /** confirm-select is sent once per channel; the publisher asks for it the first time a confirm mode needs it. */
    synchronized void ensureConfirmSelect() throws java.io.IOException {
        if (!confirmSelected) {
            publisherChannel.confirmSelect();
            confirmSelected = true;
        }
    }

    boolean isOpen() {
        return !closed.get() && connection != null && connection.isOpen();
    }

    /**
     * Idempotent: deletes the queue (best effort) and closes the connection (bounded). Returns true for the
     * call that actually closed it, false for every later one.
     */
    boolean close(int boundMs) {
        if (!closed.compareAndSet(false, true)) {
            return false;
        }
        // a broker that BLOCKED this connection has stopped reading it, so a queue.delete is
        // never answered and (before this bound) held the closing thread until the client's 10-minute RPC
        // timeout, with the connection still open. A blocked session skips the delete (x-expires reaps the
        // queue) and every other session waits for it at most boundMs.
        boolean blocked = blockState != null && blockState.isBlocked();
        if (!blocked) {
            deleteQueueWithin(boundMs);
        }
        try {
            if (connection != null) {
                // a blocked broker never sends close-ok, so waiting the full bound for it buys nothing
                connection.abort(blocked ? Math.min(boundMs, BLOCKED_ABORT_MS) : boundMs);
            }
        } catch (Exception ignored) {
            // best effort
        }
        return true;
    }

    /** How long close() waits for the close-ok of a connection the broker has blocked. */
    static final int BLOCKED_ABORT_MS = 500;

    /**
     * Best-effort queue.delete, waited for at most {@code boundMs}. The RPC runs on its own daemon thread: the
     * client's RPC wait ignores interrupts, so the only thing that frees a stuck one is closing the connection,
     * which the caller does next ({@code abort}) whether or not the delete came back.
     */
    private void deleteQueueWithin(int boundMs) {
        if (consumerChannel == null || queue == null) {
            return;
        }
        final java.util.concurrent.FutureTask<Void> delete = new java.util.concurrent.FutureTask<Void>(
                new java.util.concurrent.Callable<Void>() {
                    @Override
                    public Void call() throws Exception {
                        if (consumerChannel.isOpen()) {
                            consumerChannel.queueDelete(queue);
                        }
                        return null;
                    }
                });
        Thread t = new Thread(delete, "amqp-queue-delete");
        t.setDaemon(true);
        t.start();
        try {
            delete.get(boundMs, java.util.concurrent.TimeUnit.MILLISECONDS);
        } catch (java.util.concurrent.TimeoutException timedOut) {
            // x-expires is the backstop for a queue we could not delete; abort() below frees the stuck thread
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
        } catch (Exception ignored) {
            // best effort
        }
    }
}
