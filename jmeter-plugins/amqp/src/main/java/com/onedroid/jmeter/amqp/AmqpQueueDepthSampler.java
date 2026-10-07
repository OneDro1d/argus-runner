package com.onedroid.jmeter.amqp;

import org.apache.jorphan.logging.LoggingManager;
import org.apache.log.Logger;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.io.IOException;
import java.nio.charset.StandardCharsets;

/**
 * JMeter Java Request sampler that checks the depth of an AMQP (0-9-1) queue
 * without consuming any messages.
 *
 * Uses channel.queueDeclarePassive(queueName) — the queue MUST already exist;
 * this sampler will never create one. Returns messageCount as response data
 * and consumerCount as a response header so downstream JMeter assertions can
 * read either value.
 *
 * Optional expected_min_depth / expected_max_depth parameters turn the sampler
 * into an assertion too (fails the sample if the actual depth is outside the
 * configured bounds).
 *
 * Add this jar to JMETER_HOME/lib/ext, then in JMeter:
 * Thread Group -> Sampler -> Java Request
 * and choose this class.
 */
public class AmqpQueueDepthSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_QUEUE_NAME = "queue_name";
    private static final String PARAM_EXPECTED_MIN = "expected_min_depth";
    private static final String PARAM_EXPECTED_MAX = "expected_max_depth";
    private static final String PARAM_TIMEOUT_MS = "connect_timeout_ms";

    // Sentinel meaning "no threshold configured" — negative values can't be
    // valid message counts, so they make a clean default.
    private static final int UNSET_THRESHOLD = -1;

    private static final Logger log = LoggingManager.getLoggerForClass();

    private Connection connection;
    private Channel channel;

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_URI, "amqp://localhost:5672");
        args.addArgument(PARAM_USERNAME, "");
        args.addArgument(PARAM_PASSWORD, "");
        args.addArgument(PARAM_QUEUE_NAME, "");
        args.addArgument(PARAM_EXPECTED_MIN, "");
        args.addArgument(PARAM_EXPECTED_MAX, "");
        args.addArgument(PARAM_TIMEOUT_MS, "5000");
        return args;
    }

    @Override
    public void setupTest(JavaSamplerContext context) {
        String uri = context.getParameter(PARAM_URI);
        String username = context.getParameter(PARAM_USERNAME);
        String password = context.getParameter(PARAM_PASSWORD);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 5000);

        try {
            ConnectionFactory factory = new ConnectionFactory();
            factory.setUri(uri);

            if (username != null && !username.trim().isEmpty()) {
                factory.setUsername(username);
            }
            if (password != null && !password.trim().isEmpty()) {
                factory.setPassword(password);
            }

            factory.setConnectionTimeout(timeoutMs);

            this.connection = factory.newConnection("jmeter-amqp-sampler");
            this.channel = connection.createChannel();
        } catch (Exception e) {
            log.error("AMQP setupTest failed: " + e.getMessage(), e);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        result.setSampleLabel("AMQP Queue Depth");

        String queueName = context.getParameter(PARAM_QUEUE_NAME);
        int expectedMin = parseIntOrUnset(context.getParameter(PARAM_EXPECTED_MIN));
        int expectedMax = parseIntOrUnset(context.getParameter(PARAM_EXPECTED_MAX));

        if (queueName == null || queueName.trim().isEmpty()) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage("queue_name is required");
            return result;
        }

        try {
            // sampleStart before the channel-open check — if that check
            // throws IllegalStateException we want the subsequent sampleEnd
            // in the catch block to measure a meaningful zero-duration
            // sample rather than sampleEnd-without-sampleStart (undefined
            // timing per JMeter SampleResult contract).
            result.sampleStart();

            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException("AMQP channel is not open (setupTest likely failed).");
            }

            // Passive declare does not create the queue. If the queue does
            // not exist the broker closes the channel with a 404, which
            // surfaces here as IOException.
            AMQP.Queue.DeclareOk decl = channel.queueDeclarePassive(queueName);
            result.sampleEnd();

            int messageCount = decl.getMessageCount();
            int consumerCount = decl.getConsumerCount();

            result.setResponseData(Integer.toString(messageCount).getBytes(StandardCharsets.UTF_8));
            result.setDataType(SampleResult.TEXT);
            result.setResponseHeaders(
                    "queueName: " + queueName + "\n" +
                    "messageCount: " + messageCount + "\n" +
                    "consumerCount: " + consumerCount);

            String thresholdFailure = checkThresholds(messageCount, expectedMin, expectedMax);
            if (thresholdFailure != null) {
                result.setSuccessful(false);
                result.setResponseCode("412");
                result.setResponseMessage(thresholdFailure + " (actual=" + messageCount + ")");
            } else {
                result.setSuccessful(true);
                result.setResponseCode("200");
                result.setResponseMessage(
                        "queue=" + queueName +
                        " messages=" + messageCount +
                        " consumers=" + consumerCount);
            }
        } catch (IOException e) {
            // queueDeclarePassive() closes the channel on 404. If we want
            // subsequent samples on the same thread to still work, reopen.
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("404");
            result.setResponseMessage("Queue lookup failed: " + e.getMessage());
            log.warn("AMQP queue depth lookup failed for '" + queueName + "': " + e.getMessage());
            reopenChannel();
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("Queue depth check failed: " + e.getClass().getSimpleName() + ": " + e.getMessage());
            log.error("AMQP queue depth check failed: " + e.getMessage(), e);
        }

        return result;
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

    /**
     * Returns null if within bounds, otherwise a short failure message.
     */
    static String checkThresholds(int actual, int min, int max) {
        if (min != UNSET_THRESHOLD && actual < min) {
            return "queue depth below expected_min_depth=" + min;
        }
        if (max != UNSET_THRESHOLD && actual > max) {
            return "queue depth above expected_max_depth=" + max;
        }
        return null;
    }

    /**
     * After a 404 on queueDeclarePassive() the channel is closed by the
     * broker. Open a fresh one so the same sampler instance can handle
     * the next thread iteration.
     */
    private void reopenChannel() {
        try {
            if (connection != null && connection.isOpen()) {
                this.channel = connection.createChannel();
            }
        } catch (Exception e) {
            log.warn("Failed to reopen AMQP channel after 404: " + e.getMessage());
            this.channel = null;
        }
    }

    private static int parseInt(String s, int defaultVal) {
        try { return Integer.parseInt(s); }
        catch (Exception e) { return defaultVal; }
    }

    /**
     * Treats null / empty / unparseable input as UNSET_THRESHOLD. That
     * mapping is what lets users leave expected_min_depth /
     * expected_max_depth blank when they just want a depth snapshot.
     */
    static int parseIntOrUnset(String s) {
        if (s == null || s.trim().isEmpty()) return UNSET_THRESHOLD;
        try { return Integer.parseInt(s.trim()); }
        catch (NumberFormatException e) { return UNSET_THRESHOLD; }
    }

    // Visible for test. Lets unit tests inject a mock channel + connection
    // without running setupTest(). Mirrors the hook Howard added to
    // AmqpConsumeSampler. Both are needed so the 404 reopenChannel test
    // can verify a fresh channel is created from the connection.
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }

    void setConnectionForTesting(Connection connection) {
        this.connection = connection;
    }
}
