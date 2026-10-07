package com.onedroid.jmeter.amqp;

import org.apache.jorphan.logging.LoggingManager;
import org.apache.log.Logger;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;
import com.rabbitmq.client.Consumer;
import com.rabbitmq.client.DefaultConsumer;
import com.rabbitmq.client.Envelope;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;

/**
 * JMeter Java Request sampler that implements the RabbitMQ direct reply-to
 * (request-reply) pattern. Unlike the canonical AMQP RPC pattern
 * ({@link AmqpRpcSampler}), this sampler uses the broker's built-in
 * {@code amq.rabbitmq.reply-to} pseudo-queue, which eliminates reply-queue
 * declaration, binding, and cleanup overhead entirely.
 *
 * Differences from {@link AmqpRpcSampler}:
 * <ul>
 *   <li>No queue is declared. {@code amq.rabbitmq.reply-to} is a broker-side
 *       pseudo-queue that exists per-channel.</li>
 *   <li>Consumer uses {@code autoAck=true}, which is a broker requirement for
 *       direct reply-to. The sampler issues no {@code basicAck} calls.</li>
 *   <li>Reply delivery is callback-driven (async) rather than poll-driven.
 *       The {@link DefaultConsumer} hands off each delivery into a
 *       {@link BlockingQueue} and {@code runTest} blocks on that queue with
 *       the remaining timeout budget.</li>
 *   <li>No teardown cleanup is needed — closing the channel ends the
 *       pseudo-queue and cancels the consumer.</li>
 * </ul>
 *
 * Sample timing covers the full round trip: {@code sampleStart()} fires just
 * before {@code basicPublish}, {@code sampleEnd()} fires when the matching
 * reply is received (or the deadline is reached). Reported duration IS the
 * direct-reply round-trip latency.
 *
 * Lifecycle:
 * <pre>
 *   setupTest()   — open connection + channel, install a persistent
 *                   basicConsume on amq.rabbitmq.reply-to. The consumer
 *                   lives for the duration of the thread and feeds all
 *                   iterations.
 *   runTest()     — generate correlationId, publish request, block on the
 *                   inbox queue until a delivery with the matching
 *                   correlationId arrives (or the deadline is hit).
 *                   Non-matching deliveries on the inbox are discarded
 *                   (late arrivals from a prior timed-out iteration).
 *   teardownTest() — close channel + connection. Broker cancels the
 *                   consumer; pseudo-queue has no explicit lifecycle.
 * </pre>
 *
 * Add this jar to JMETER_HOME/lib/ext, then in JMeter:
 * <pre>
 *   Thread Group -> Sampler -> Java Request
 *   classname: com.onedroid.jmeter.amqp.AmqpDirectReplySampler
 * </pre>
 *
 * Parameters:
 * <pre>
 *   amqp_uri           AMQP connection URI
 *   username           broker username (optional if in URI)
 *   password           broker password (optional if in URI)
 *   exchange           request exchange ("" = default exchange)
 *   routing_key        request routing key (typically the server queue name
 *                      when using the default exchange)
 *   message_body       request payload (UTF-8)
 *   content_type       request MIME type
 *   timeout_ms         total budget for receiving the reply (default 30000)
 *   connect_timeout_ms socket connect timeout (default 5000)
 * </pre>
 *
 * Success: HTTP 200 with reply body as responseData and reply metadata in
 * responseHeaders.
 * Timeout: HTTP 408 with "direct-reply timeout" message.
 * Config error: HTTP 400 for missing routing_key.
 * Failure: HTTP 500 with exception details.
 */
public class AmqpDirectReplySampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_ROUTING_KEY = "routing_key";
    private static final String PARAM_MESSAGE_BODY = "message_body";
    private static final String PARAM_CONTENT_TYPE = "content_type";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_CONNECT_TIMEOUT_MS = "connect_timeout_ms";

    static final String DIRECT_REPLY_TO = "amq.rabbitmq.reply-to";

    private static final Logger log = LoggingManager.getLoggerForClass();

    private Connection connection;
    private Channel channel;
    private final BlockingQueue<ReplyMessage> inbox = new LinkedBlockingQueue<ReplyMessage>();

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_URI, "amqp://localhost:5672");
        args.addArgument(PARAM_USERNAME, "");
        args.addArgument(PARAM_PASSWORD, "");
        args.addArgument(PARAM_EXCHANGE, "");
        args.addArgument(PARAM_ROUTING_KEY, "rpc.request");
        args.addArgument(PARAM_MESSAGE_BODY, "");
        args.addArgument(PARAM_CONTENT_TYPE, "application/json");
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

            this.connection = factory.newConnection("jmeter-amqp-direct-reply-sampler");
            this.channel = connection.createChannel();

            // One persistent consumer per thread on amq.rabbitmq.reply-to.
            // autoAck=true is required by the broker for this pseudo-queue;
            // any explicit ack is rejected with a channel error.
            installConsumer(this.channel);
        } catch (Exception e) {
            log.error("AMQP direct-reply setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP Direct Reply");

        String exchange = context.getParameter(PARAM_EXCHANGE);
        String routingKey = context.getParameter(PARAM_ROUTING_KEY);
        String messageBody = context.getParameter(PARAM_MESSAGE_BODY);
        String contentType = context.getParameter(PARAM_CONTENT_TYPE);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 30000);

        // Validate routing_key before sampleStart so config errors don't
        // show up as timed samples. Matches AmqpRpcSampler pattern.
        if (routingKey == null || routingKey.trim().isEmpty()) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage("routing_key is required");
            return result;
        }

        byte[] payload = messageBody == null ? new byte[0] : messageBody.getBytes(StandardCharsets.UTF_8);
        if (contentType == null || contentType.isEmpty()) {
            contentType = "application/octet-stream";
        }

        String correlationId = UUID.randomUUID().toString();

        // sampleStart before channel-open check — if that check throws we
        // still want a well-defined (zero-duration) sample (CAT-2614 pattern).
        result.sampleStart();

        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException("AMQP channel is not open (setupTest likely failed).");
            }

            AMQP.BasicProperties props = new AMQP.BasicProperties.Builder()
                    .correlationId(correlationId)
                    .contentType(contentType)
                    .replyTo(DIRECT_REPLY_TO)
                    .build();

            channel.basicPublish(
                    exchange == null ? "" : exchange,
                    routingKey,
                    props,
                    payload);

            long deadline = System.currentTimeMillis() + Math.max(0, timeoutMs);

            while (true) {
                long remaining = deadline - System.currentTimeMillis();
                if (remaining <= 0) {
                    return directReplyTimeout(result, correlationId);
                }

                ReplyMessage msg;
                try {
                    msg = inbox.poll(remaining, TimeUnit.MILLISECONDS);
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                    result.sampleEnd();
                    result.setSuccessful(false);
                    result.setResponseCode("500");
                    result.setResponseMessage("Direct-reply poll interrupted");
                    return result;
                }

                if (msg == null) {
                    return directReplyTimeout(result, correlationId);
                }

                String replyCorrelationId = null;
                if (msg.props != null) {
                    replyCorrelationId = msg.props.getCorrelationId();
                }

                if (correlationId.equals(replyCorrelationId)) {
                    return success(result, msg, correlationId);
                }

                // Non-match = late arrival from a prior timed-out iteration
                // on this same channel. No ack needed (autoAck was true on
                // the consumer). Drop and keep polling.
                log.warn("AMQP direct-reply: discarding stale reply (correlationId="
                        + replyCorrelationId + ")");
            }
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("Direct-reply failed: " + e.getClass().getSimpleName() + ": " + e.getMessage());
            log.error("AMQP direct-reply failed: " + e.getMessage(), e);
            return result;
        }
    }

    @Override
    public void teardownTest(JavaSamplerContext context) {
        // Closing the channel cancels the consumer and releases the
        // pseudo-queue. No explicit consumer cancel or queue delete.
        try {
            if (channel != null && channel.isOpen()) channel.close();
        } catch (Exception ignored) { }
        try {
            if (connection != null && connection.isOpen()) connection.close();
        } catch (Exception ignored) { }
    }

    // --- consumer -------------------------------------------------------

    /**
     * Installs a {@link DefaultConsumer} on {@link #DIRECT_REPLY_TO} that
     * drops every delivery into {@link #inbox}. Package-private so tests
     * can call it directly without running {@link #setupTest}.
     */
    void installConsumer(Channel ch) throws IOException {
        Consumer consumer = new DefaultConsumer(ch) {
            @Override
            public void handleDelivery(String consumerTag,
                                       Envelope envelope,
                                       AMQP.BasicProperties properties,
                                       byte[] body) {
                inbox.offer(new ReplyMessage(body, properties, envelope));
            }

            @Override
            public void handleCancel(String consumerTag) {
                log.warn("AMQP direct-reply consumer was cancelled by broker (consumerTag="
                        + consumerTag + ")");
            }
        };
        // autoAck=true is mandatory for amq.rabbitmq.reply-to.
        ch.basicConsume(DIRECT_REPLY_TO, /* autoAck */ true, consumer);
    }

    // --- helpers --------------------------------------------------------

    private static SampleResult success(SampleResult result, ReplyMessage msg, String correlationId) {
        byte[] body = msg.body;

        result.sampleEnd();
        result.setSuccessful(true);
        result.setResponseCode("200");
        result.setResponseMessage("Direct-reply OK — reply "
                + (body == null ? 0 : body.length)
                + " bytes, correlationId=" + correlationId);

        if (body != null) {
            result.setResponseData(body);
        } else {
            result.setResponseData(new byte[0]);
        }

        String contentType = msg.props != null ? msg.props.getContentType() : null;
        if (contentType != null && !contentType.isEmpty()) {
            result.setContentType(contentType);
            result.setDataType(contentType.startsWith("text/") || contentType.contains("json") || contentType.contains("xml")
                    ? SampleResult.TEXT
                    : SampleResult.BINARY);
        } else {
            result.setDataType(SampleResult.BINARY);
        }

        result.setResponseHeaders(buildHeaders(msg.env, msg.props, correlationId));
        return result;
    }

    private static SampleResult directReplyTimeout(SampleResult result, String correlationId) {
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode("408");
        result.setResponseMessage("Direct-reply timeout — no matching reply for correlationId=" + correlationId);
        result.setResponseData(new byte[0]);
        result.setDataType(SampleResult.TEXT);
        return result;
    }

    private static String buildHeaders(Envelope env, AMQP.BasicProperties props, String requestCorrelationId) {
        StringBuilder sb = new StringBuilder(256);
        sb.append("requestCorrelationId=").append(requestCorrelationId).append('\n');
        if (env != null) {
            sb.append("deliveryTag=").append(env.getDeliveryTag()).append('\n');
            sb.append("exchange=").append(env.getExchange() == null ? "" : env.getExchange()).append('\n');
            sb.append("routingKey=").append(env.getRoutingKey() == null ? "" : env.getRoutingKey()).append('\n');
            sb.append("redelivered=").append(env.isRedeliver()).append('\n');
        }
        if (props != null) {
            if (props.getCorrelationId() != null) sb.append("correlationId=").append(props.getCorrelationId()).append('\n');
            if (props.getContentType() != null) sb.append("contentType=").append(props.getContentType()).append('\n');
            if (props.getMessageId() != null) sb.append("messageId=").append(props.getMessageId()).append('\n');
            if (props.getReplyTo() != null) sb.append("replyTo=").append(props.getReplyTo()).append('\n');
            Map<String, Object> headers = props.getHeaders();
            if (headers != null) {
                for (Map.Entry<String, Object> e : headers.entrySet()) {
                    sb.append("header.").append(e.getKey()).append('=')
                            .append(e.getValue() == null ? "" : e.getValue().toString()).append('\n');
                }
            }
        }
        return sb.toString();
    }

    private static int parseInt(String s, int defaultVal) {
        try { return Integer.parseInt(s); }
        catch (Exception e) { return defaultVal; }
    }

    // --- test hooks -----------------------------------------------------

    // Visible for test. Lets unit tests inject a mock channel without
    // running setupTest() (which requires a real AMQP connection).
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }

    /**
     * Test hook: synthesize an incoming reply delivery without routing
     * through a real broker. Equivalent to what the installed
     * {@link DefaultConsumer} would do on a real delivery.
     */
    void enqueueReplyForTesting(byte[] body, AMQP.BasicProperties props, Envelope env) {
        inbox.offer(new ReplyMessage(body, props, env));
    }

    // --- types ----------------------------------------------------------

    private static final class ReplyMessage {
        final byte[] body;
        final AMQP.BasicProperties props;
        final Envelope env;

        ReplyMessage(byte[] body, AMQP.BasicProperties props, Envelope env) {
            this.body = body;
            this.props = props;
            this.env = env;
        }
    }
}
