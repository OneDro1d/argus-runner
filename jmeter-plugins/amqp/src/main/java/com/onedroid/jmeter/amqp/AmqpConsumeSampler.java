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
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * JMeter Java Request sampler that consumes a single AMQP message (AMQP 0-9-1)
 * from a named queue using synchronous pull (basicGet), with optional filtering
 * by correlationId and/or routing key.
 *
 * Add this jar to JMETER_HOME/lib/ext, then in JMeter:
 *   Thread Group -> Sampler -> Java Request
 *   classname: com.onedroid.jmeter.amqp.AmqpConsumeSampler
 *
 * Parameters:
 *   amqp_uri                   AMQP connection URI
 *   username                   broker username (optional if in URI)
 *   password                   broker password (optional if in URI)
 *   queue_name                 queue to consume from
 *   auto_ack                   true = server auto-acks on delivery, false = manual ack
 *                              (ignored when a filter is set; poll mode always uses manual ack
 *                              so non-matching messages can be requeued on timeout)
 *   timeout_ms                 total budget for finding a matching message (poll deadline)
 *   connect_timeout_ms         socket connect timeout
 *   expected_correlation_id    optional; when set, poll until a message with this correlationId
 *                              arrives or the budget expires
 *   expected_routing_key       optional; when set, message must also have this routing key
 *
 * Success: HTTP 200 with body as responseData and message metadata in responseHeaders.
 * Empty/timeout: HTTP 408 with "no message available" / "no matching message".
 * Failure: HTTP 500 with exception details.
 */
public class AmqpConsumeSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_QUEUE_NAME = "queue_name";
    private static final String PARAM_AUTO_ACK = "auto_ack";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_CONNECT_TIMEOUT_MS = "connect_timeout_ms";
    private static final String PARAM_EXPECTED_CORRELATION_ID = "expected_correlation_id";
    private static final String PARAM_EXPECTED_ROUTING_KEY = "expected_routing_key";

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
        args.addArgument(PARAM_QUEUE_NAME, "test.queue");
        args.addArgument(PARAM_AUTO_ACK, "true");
        args.addArgument(PARAM_TIMEOUT_MS, "5000");
        args.addArgument(PARAM_CONNECT_TIMEOUT_MS, "5000");
        args.addArgument(PARAM_EXPECTED_CORRELATION_ID, "");
        args.addArgument(PARAM_EXPECTED_ROUTING_KEY, "");
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

            this.connection = factory.newConnection("jmeter-amqp-consume-sampler");
            this.channel = connection.createChannel();
        } catch (Exception e) {
            log.error("AMQP consume setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP Consume");

        String queueName = context.getParameter(PARAM_QUEUE_NAME);
        boolean autoAckParam = parseBoolean(context.getParameter(PARAM_AUTO_ACK), true);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 5000);
        String expectedCorrelationId = nullIfEmpty(context.getParameter(PARAM_EXPECTED_CORRELATION_ID));
        String expectedRoutingKey = nullIfEmpty(context.getParameter(PARAM_EXPECTED_ROUTING_KEY));

        boolean filtering = (expectedCorrelationId != null) || (expectedRoutingKey != null);

        // Validate queue_name before sampleStart so we don't record a timed
        // sample for a config error. Matches AmqpQueueDepthSampler (CAT-2614).
        if (queueName == null || queueName.trim().isEmpty()) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage("queue_name is required");
            return result;
        }

        result.sampleStart();

        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException("AMQP channel is not open (setupTest likely failed).");
            }

            long deadline = System.currentTimeMillis() + Math.max(0, timeoutMs);
            // When filtering we always use manual ack internally so non-matching
            // messages can be requeued on the way out; this preserves queue state
            // for other consumers regardless of the user's auto_ack choice.
            boolean internalAutoAck = !filtering && autoAckParam;

            List<Long> nonMatchingTags = new ArrayList<Long>();

            while (true) {
                GetResponse response = channel.basicGet(queueName, internalAutoAck);

                if (response == null) {
                    // queue empty for now
                    if (System.currentTimeMillis() >= deadline) {
                        requeueAll(channel, nonMatchingTags);
                        return emptyTimeout(result, filtering);
                    }
                    sleepQuietly(POLL_INTERVAL_MS);
                    continue;
                }

                if (!filtering) {
                    // simple single-get mode: first message wins
                    if (!internalAutoAck) {
                        channel.basicAck(response.getEnvelope().getDeliveryTag(), false);
                    }
                    return success(result, response);
                }

                // filtering mode: check match, requeue non-matches on exit
                if (matches(response, expectedCorrelationId, expectedRoutingKey)) {
                    channel.basicAck(response.getEnvelope().getDeliveryTag(), false);
                    requeueAll(channel, nonMatchingTags);
                    return success(result, response);
                }

                nonMatchingTags.add(response.getEnvelope().getDeliveryTag());

                if (System.currentTimeMillis() >= deadline) {
                    requeueAll(channel, nonMatchingTags);
                    return emptyTimeout(result, filtering);
                }
            }
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("Consume failed: " + e.getClass().getSimpleName() + ": " + e.getMessage());
            log.error("AMQP consume failed: " + e.getMessage(), e);
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

    // --- helpers -----------------------------------------------------------

    private static SampleResult success(SampleResult result, GetResponse response) {
        Envelope env = response.getEnvelope();
        AMQP.BasicProperties props = response.getProps();
        byte[] body = response.getBody();

        result.sampleEnd();
        result.setSuccessful(true);
        result.setResponseCode("200");
        result.setResponseMessage("Consumed " + (body == null ? 0 : body.length) + " bytes");

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

        result.setResponseHeaders(buildHeaders(env, props));
        return result;
    }

    private static SampleResult emptyTimeout(SampleResult result, boolean filtering) {
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode("408");
        result.setResponseMessage(filtering
                ? "no matching message within timeout"
                : "no message available within timeout");
        result.setResponseData(new byte[0]);
        result.setDataType(SampleResult.TEXT);
        return result;
    }

    private static boolean matches(GetResponse response, String expectedCorrelationId, String expectedRoutingKey) {
        if (expectedCorrelationId != null) {
            AMQP.BasicProperties props = response.getProps();
            String actual = props != null ? props.getCorrelationId() : null;
            if (!expectedCorrelationId.equals(actual)) {
                return false;
            }
        }
        if (expectedRoutingKey != null) {
            String actual = response.getEnvelope().getRoutingKey();
            if (!expectedRoutingKey.equals(actual)) {
                return false;
            }
        }
        return true;
    }

    private static void requeueAll(Channel channel, List<Long> tags) {
        if (tags == null || tags.isEmpty()) return;
        for (Long tag : tags) {
            try {
                channel.basicNack(tag, false, true);
            } catch (Exception e) {
                log.warn("basicNack failed for deliveryTag=" + tag + ": " + e.getMessage());
            }
        }
    }

    private static String buildHeaders(Envelope env, AMQP.BasicProperties props) {
        StringBuilder sb = new StringBuilder(256);
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

    private static boolean parseBoolean(String s, boolean defaultVal) {
        if (s == null) return defaultVal;
        String v = s.trim().toLowerCase();
        if (v.equals("true") || v.equals("1") || v.equals("yes") || v.equals("y")) return true;
        if (v.equals("false") || v.equals("0") || v.equals("no") || v.equals("n")) return false;
        return defaultVal;
    }

    private static String nullIfEmpty(String s) {
        if (s == null) return null;
        String t = s.trim();
        return t.isEmpty() ? null : t;
    }

    private static void sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    // Visible for test. Lets unit tests inject a mock channel without running setupTest().
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }
}
