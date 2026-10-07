package com.onedroid.jmeter.amqp;

import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;

import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

/**
 * Helpers the live session/subscribe tests share. Every queue a live test declares carries x-expires (the
 * broker reaps it even if the run is killed) and is deleted by {@link #cleanup}.
 */
final class LiveSessions {

    static final int EXPIRES_MS = 120_000;
    private final AmqpLiveHarness broker;
    private final List<String> queues = new ArrayList<String>();

    LiveSessions(AmqpLiveHarness broker) {
        this.broker = broker;
    }

    /** A fresh run id: a run never reuses another run's queue because the id is in the name. */
    static String newRunId() {
        return "live" + UUID.randomUUID().toString().substring(0, 8);
    }

    static String key(String runId, int thread) {
        return runId + "-s1-" + thread;
    }

    static String queueName(String runId, int thread) {
        return "argus-load-" + runId + "-s1-" + String.format("%04d", thread);
    }

    static String routingKey(String runId, int thread) {
        return "argus-load." + runId + ".1." + String.format("%04d", thread);
    }

    JavaSamplerContext setup(String runId, int thread, String... kv) {
        queues.add(queueName(runId, thread));
        TestCtx c = TestCtx.params()
                .with("amqp_uri", broker.samplerUri())
                .with("session", key(runId, thread))
                .with("exchange", "amq.direct")
                .with("routing_key", routingKey(runId, thread))
                .with("queue", queueName(runId, thread))
                .with("queue_expires_ms", String.valueOf(EXPIRES_MS))
                .with("label", "live-amqp-setup");
        for (int i = 0; i < kv.length; i += 2) {
            c.with(kv[i], kv[i + 1]);
        }
        return c.buildFor(new AmqpSessionSetupSampler().getDefaultParameters());
    }

    JavaSamplerContext publish(String runId, int thread, String... kv) {
        TestCtx c = TestCtx.params()
                .with("amqp_uri", broker.samplerUri())
                .with("session", key(runId, thread))
                .with("exchange", "amq.direct")
                .with("routing_key", routingKey(runId, thread))
                .with("message_size_bytes", "1000")
                .with("confirm", "each")
                .with("label", "live-amqp-publish");
        for (int i = 0; i < kv.length; i += 2) {
            c.with(kv[i], kv[i + 1]);
        }
        return c.build();
    }

    static JavaSamplerContext subscribe(String runId, int thread, String... kv) {
        TestCtx c = TestCtx.params()
                .with("session", key(runId, thread))
                .with("timeout_ms", "5000")
                .with("label", "live-amqp-deliver");
        for (int i = 0; i < kv.length; i += 2) {
            c.with(kv[i], kv[i + 1]);
        }
        return c.buildFor(new AmqpSubscribeSampler().getDefaultParameters());
    }

    /** Closes every session (the sampler's own teardown), then deletes anything it did not. */
    void cleanup() {
        try {
            new AmqpSessionSetupSampler().teardownTest(null);
        } finally {
            for (String q : queues) {
                broker.deleteQueue(q);
            }
        }
    }

    /** Names of the queues this helper declared (for the "no leftovers" check). */
    List<String> declared() {
        return queues;
    }
}
