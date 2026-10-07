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
 * JMeter Java Request sampler that publishes ONE message to a <b>topic</b>
 * exchange with a concrete routing key and verifies the broker's
 * pattern-matching routing: specifically, that every queue listed in
 * {@code match_queues} received the message (positive assertion) AND that
 * no queue listed in {@code no_match_queues} received it (negative
 * assertion). This is the pub/sub companion to {@link AmqpFanoutSampler}
 * — same shape, but for topic exchanges where wildcard bindings
 * ({@code *} = one word, {@code #} = zero or more) decide delivery.
 *
 * <p>Queue expectation: all queues named in {@code match_queues} and
 * {@code no_match_queues} must already be bound to the topic exchange by
 * the caller's JMeter test plan (typically a Setup Thread Group that
 * declares the exchange, queues, and their binding patterns). This sampler
 * does not declare or bind anything — it assumes the routing topology is
 * in place. To avoid disturbing production traffic, use dedicated test
 * queues; the sampler polls with {@code autoAck=true} and will drain any
 * non-matching messages that happen to be on the polled queues.
 *
 * <p>Timing: {@code sampleStart()} fires just before {@code basicPublish};
 * {@code sampleEnd()} fires when verification completes — either when all
 * match queues have matched AND the no-match settle window has elapsed, or
 * when the deadline is hit, or when a no-match queue is observed receiving
 * the message (immediate fail).
 *
 * <p>Polling strategy: interleaved round-robin across match queues + all
 * no-match queues at {@value #POLL_INTERVAL_MS}ms. Match queues are
 * removed from the pending set on match. No-match queues stay in the poll
 * set and are checked every sweep. Once all match queues have matched,
 * the sampler holds a {@code settle_ms} window polling only the no-match
 * queues — the broker's routing decision is made at publish time, so any
 * no-match arrival would already be in flight; settle_ms bounds how long
 * we're willing to wait for that in-flight signal.
 *
 * <p>Add this jar to {@code JMETER_HOME/lib/ext}, then in JMeter:
 * Thread Group &rarr; Sampler &rarr; Java Request,
 * classname {@code com.onedroid.jmeter.amqp.AmqpPubSubSampler}.
 *
 * <p>Parameters:
 * <ul>
 *   <li>{@code amqp_uri} — AMQP connection URI</li>
 *   <li>{@code username} / {@code password} — broker credentials (optional if in URI)</li>
 *   <li>{@code exchange} — topic exchange name (required, non-blank)</li>
 *   <li>{@code routing_key} — concrete routing key to publish with (required, non-blank)</li>
 *   <li>{@code message_body} — payload (UTF-8)</li>
 *   <li>{@code content_type} — MIME type</li>
 *   <li>{@code match_queues} — comma-separated queue names that should ALL receive</li>
 *   <li>{@code no_match_queues} — comma-separated queue names that should NONE receive</li>
 *   <li>{@code timeout_ms} — total budget for match queues (default 30000)</li>
 *   <li>{@code settle_ms} — extra window to confirm no-match queues stay empty (default 200)</li>
 *   <li>{@code connect_timeout_ms} — socket connect timeout (default 5000)</li>
 * </ul>
 *
 * <p>At least one of {@code match_queues} or {@code no_match_queues} must
 * be non-empty, otherwise the sample short-circuits to 400 — a run with
 * nothing to verify would be a silent false-positive.
 *
 * <p>Response codes:
 * <ul>
 *   <li>{@code 200} — all match queues received; no no-match queue received. Verification passed.</li>
 *   <li>{@code 408} — at least one match queue failed to receive within the deadline. Timing-based failure.</li>
 *   <li>{@code 409} — at least one no-match queue unexpectedly received the message. Routing contract violation.</li>
 *   <li>{@code 400} — config error (blank exchange, blank routing_key, or both queue lists empty)</li>
 *   <li>{@code 500} — channel not open, publish failure, or other AMQP exception</li>
 * </ul>
 * When both 408 and 409 conditions fire, 409 wins — a routing violation
 * is the more important signal than missing matches.
 */
public class AmqpPubSubSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_ROUTING_KEY = "routing_key";
    private static final String PARAM_MESSAGE_BODY = "message_body";
    private static final String PARAM_CONTENT_TYPE = "content_type";
    private static final String PARAM_MATCH_QUEUES = "match_queues";
    private static final String PARAM_NO_MATCH_QUEUES = "no_match_queues";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_SETTLE_MS = "settle_ms";
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
        args.addArgument(PARAM_ROUTING_KEY, "");
        args.addArgument(PARAM_MESSAGE_BODY, "");
        args.addArgument(PARAM_CONTENT_TYPE, "application/json");
        args.addArgument(PARAM_MATCH_QUEUES, "");
        args.addArgument(PARAM_NO_MATCH_QUEUES, "");
        args.addArgument(PARAM_TIMEOUT_MS, "30000");
        args.addArgument(PARAM_SETTLE_MS, "200");
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

            this.connection = factory.newConnection("jmeter-amqp-pubsub-sampler");
            this.channel = connection.createChannel();
        } catch (Exception e) {
            log.error("AMQP pubsub setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP PubSub");

        String exchange = context.getParameter(PARAM_EXCHANGE);
        String routingKey = context.getParameter(PARAM_ROUTING_KEY);
        String messageBody = context.getParameter(PARAM_MESSAGE_BODY);
        String contentType = context.getParameter(PARAM_CONTENT_TYPE);
        String matchRaw = context.getParameter(PARAM_MATCH_QUEUES);
        String noMatchRaw = context.getParameter(PARAM_NO_MATCH_QUEUES);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 30000);
        int settleMs = parseInt(context.getParameter(PARAM_SETTLE_MS), 200);

        // Config validation before sampleStart. Mirrors CAT-2615/2617:
        // config errors are zero-duration samples.
        if (exchange == null || exchange.trim().isEmpty()) {
            return fail400(result, "exchange is required (topic exchange name)");
        }
        if (routingKey == null || routingKey.trim().isEmpty()) {
            return fail400(result, "routing_key is required (concrete key, not a pattern)");
        }

        List<String> matchQueues = parseQueueList(matchRaw);
        List<String> noMatchQueues = parseQueueList(noMatchRaw);

        if (matchQueues.isEmpty() && noMatchQueues.isEmpty()) {
            return fail400(result,
                    "at least one of match_queues or no_match_queues must be non-empty "
                            + "(nothing to verify otherwise)");
        }

        byte[] payload = messageBody == null ? new byte[0] : messageBody.getBytes(StandardCharsets.UTF_8);
        if (contentType == null || contentType.isEmpty()) {
            contentType = "application/octet-stream";
        }

        String correlationId = UUID.randomUUID().toString();

        // sampleStart before channel-open check — 500 path has well-defined
        // timing (CAT-2614 pattern).
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

            channel.basicPublish(exchange, routingKey, props, payload);

            long deadline = startMs + Math.max(0, timeoutMs);
            Map<String, QueueOutcome> outcomes = verifyRouting(
                    matchQueues, noMatchQueues, correlationId,
                    startMs, deadline, Math.max(0, settleMs));

            // Tally
            List<String> missingMatch = new ArrayList<String>();
            List<String> unexpectedMatch = new ArrayList<String>();
            for (String q : matchQueues) {
                if (!outcomes.get(q).received) missingMatch.add(q);
            }
            for (String q : noMatchQueues) {
                if (outcomes.get(q).received) unexpectedMatch.add(q);
            }

            result.sampleEnd();
            boolean allMatchHit = missingMatch.isEmpty();
            boolean noUnexpected = unexpectedMatch.isEmpty();

            if (allMatchHit && noUnexpected) {
                result.setSuccessful(true);
                result.setResponseCode("200");
                result.setResponseMessage("PubSub OK — "
                        + matchQueues.size() + " match queues received, "
                        + noMatchQueues.size() + " no-match queues stayed empty, "
                        + "correlationId=" + correlationId);
            } else if (!noUnexpected) {
                // 409 takes priority over 408 — routing contract violation
                // is the stronger finding.
                result.setSuccessful(false);
                result.setResponseCode("409");
                result.setResponseMessage("PubSub routing violation — "
                        + "unexpected matches on no-match queues: " + unexpectedMatch
                        + (missingMatch.isEmpty() ? "" : "; also missing=" + missingMatch)
                        + ", correlationId=" + correlationId);
            } else {
                result.setSuccessful(false);
                result.setResponseCode("408");
                result.setResponseMessage("PubSub incomplete — "
                        + (matchQueues.size() - missingMatch.size()) + "/"
                        + matchQueues.size() + " match queues received; missing="
                        + missingMatch + ", correlationId=" + correlationId);
            }

            result.setResponseData(new byte[0]);
            result.setDataType(SampleResult.TEXT);
            result.setResponseHeaders(buildHeaders(
                    correlationId, exchange, routingKey, matchQueues, noMatchQueues, outcomes));
            return result;
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("PubSub failed: " + e.getClass().getSimpleName()
                    + ": " + e.getMessage());
            log.error("AMQP pubsub failed: " + e.getMessage(), e);
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

    // --- verification loop ----------------------------------------------

    /**
     * Two-phase verification:
     *   Phase 1 — poll ALL queues (match + no-match) round-robin until all
     *             match queues match or the deadline hits. Match queues
     *             drop out of the pending set on match. No-match queues
     *             are also polled every sweep; if one matches the
     *             correlationId, that's a routing-contract violation
     *             recorded as {@code received=true}, and we exit early.
     *   Phase 2 — once all match queues match, hold a settle window of
     *             {@code settleMs}, polling only no-match queues. If any
     *             no-match queue sees the correlationId during this
     *             window, record it. Phase 2 does NOT short-circuit on
     *             violation — we want the full settle window to see ALL
     *             violators, not just the first one.
     */
    private Map<String, QueueOutcome> verifyRouting(
            List<String> matchQueues,
            List<String> noMatchQueues,
            String correlationId,
            long startMs,
            long deadline,
            int settleMs) throws Exception {

        Map<String, QueueOutcome> outcomes = new LinkedHashMap<String, QueueOutcome>();
        for (String q : matchQueues) {
            outcomes.put(q, new QueueOutcome("match", false, -1, 0));
        }
        for (String q : noMatchQueues) {
            outcomes.put(q, new QueueOutcome("no_match", false, -1, 0));
        }

        // Phase 1: poll all queues until all match queues have matched or
        // deadline hits. Match queues leave the pending set on match;
        // no-match queues stay in the pending set throughout Phase 1 since
        // a violation can arrive at any time during this window.
        Set<String> pendingMatch = new LinkedHashSet<String>(matchQueues);

        while (true) {
            boolean sawViolation = false;
            for (String queue : outcomes.keySet()) {
                // Skip match queues that already matched — we don't want
                // to re-poll them and accidentally consume something
                // unrelated.
                if ("match".equals(outcomes.get(queue).role) && !pendingMatch.contains(queue)) {
                    continue;
                }
                GetResponse resp = channel.basicGet(queue, /* autoAck */ true);
                if (resp == null) continue;

                QueueOutcome outcome = outcomes.get(queue);
                outcome.nonMatchDrained++;

                String replyCid = resp.getProps() != null ? resp.getProps().getCorrelationId() : null;
                if (correlationId.equals(replyCid)) {
                    outcome.received = true;
                    outcome.latencyMs = System.currentTimeMillis() - startMs;
                    outcome.nonMatchDrained--;
                    if ("match".equals(outcome.role)) {
                        pendingMatch.remove(queue);
                    } else {
                        // Violation — noted, keep Phase 1 going to see
                        // the full state at deadline, but we no longer
                        // need to stay in the match-waiting loop.
                        sawViolation = true;
                    }
                }
            }

            if (pendingMatch.isEmpty() || sawViolation) break;
            if (System.currentTimeMillis() >= deadline) break;
            sleepQuietly(POLL_INTERVAL_MS);
        }

        // Phase 2: if every match queue matched, spend settle_ms polling
        // no-match queues to confirm the broker didn't route them
        // anything. If we already saw a violation or missed a match
        // queue, skip the settle — the outcome is already decided.
        if (pendingMatch.isEmpty() && !noMatchQueues.isEmpty()) {
            long settleDeadline = System.currentTimeMillis() + settleMs;
            while (System.currentTimeMillis() < settleDeadline) {
                for (String queue : noMatchQueues) {
                    GetResponse resp = channel.basicGet(queue, /* autoAck */ true);
                    if (resp == null) continue;

                    QueueOutcome outcome = outcomes.get(queue);
                    outcome.nonMatchDrained++;

                    String replyCid = resp.getProps() != null ? resp.getProps().getCorrelationId() : null;
                    if (correlationId.equals(replyCid)) {
                        outcome.received = true;
                        outcome.latencyMs = System.currentTimeMillis() - startMs;
                        outcome.nonMatchDrained--;
                        // Don't break — keep polling to surface ALL
                        // violators, not just the first.
                    }
                }
                sleepQuietly(POLL_INTERVAL_MS);
            }
        }

        return outcomes;
    }

    // --- helpers --------------------------------------------------------

    private static SampleResult fail400(SampleResult result, String message) {
        result.sampleStart();
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode("400");
        result.setResponseMessage(message);
        return result;
    }

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

    private static String buildHeaders(String correlationId,
                                        String exchange,
                                        String routingKey,
                                        List<String> matchQueues,
                                        List<String> noMatchQueues,
                                        Map<String, QueueOutcome> outcomes) {
        StringBuilder sb = new StringBuilder(384);
        sb.append("correlationId=").append(correlationId).append('\n');
        sb.append("exchange=").append(exchange).append('\n');
        sb.append("routingKey=").append(routingKey).append('\n');
        sb.append("matchCount=").append(matchQueues.size()).append('\n');
        sb.append("noMatchCount=").append(noMatchQueues.size()).append('\n');
        int i = 0;
        for (Map.Entry<String, QueueOutcome> e : outcomes.entrySet()) {
            String prefix = "queue." + i + ".";
            QueueOutcome o = e.getValue();
            boolean passed = "match".equals(o.role) ? o.received : !o.received;
            sb.append(prefix).append("name=").append(e.getKey()).append('\n');
            sb.append(prefix).append("role=").append(o.role).append('\n');
            sb.append(prefix).append("received=").append(o.received).append('\n');
            sb.append(prefix).append("passed=").append(passed).append('\n');
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
        final String role;          // "match" or "no_match"
        boolean received;
        long latencyMs;
        int nonMatchDrained;

        QueueOutcome(String role, boolean received, long latencyMs, int nonMatchDrained) {
            this.role = role;
            this.received = received;
            this.latencyMs = latencyMs;
            this.nonMatchDrained = nonMatchDrained;
        }
    }
}
