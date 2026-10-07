package com.onedroid.jmeter.amqp;

import org.apache.jorphan.logging.LoggingManager;
import org.apache.log.Logger;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;
import com.rabbitmq.client.Envelope;
import com.rabbitmq.client.GetResponse;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.UUID;

/**
 * JMeter Java Request sampler that implements the AMQP RPC (request-reply)
 * pattern: publish a request to an exchange with a replyTo queue + correlationId,
 * then consume the matching reply from an exclusive temporary queue.
 *
 * Sample timing covers the full round trip: sampleStart() fires just before
 * basicPublish, sampleEnd() fires when the matching reply is received (or the
 * deadline is reached). So the reported duration IS the RPC round-trip latency.
 *
 * Lifecycle:
 *   setupTest()   — open connection + channel, declare one exclusive
 *                   auto-delete reply queue that is reused for all iterations
 *                   of this thread. Broker deletes the queue when the channel
 *                   closes in teardownTest().
 *   runTest()     — generate correlationId, publish request, poll the reply
 *                   queue until a reply with the matching correlationId
 *                   arrives (or the deadline is hit). Non-matching replies
 *                   on our own exclusive queue are ack-and-discarded (they
 *                   can only be stale replies from earlier timed-out
 *                   iterations on the same thread — nothing else can put
 *                   messages on our exclusive queue).
 *   teardownTest() — close channel + connection.
 *
 * Add this jar to JMETER_HOME/lib/ext, then in JMeter:
 *   Thread Group -> Sampler -> Java Request
 *   classname: com.onedroid.jmeter.amqp.AmqpRpcSampler
 *
 * Parameters:
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
 *
 * Success: HTTP 200 with reply body as responseData and reply metadata in
 * responseHeaders.
 * Timeout: HTTP 408 with "RPC timeout" message.
 * Config error: HTTP 400 for missing routing_key.
 * Failure: HTTP 500 with exception details.
 */
public class AmqpRpcSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_ROUTING_KEY = "routing_key";
    private static final String PARAM_MESSAGE_BODY = "message_body";
    private static final String PARAM_CONTENT_TYPE = "content_type";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_CONNECT_TIMEOUT_MS = "connect_timeout_ms";

    private static final int POLL_INTERVAL_MS = 50;

    private static final Logger log = LoggingManager.getLoggerForClass();

    private Connection connection;
    private Channel channel;
    // Server-generated exclusive reply queue name, populated in setupTest().
    private String replyQueueName;

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

            this.connection = factory.newConnection("jmeter-amqp-rpc-sampler");
            this.channel = connection.createChannel();

            // Exclusive auto-delete reply queue — broker generates a unique name,
            // no other consumer can attach, and the queue is removed when this
            // channel closes. This matches the canonical AMQP RPC pattern.
            AMQP.Queue.DeclareOk decl = this.channel.queueDeclare(
                    /* queue */ "",
                    /* durable */ false,
                    /* exclusive */ true,
                    /* autoDelete */ true,
                    /* arguments */ null);
            this.replyQueueName = decl.getQueue();
        } catch (Exception e) {
            log.error("AMQP RPC setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
            this.replyQueueName = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP RPC");

        String exchange = context.getParameter(PARAM_EXCHANGE);
        String routingKey = context.getParameter(PARAM_ROUTING_KEY);
        String messageBody = context.getParameter(PARAM_MESSAGE_BODY);
        String contentType = context.getParameter(PARAM_CONTENT_TYPE);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 30000);

        // Validate routing_key before sampleStart so config errors don't
        // show up as timed samples. Matches AmqpConsumeSampler /
        // AmqpQueueDepthSampler validation pattern.
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
        // still want a well-defined (zero-duration) sample per the CAT-2614
        // pattern. Also means the reported duration IS the RPC round-trip
        // for the normal path.
        result.sampleStart();

        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException("AMQP channel is not open (setupTest likely failed).");
            }
            if (replyQueueName == null || replyQueueName.isEmpty()) {
                throw new IllegalStateException("Reply queue was not declared (setupTest likely failed).");
            }

            AMQP.BasicProperties props = new AMQP.BasicProperties.Builder()
                    .correlationId(correlationId)
                    .contentType(contentType)
                    .replyTo(replyQueueName)
                    .build();

            channel.basicPublish(
                    exchange == null ? "" : exchange,
                    routingKey,
                    props,
                    payload);

            long deadline = System.currentTimeMillis() + Math.max(0, timeoutMs);

            while (true) {
                // Manual ack so we can discard non-matching stale replies
                // explicitly.
                GetResponse response = channel.basicGet(replyQueueName, /* autoAck */ false);

                if (response == null) {
                    if (System.currentTimeMillis() >= deadline) {
                        return rpcTimeout(result, correlationId);
                    }
                    sleepQuietly(POLL_INTERVAL_MS);
                    continue;
                }

                String replyCorrelationId = null;
                AMQP.BasicProperties replyProps = response.getProps();
                if (replyProps != null) {
                    replyCorrelationId = replyProps.getCorrelationId();
                }

                if (correlationId.equals(replyCorrelationId)) {
                    // Match — ack and return.
                    channel.basicAck(response.getEnvelope().getDeliveryTag(), false);
                    return success(result, response, correlationId);
                }

                // Non-match on our own exclusive queue = stale reply from a
                // prior timed-out iteration (nothing else can publish here).
                // Discard via ack so we don't pull it again on the next
                // poll. Requeue would just hand it back to us.
                try {
                    channel.basicAck(response.getEnvelope().getDeliveryTag(), false);
                } catch (Exception ackErr) {
                    log.warn("Failed to ack stale reply (correlationId=" + replyCorrelationId
                            + "): " + ackErr.getMessage());
                }

                if (System.currentTimeMillis() >= deadline) {
                    return rpcTimeout(result, correlationId);
                }
            }
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("RPC failed: " + e.getClass().getSimpleName() + ": " + e.getMessage());
            log.error("AMQP RPC failed: " + e.getMessage(), e);
            return result;
        }
    }

    @Override
    public void teardownTest(JavaSamplerContext context) {
        // Closing the channel triggers broker cleanup of the exclusive
        // auto-delete reply queue. No explicit queueDelete needed.
        try {
            if (channel != null && channel.isOpen()) channel.close();
        } catch (Exception ignored) { }
        try {
            if (connection != null && connection.isOpen()) connection.close();
        } catch (Exception ignored) { }
    }

    // --- helpers ----------------------------------------------------------

    private static SampleResult success(SampleResult result, GetResponse response, String correlationId) {
        Envelope env = response.getEnvelope();
        AMQP.BasicProperties props = response.getProps();
        byte[] body = response.getBody();

        result.sampleEnd();
        result.setSuccessful(true);
        result.setResponseCode("200");
        result.setResponseMessage("RPC OK — reply "
                + (body == null ? 0 : body.length)
                + " bytes, correlationId=" + correlationId);

        if (body != null) {
            result.setResponseData(body);
        } else {
            result.setResponseData(new byte[0]);
        }

        String contentType = props != null ? props.getContentType() : null;
        if (contentType != null && !contentType.isEmpty()) {
            result.setContentType(contentType);
            result.setDataType(contentType.startsWith("text/") || contentType.contains("json") || contentType.contains("xml")
                    ? SampleResult.TEXT
                    : SampleResult.BINARY);
        } else {
            result.setDataType(SampleResult.BINARY);
        }

        result.setResponseHeaders(buildHeaders(env, props, correlationId));
        return result;
    }

    private static SampleResult rpcTimeout(SampleResult result, String correlationId) {
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode("408");
        result.setResponseMessage("RPC timeout — no matching reply for correlationId=" + correlationId);
        result.setResponseData(new byte[0]);
        result.setDataType(SampleResult.TEXT);
        return result;
    }

    private static String buildHeaders(Envelope env, AMQP.BasicProperties props, String requestCorrelationId) {
        StringBuilder sb = new StringBuilder(256);
        sb.append("requestCorrelationId=").append(requestCorrelationId).append('\n');
        sb.append("deliveryTag=").append(env.getDeliveryTag()).append('\n');
        sb.append("exchange=").append(env.getExchange() == null ? "" : env.getExchange()).append('\n');
        sb.append("routingKey=").append(env.getRoutingKey() == null ? "" : env.getRoutingKey()).append('\n');
        sb.append("redelivered=").append(env.isRedeliver()).append('\n');
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

    private static void sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    // Visible for test. Lets unit tests inject a mock channel + a reply-queue
    // name without running setupTest() (which requires a real AMQP connection).
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }

    void setReplyQueueNameForTesting(String name) {
        this.replyQueueName = name;
    }
}
