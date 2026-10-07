package com.onedroid.jmeter.amqp;

import org.apache.jorphan.logging.LoggingManager;
import org.apache.log.Logger;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;
import com.rabbitmq.client.GetResponse;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;

/**
 * JMeter Java Request sampler that publishes ONE message to a fanout exchange
 * and then verifies the broker fanned it out to every queue named in
 * {@code verify_queues}. Report succeeds only if ALL queues see a message
 * with the matching correlationId before the deadline.
 *
 * <p>Queue expectation: the queues listed in {@code verify_queues} must
 * already be bound to the fanout exchange by the user's JMeter test plan
 * (typically a Setup Thread Group that declares and binds them). This
 * sampler does not declare or bind queues — it assumes the test topology
 * is in place. To avoid disturbing production traffic, use dedicated test
 * queues bound specifically for the run; the sampler polls with
 * {@code autoAck=true} and will drain non-matching messages that happen to
 * be on the same queue.
 *
 * <p>Timing: {@code sampleStart()} fires just before {@code basicPublish},
 * {@code sampleEnd()} fires when the last queue matches (or the shared
 * deadline is hit). The reported duration is therefore the end-to-end
 * fan-out latency — the time from publish to the LAST queue receiving.
 *
 * <p>Polling strategy: interleaved round-robin across the pending queues
 * at {@value #POLL_INTERVAL_MS}ms between sweeps. Each queue is polled
 * independently; matches are remembered per-queue, so an early-arriving
 * match isn't re-polled while slower queues catch up.
 *
 * <p>Add this jar to {@code JMETER_HOME/lib/ext}, then in JMeter:
 * Thread Group &rarr; Sampler &rarr; Java Request,
 * classname {@code com.onedroid.jmeter.amqp.AmqpFanoutSampler}.
 *
 * <p>Parameters:
 * <ul>
 *   <li>{@code amqp_uri} — AMQP connection URI</li>
 *   <li>{@code username} / {@code password} — broker credentials (optional if in URI)</li>
 *   <li>{@code exchange} — fanout exchange name (required, non-blank)</li>
 *   <li>{@code message_body} — payload (UTF-8)</li>
 *   <li>{@code content_type} — MIME type</li>
 *   <li>{@code verify_queues} — comma-separated queue names to check (required, non-empty)</li>
 *   <li>{@code timeout_ms} — total budget for ALL queues to see the message (default 30000)</li>
 *   <li>{@code connect_timeout_ms} — socket connect timeout (default 5000)</li>
 * </ul>
 *
 * <p>Response codes:
 * <ul>
 *   <li>200 — every queue received the message within the deadline</li>
 *   <li>408 — at least one queue timed out; responseHeaders lists which</li>
 *   <li>400 — config error (blank exchange, empty verify_queues)</li>
 *   <li>500 — channel not open, publish failure, or other AMQP exception</li>
 * </ul>
 */
public class AmqpFanoutSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_MESSAGE_BODY = "message_body";
    private static final String PARAM_CONTENT_TYPE = "content_type";
    private static final String PARAM_VERIFY_QUEUES = "verify_queues";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_CONNECT_TIMEOUT_MS = "connect_timeout_ms";

    private static final int POLL_INTERVAL_MS = 50;

    private static final Logger log = LoggingManager.getLoggerForClass();

    private Connection connection;
    private Channel channel;

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_URI, "amqp://localhost:5672");
        args.addArgument(PARAM_USERNAME, "");
        args.addArgument(PARAM_PASSWORD, "");
        args.addArgument(PARAM_EXCHANGE, "");
        args.addArgument(PARAM_MESSAGE_BODY, "");
        args.addArgument(PARAM_CONTENT_TYPE, "application/json");
        args.addArgument(PARAM_VERIFY_QUEUES, "");
        args.addArgument(PARAM_TIMEOUT_MS, "30000");
        args.addArgument(PARAM_CONNECT_TIMEOUT_MS, "5000");
        return args;
    }

    @Override
    public void setupTest(JavaSamplerContext context) {
        String uri = context.getParameter(PARAM_URI);
        String username = context.getParameter(PARAM_USERNAME);
        String password = context.getParameter(PARAM_PASSWORD);
        int connectTimeoutMs = parseInt(context.getParameter(PARAM_CONNECT_TIMEOUT_MS), 5000);

        try {
            ConnectionFactory factory = new ConnectionFactory();
            factory.setUri(uri);

            if (username != null && !username.trim().isEmpty()) {
                factory.setUsername(username);
            }
            if (password != null && !password.trim().isEmpty()) {
                factory.setPassword(password);
            }

            factory.setConnectionTimeout(connectTimeoutMs);

            this.connection = factory.newConnection("jmeter-amqp-fanout-sampler");
            this.channel = connection.createChannel();
        } catch (Exception e) {
            log.error("AMQP fanout setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP Fanout");

        String exchange = context.getParameter(PARAM_EXCHANGE);
        String messageBody = context.getParameter(PARAM_MESSAGE_BODY);
        String contentType = context.getParameter(PARAM_CONTENT_TYPE);
        String verifyQueuesRaw = context.getParameter(PARAM_VERIFY_QUEUES);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 30000);

        // Config validation before sampleStart — mirrors the CAT-2615
        // AmqpRpcSampler pattern. Config errors are zero-duration samples.
        if (exchange == null || exchange.trim().isEmpty()) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage("exchange is required (fanout exchange name)");
            return result;
        }

        List<String> verifyQueues = parseQueueList(verifyQueuesRaw);
        if (verifyQueues.isEmpty()) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage("verify_queues is required (comma-separated list)");
            return result;
        }

        byte[] payload = messageBody == null ? new byte[0] : messageBody.getBytes(StandardCharsets.UTF_8);
        if (contentType == null || contentType.isEmpty()) {
            contentType = "application/octet-stream";
        }

        String correlationId = UUID.randomUUID().toString();

        // sampleStart before channel-open check so 500 samples have
        // well-defined timing (CAT-2614 pattern carried forward).
        result.sampleStart();
        long startMs = System.currentTimeMillis();

        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException("AMQP channel is not open (setupTest likely failed).");
            }

            AMQP.BasicProperties props = new AMQP.BasicProperties.Builder()
                    .correlationId(correlationId)
                    .contentType(contentType)
                    .build();

            // Fanout exchanges ignore routing_key; pass "" explicitly so this
            // works even if the user accidentally wired a non-fanout exchange
            // expecting a key.
            channel.basicPublish(exchange, "", props, payload);

            long deadline = startMs + Math.max(0, timeoutMs);
            Map<String, QueueOutcome> outcomes = pollUntilAllMatchOrDeadline(
                    verifyQueues, correlationId, startMs, deadline);

            int matched = 0;
            List<String> missing = new ArrayList<String>();
            for (Map.Entry<String, QueueOutcome> e : outcomes.entrySet()) {
                if (e.getValue().matched) {
                    matched++;
                } else {
                    missing.add(e.getKey());
                }
            }

            result.sampleEnd();
            boolean allMatched = missing.isEmpty();
            result.setSuccessful(allMatched);
            result.setResponseCode(allMatched ? "200" : "408");
            result.setResponseMessage(allMatched
                    ? ("Fanout OK — " + matched + "/" + verifyQueues.size()
                            + " queues matched, correlationId=" + correlationId)
                    : ("Fanout incomplete — " + matched + "/" + verifyQueues.size()
                            + " queues matched; missing=" + missing
                            + ", correlationId=" + correlationId));
            result.setResponseData(new byte[0]);
            result.setDataType(SampleResult.TEXT);
            result.setResponseHeaders(buildHeaders(correlationId, exchange, outcomes));
            return result;
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("Fanout failed: " + e.getClass().getSimpleName()
                    + ": " + e.getMessage());
            log.error("AMQP fanout failed: " + e.getMessage(), e);
            return result;
        }
    }

    @Override
    public void teardownTest(JavaSamplerContext context) {
        try {
            if (channel != null && channel.isOpen()) channel.close();
        } catch (Exception ignored) { }
        try {
            if (connection != null && connection.isOpen()) connection.close();
        } catch (Exception ignored) { }
    }

    // --- polling ---------------------------------------------------------

    /**
     * Interleaved round-robin poll across pending queues. Each sweep calls
     * basicGet once per still-pending queue; on match we record the latency
     * and drop the queue from the pending set. Non-matching messages are
     * consumed by autoAck=true — hence the doc contract that callers should
     * use dedicated test queues.
     */
    private Map<String, QueueOutcome> pollUntilAllMatchOrDeadline(
            List<String> verifyQueues,
            String correlationId,
            long startMs,
            long deadline) throws Exception {

        Map<String, QueueOutcome> outcomes = new LinkedHashMap<String, QueueOutcome>();
        for (String q : verifyQueues) {
            outcomes.put(q, new QueueOutcome(false, -1, 0));
        }
        Set<String> pending = new LinkedHashSet<String>(verifyQueues);

        while (!pending.isEmpty()) {
            for (Iterator<String> it = pending.iterator(); it.hasNext(); ) {
                String queue = it.next();
                GetResponse resp = channel.basicGet(queue, /* autoAck */ true);

                if (resp == null) {
                    continue;
                }

                QueueOutcome outcome = outcomes.get(queue);
                outcome.nonMatchDrained++;

                String replyCid = resp.getProps() != null ? resp.getProps().getCorrelationId() : null;
                if (correlationId.equals(replyCid)) {
                    outcome.matched = true;
                    outcome.latencyMs = System.currentTimeMillis() - startMs;
                    // The matching message itself doesn't count against the
                    // drain counter — reset to reflect only non-matches.
                    outcome.nonMatchDrained--;
                    it.remove();
                }
            }
            if (pending.isEmpty()) {
                break;
            }
            if (System.currentTimeMillis() >= deadline) {
                break;
            }
            sleepQuietly(POLL_INTERVAL_MS);
        }

        return outcomes;
    }

    // --- helpers ---------------------------------------------------------

    static List<String> parseQueueList(String raw) {
        List<String> out = new ArrayList<String>();
        if (raw == null) return out;
        for (String token : raw.split(",")) {
            String trimmed = token.trim();
            if (!trimmed.isEmpty()) {
                out.add(trimmed);
            }
        }
        return out;
    }

    private static String buildHeaders(String correlationId, String exchange,
                                        Map<String, QueueOutcome> outcomes) {
        StringBuilder sb = new StringBuilder(256);
        sb.append("correlationId=").append(correlationId).append('\n');
        sb.append("exchange=").append(exchange).append('\n');
        sb.append("queueCount=").append(outcomes.size()).append('\n');
        int i = 0;
        for (Map.Entry<String, QueueOutcome> e : outcomes.entrySet()) {
            String prefix = "queue." + i + ".";
            QueueOutcome o = e.getValue();
            sb.append(prefix).append("name=").append(e.getKey()).append('\n');
            sb.append(prefix).append("matched=").append(o.matched).append('\n');
            sb.append(prefix).append("latencyMs=").append(o.latencyMs).append('\n');
            sb.append(prefix).append("nonMatchDrained=").append(o.nonMatchDrained).append('\n');
            i++;
        }
        return sb.toString();
    }

    private static int parseInt(String s, int defaultVal) {
        try { return Integer.parseInt(s); }
        catch (Exception e) { return defaultVal; }
    }

    private static void sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    // Visible for test. Lets unit tests inject a mock channel without
    // running setupTest() (which requires a real AMQP connection).
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }

    /**
     * Per-queue outcome tracker. Mutable so the polling loop can update
     * entries in place without rebuilding the map.
     */
    static final class QueueOutcome {
        boolean matched;
        long latencyMs;
        int nonMatchDrained;

        QueueOutcome(boolean matched, long latencyMs, int nonMatchDrained) {
            this.matched = matched;
            this.latencyMs = latencyMs;
            this.nonMatchDrained = nonMatchDrained;
        }
    }
}
