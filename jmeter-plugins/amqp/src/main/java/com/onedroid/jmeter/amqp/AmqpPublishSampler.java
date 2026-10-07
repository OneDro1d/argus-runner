package com.onedroid.jmeter.amqp;

import org.apache.jorphan.logging.LoggingManager;
import org.apache.log.Logger;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;
import com.rabbitmq.client.ShutdownSignalException;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.UUID;
import java.util.concurrent.TimeoutException;

/**
 * JMeter Java Request sampler that publishes AMQP messages (AMQP 0-9-1).
 *
 * Add this jar to JMETER_HOME/lib/ext, then in JMeter:
 * Thread Group -> Sampler -> Java Request
 * and choose this class.
 *
 * AC-D16 — EXPECTED REFUSAL. When {@code expected_refusal_code} is set, the sampler enables
 * publisher confirms ({@code confirmSelect}) and calls {@code waitForConfirmsOrDie} after the
 * publish. A broker-side refusal (e.g. 406 PRECONDITION_FAILED for a {@code user_id} property
 * that does not match the connection's authenticated user, or 403 ACCESS_REFUSED) closes the
 * channel with an AMQP {@code channel.close}, which the RabbitMQ Java client surfaces as a
 * {@link ShutdownSignalException} carrying the actual reply code and text (measured against
 * amqp-client 5.22.0's {@code ChannelN.waitForConfirms}: it checks {@code getCloseReason()} and
 * throws that exception directly — no separate shutdown listener is needed). Success is then
 * "refused with EXACTLY the declared code"; an accepted publish, a timeout, or a refusal with a
 * different code are all failures, named by the ACTUAL outcome only (never the declared one —
 * holdout material stays out of the sample result).
 *
 * When {@code expected_refusal_code} is unset and {@code confirm} is {@code off} (the default),
 * behaviour is the pre-AC-D16 publish path: no confirms, no wait, same 200/500 outcomes. Two
 * things apply to it since S4: a hand-off to a connection the broker has BLOCKED is refused (503),
 * and every message passes through {@link Redact}.
 *
 * S3 — CONFIRMS FOR LOAD. {@code confirm} = {@code off} (default) | {@code each} | {@code batch:<n>}.
 * {@code each}: confirm-select once, publish, wait for the confirm; the sample time is publish->confirm
 * and the response message carries {@code us=<microseconds>}. A nack is 502, a confirm that does not
 * arrive within {@code confirm_timeout_ms} is 504, a channel closed by the broker carries the AMQP reply
 * code. {@code batch:<n>}: the n-th publish waits for every outstanding confirm. The wait is
 * {@code waitForConfirms(timeout)}, NOT {@code waitForConfirmsOrDie}: on a timeout the latter closes the
 * channel, and that close waits for a close-ok a blocked broker never sends.
 *
 * S4 — FAIL FAST WHEN BLOCKED. A {@code BlockedListener} records connection.blocked/unblocked
 * ({@link BlockState}); while blocked every sample returns 503 {@code blocked by broker: <reason>
 * (since=<epoch ms>)} at once, before anything is published; the first success after the unblock
 * appends {@code (was blocked <ms>ms)}. RabbitMQ announces a block only to a connection that has
 * published, so the FIRST publish after an alarm can be handed over unannounced: only S3 (the confirm
 * that never arrives, 504) catches that one. Automatic recovery is off (a silent reconnect would hide
 * a broker restart) and teardown is time-bounded.
 *
 * S10 — NO SECRETS IN OUTPUT. Every response message and log line passes through {@link Redact}.
 */
public class AmqpPublishSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_ROUTING_KEY = "routing_key";
    private static final String PARAM_MESSAGE_BODY = "message_body";
    private static final String PARAM_MESSAGE_SIZE_BYTES = "message_size_bytes";
    private static final String PARAM_CONTENT_TYPE = "content_type";
    private static final String PARAM_PERSISTENT = "persistent";
    private static final String PARAM_TIMEOUT_MS = "connect_timeout_ms";
    // AC-D16
    private static final String PARAM_USER_ID = "user_id";
    private static final String PARAM_EXPECTED_REFUSAL_CODE = "expected_refusal_code";
    private static final String PARAM_CONFIRM_TIMEOUT_MS = "confirm_timeout_ms";
    // S3 / S4
    private static final String PARAM_CONFIRM = "confirm";
    private static final String PARAM_LABEL = "label";
    private static final String DEFAULT_LABEL = "AMQP Publish";
    // S5 (PR-C): a session publishes on the session's channel and stamps the send time
    private static final String PARAM_SESSION = "session";
    /** {@code System.nanoTime()} at publish; the subscriber of the same JVM subtracts it from its receipt time. */
    static final String HEADER_SENT_NS = "x-argus-sent-ns";
    static final String HEADER_RUN = "x-argus-run";
    /** Bound on every connection close (guard.go closeTimeout): a blocked broker never answers close-ok. */
    static final int CLOSE_TIMEOUT_MS = 3000;
    // package-private so a test can attach a log target and prove nothing secret is logged
    static final Logger log = LoggingManager.getLoggerForClass();

    private Connection connection;
    private Channel channel;
    private BlockState blockState = new BlockState();
    private Session session;
    private boolean confirmSelected;
    private int batchCount;
    private String setupError;
    private String[] secrets = new String[0];

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_URI, "amqp://localhost:5672");
        args.addArgument(PARAM_USERNAME, "");
        args.addArgument(PARAM_PASSWORD, "");
        args.addArgument(PARAM_EXCHANGE, "");
        args.addArgument(PARAM_ROUTING_KEY, "test.key");
        args.addArgument(PARAM_MESSAGE_BODY, "");
        args.addArgument(PARAM_MESSAGE_SIZE_BYTES, "256");
        args.addArgument(PARAM_CONTENT_TYPE, "application/json");
        args.addArgument(PARAM_PERSISTENT, "false");
        args.addArgument(PARAM_TIMEOUT_MS, "5000");
        args.addArgument(PARAM_USER_ID, "");
        args.addArgument(PARAM_EXPECTED_REFUSAL_CODE, "");
        args.addArgument(PARAM_CONFIRM_TIMEOUT_MS, "5000");
        args.addArgument(PARAM_CONFIRM, "off");
        args.addArgument(PARAM_LABEL, DEFAULT_LABEL);
        return args;
    }

    @Override
    public void setupTest(JavaSamplerContext context) {
        String uri = context.getParameter(PARAM_URI);
        String username = context.getParameter(PARAM_USERNAME);
        String password = context.getParameter(PARAM_PASSWORD);
        int timeoutMs = parseInt(context.getParameter(PARAM_TIMEOUT_MS), 5000);
        this.secrets = Redact.secretsOf(uri, username, password);
        this.setupError = null;
        String sessionKey = context.getParameter(PARAM_SESSION);
        if (sessionKey != null && !sessionKey.trim().isEmpty()) {
            return; // S5: the session setup sampler owns the connection; runTest binds to it
        }

        Connection conn = null;
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
            factory.setHandshakeTimeout(timeoutMs);
            // S4: a silent reconnect would hide the broker restart the load report is meant to show.
            factory.setAutomaticRecoveryEnabled(false);
            factory.setTopologyRecoveryEnabled(false);
            factory.setShutdownTimeout(CLOSE_TIMEOUT_MS);

            conn = factory.newConnection("jmeter-amqp-sampler");
            conn.addBlockedListener(blockState.listener());
            Channel ch = conn.createChannel();
            this.connection = conn;
            this.channel = ch;
            ConfirmMode mode = ConfirmMode.parse(context.getParameter(PARAM_CONFIRM));
            if (mode != null && mode.kind != ConfirmMode.OFF) {
                ensureConfirmSelect();
            }
        } catch (Exception e) {
            this.setupError = describe(e);
            logError("AMQP setupTest failed: " + describe(e));
            abortQuietly(conn);
            this.connection = null;
            this.channel = null;
        }
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult r = doRun(context);
        String key = context.getParameter(PARAM_SESSION);
        if (key != null && !key.trim().isEmpty()) {
            publishedOk(r.isSuccessful()); // the template's IfController skips the subscribe wait on 0
        }
        return r;
    }

    /** Writes the JMeter thread variable {@code amqp_published_ok} (1/0); a no-op outside a running test. */
    private static void publishedOk(boolean ok) {
        try {
            org.apache.jmeter.threads.JMeterVariables vars =
                    org.apache.jmeter.threads.JMeterContextService.getContext().getVariables();
            if (vars != null) {
                vars.put("amqp_published_ok", ok ? "1" : "0");
            }
        } catch (RuntimeException ignored) {
            // outside JMeter (unit tests)
        }
    }

    private SampleResult doRun(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        String label = context.getParameter(PARAM_LABEL);
        result.setSampleLabel(label == null || label.trim().isEmpty() ? DEFAULT_LABEL : label.trim());
        // Secrets known from this sample's own parameters, plus those seen at setup.
        this.secrets = merge(this.secrets, Redact.secretsOf(context.getParameter(PARAM_URI),
                context.getParameter(PARAM_USERNAME), context.getParameter(PARAM_PASSWORD)));

        String exchange = context.getParameter(PARAM_EXCHANGE);
        String routingKey = context.getParameter(PARAM_ROUTING_KEY);
        String messageBody = context.getParameter(PARAM_MESSAGE_BODY);
        int messageSize = parseInt(context.getParameter(PARAM_MESSAGE_SIZE_BYTES), 256);
        String contentType = context.getParameter(PARAM_CONTENT_TYPE);
        boolean persistent = parseBoolean(context.getParameter(PARAM_PERSISTENT), false);

        String expectedRefusalRaw = context.getParameter(PARAM_EXPECTED_REFUSAL_CODE);
        boolean refusalDeclared = expectedRefusalRaw != null && !expectedRefusalRaw.trim().isEmpty();

        // S5: with a session the connection, channel and block state are the session's.
        String sessionKey = context.getParameter(PARAM_SESSION);
        boolean useSession = sessionKey != null && !sessionKey.trim().isEmpty();
        if (useSession) {
            Session sess = SessionRegistry.get(sessionKey.trim());
            if (sess == null || !sess.isOpen()) {
                return reject(result, "500", "no open session for this thread (the session setup sampler did not run or failed)");
            }
            this.session = sess;
            this.channel = sess.publisherChannel;
            this.blockState = sess.blockState;
            this.secrets = merge(this.secrets, sess.secrets);
        }

        // S3: the confirm mode is validated before anything else, so a typo is a loud 400.
        ConfirmMode mode = ConfirmMode.parse(context.getParameter(PARAM_CONFIRM));
        if (mode == null) {
            return reject(result, "400", ConfirmMode.INVALID);
        }
        if (refusalDeclared && mode.kind == ConfirmMode.BATCH) {
            return reject(result, "400", "confirm must be off or each when expected_refusal_code is set");
        }
        int confirmTimeoutMs = parseInt(context.getParameter(PARAM_CONFIRM_TIMEOUT_MS), 5000);
        if (mode.kind != ConfirmMode.OFF && !refusalDeclared && (confirmTimeoutMs < 1 || confirmTimeoutMs > 600000)) {
            return reject(result, "400", "confirm_timeout_ms must be 1..600000");
        }

        // S4: a hand-off to a connection the broker has blocked is not a valid publish. Fail at once.
        if (blockState.isBlocked()) {
            return reject(result, "503", scrub(blockState.blockedMessage()));
        }

        // Use custom message body if provided, otherwise generate random bytes
        byte[] payload;
        if (messageBody != null && !messageBody.isEmpty()) {
            payload = messageBody.getBytes(StandardCharsets.UTF_8);
        } else {
            payload = buildPayload(messageSize);
        }

        // Default content type based on payload type
        if (contentType == null || contentType.isEmpty()) {
            contentType = (messageBody != null && !messageBody.isEmpty())
                ? "application/json"
                : "application/octet-stream";
        }

        String correlationId = UUID.randomUUID().toString();
        String userId = context.getParameter(PARAM_USER_ID);

        // AC-D16: the TRIGGER can set message properties needed to provoke a refusal (at least
        // user_id) — applied whether or not a refusal is declared, since setting it alone changes
        // nothing for a scenario that declares no expected_refusal_code (an unset user_id never
        // reaches the builder, so bytes on the wire are unchanged from before this field existed).
        AMQP.BasicProperties.Builder propsBuilder = new AMQP.BasicProperties.Builder()
                .correlationId(correlationId)
                .contentType(contentType)
                .deliveryMode(persistent ? 2 : 1);
        if (userId != null && !userId.trim().isEmpty()) {
            propsBuilder.userId(userId.trim());
        }
        if (useSession) {
            // stamped as late as possible: the latency the subscriber reports starts here
            java.util.Map<String, Object> headers = new java.util.HashMap<String, Object>();
            headers.put(HEADER_SENT_NS, System.nanoTime());
            headers.put(HEADER_RUN, session.run);
            propsBuilder.headers(headers);
        }
        AMQP.BasicProperties props = propsBuilder.build();
        String ex = exchange == null ? "" : exchange;

        if (!refusalDeclared) {
            if (mode.kind == ConfirmMode.OFF) {
                return publishUnconfirmed(result, ex, routingKey, props, payload, correlationId);
            }
            return publishConfirmed(result, mode, confirmTimeoutMs, ex, routingKey, props, payload, correlationId);
        }

        // AC-D16 — EXPECTED REFUSAL. Publisher confirms are how the refusal is OBSERVED: a
        // protocol-level refusal (406 PRECONDITION_FAILED, 403 ACCESS_REFUSED, …) closes the
        // channel with a channel.close, and waitForConfirmsOrDie surfaces that as a
        // ShutdownSignalException carrying the actual reply code/text (see the class doc for the
        // amqp-client 5.22.0 source citation). Success is refusal with EXACTLY the declared code.
        int expectedCode;
        try {
            expectedCode = Integer.parseInt(expectedRefusalRaw.trim());
        } catch (NumberFormatException nfe) {
            result.sampleStart();
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("400");
            result.setResponseMessage(scrub("expected_refusal_code is not a numeric AMQP reply code: " + expectedRefusalRaw));
            return result;
        }

        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException(notOpenText());
            }

            result.sampleStart();

            channel.confirmSelect();
            channel.basicPublish(ex, routingKey, props, payload);

            try {
                channel.waitForConfirmsOrDie(confirmTimeoutMs);
                // The broker acknowledged the publish — no refusal happened. Never a secret, never
                // the declared code (VR-C8: name only what actually happened).
                result.sampleEnd();
                result.setSuccessful(false);
                result.setResponseCode("200");
                result.setResponseMessage("the broker accepted the publish — no refusal was observed");
            } catch (ShutdownSignalException sse) {
                result.sampleEnd();
                Object reason = sse.getReason();
                if (reason instanceof AMQP.Channel.Close) {
                    AMQP.Channel.Close close = (AMQP.Channel.Close) reason;
                    int actualCode = close.getReplyCode();
                    String actualText = close.getReplyText();
                    String observed = "the broker refused the publish with " + actualCode
                            + (actualText == null || actualText.isEmpty() ? "" : " " + actualText);
                    result.setResponseCode(String.valueOf(actualCode));
                    if (actualCode == expectedCode) {
                        result.setSuccessful(true);
                        result.setResponseMessage(scrub(observed));
                    } else {
                        result.setSuccessful(false);
                        result.setResponseMessage(scrub("REFUSAL-MISMATCH: " + observed + " (not the declared refusal)"));
                    }
                } else {
                    result.setSuccessful(false);
                    result.setResponseCode("500");
                    result.setResponseMessage(scrub("the channel shut down: "
                            + (sse.getMessage() == null ? sse.getClass().getSimpleName() : sse.getMessage())));
                }
            } catch (TimeoutException te) {
                result.sampleEnd();
                result.setSuccessful(false);
                result.setResponseCode("200");
                result.setResponseMessage("no refusal was observed within " + confirmTimeoutMs
                        + "ms (the broker neither confirmed nor refused)");
            }
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage(scrub("Publish failed: " + e.getClass().getSimpleName() + ": " + e.getMessage()));
            logError("AMQP publish (expected-refusal) failed: " + describe(e));
        }

        return result;
    }

    /** confirm=off: the original hand-off, no confirm, no wait. */
    private SampleResult publishUnconfirmed(SampleResult result, String exchange, String routingKey,
                                            AMQP.BasicProperties props, byte[] payload, String correlationId) {
        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException(notOpenText());
            }

            result.sampleStart();

            channel.basicPublish(exchange, routingKey, props, payload);

            result.sampleEnd();

            result.setSuccessful(true);
            result.setResponseCode("200");
            result.setResponseMessage("Published " + payload.length + " bytes, correlationId=" + correlationId
                    + wasBlockedSuffix());
            result.setResponseData(("correlationId=" + correlationId).getBytes(StandardCharsets.UTF_8));
            result.setDataType(SampleResult.TEXT);
        } catch (Exception e) {
            result.sampleEnd();
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage(scrub("Publish failed: " + e.getClass().getSimpleName() + ": " + e.getMessage()));
            logError("AMQP publish failed: " + describe(e));
        }
        return result;
    }

    /** confirm=each | batch:n (S3). */
    private SampleResult publishConfirmed(SampleResult result, ConfirmMode mode, int confirmTimeoutMs,
                                          String exchange, String routingKey, AMQP.BasicProperties props,
                                          byte[] payload, String correlationId) {
        boolean each = mode.kind == ConfirmMode.EACH;
        String modeText = each ? "each" : "batch";
        try {
            if (channel == null || !channel.isOpen()) {
                throw new IllegalStateException(notOpenText());
            }
            ensureConfirmSelect();

            result.sampleStart();
            long publishedAtNs = System.nanoTime();
            channel.basicPublish(exchange, routingKey, props, payload);

            boolean waitNow = each;
            if (!each) {
                batchCount++;
                waitNow = batchCount >= mode.batchSize;
            }
            if (!waitNow) {
                // handed to the socket; its confirm is collected by the n-th publish of the batch
                result.sampleEnd();
                succeed(result, "published " + payload.length + "B confirm=batch" + wasBlockedSuffix(), correlationId);
                return result;
            }

            long waitStartNs = each ? publishedAtNs : System.nanoTime();
            if (!channel.waitForConfirms(confirmTimeoutMs)) {
                result.sampleEnd();
                batchCount = 0;
                result.setSuccessful(false);
                result.setResponseCode("502");
                result.setResponseMessage("nacked by broker (basic.nack carries no reason; usual causes: a queue at its "
                        + "x-max-length with overflow=reject-publish, an unavailable quorum queue)");
                return result;
            }
            long us = (System.nanoTime() - waitStartNs) / 1000L;
            result.sampleEnd();
            batchCount = 0;
            String text = each
                    ? "published " + payload.length + "B confirm=each us=" + us
                    : "published " + payload.length + "B confirm=batch batch-confirm us=" + us;
            succeed(result, text + wasBlockedSuffix(), correlationId);
        } catch (TimeoutException te) {
            result.sampleEnd();
            batchCount = 0;
            result.setSuccessful(false);
            result.setResponseCode("504");
            result.setResponseMessage("not confirmed within " + confirmTimeoutMs + "ms");
        } catch (ShutdownSignalException sse) {
            result.sampleEnd();
            batchCount = 0;
            Object reason = sse.getReason();
            result.setSuccessful(false);
            if (reason instanceof AMQP.Channel.Close) {
                AMQP.Channel.Close c = (AMQP.Channel.Close) reason;
                result.setResponseCode(String.valueOf(c.getReplyCode()));
                result.setResponseMessage(scrub("channel closed by broker: " + c.getReplyCode()
                        + (c.getReplyText() == null || c.getReplyText().isEmpty() ? "" : " " + c.getReplyText())));
            } else if (reason instanceof AMQP.Connection.Close) {
                AMQP.Connection.Close c = (AMQP.Connection.Close) reason;
                result.setResponseCode(String.valueOf(c.getReplyCode()));
                result.setResponseMessage(scrub("connection closed by broker: " + c.getReplyCode()
                        + (c.getReplyText() == null || c.getReplyText().isEmpty() ? "" : " " + c.getReplyText())));
            } else {
                result.setResponseCode("500");
                result.setResponseMessage(scrub("the channel shut down: "
                        + (sse.getMessage() == null ? sse.getClass().getSimpleName() : sse.getMessage())));
            }
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
            result.sampleEnd();
            batchCount = 0;
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage("Publish failed: interrupted while waiting for the confirm");
        } catch (Exception e) {
            result.sampleEnd();
            batchCount = 0;
            result.setSuccessful(false);
            result.setResponseCode("500");
            result.setResponseMessage(scrub("Publish failed: " + e.getClass().getSimpleName() + ": " + e.getMessage()));
            logError("AMQP publish (" + modeText + ") failed: " + describe(e));
        }
        return result;
    }

    private void succeed(SampleResult result, String message, String correlationId) {
        result.setSuccessful(true);
        result.setResponseCode("200");
        result.setResponseMessage(message);
        result.setResponseData(("correlationId=" + correlationId).getBytes(StandardCharsets.UTF_8));
        result.setDataType(SampleResult.TEXT);
    }

    /** " (was blocked <ms>ms)" once, on the first success after an unblock. */
    private String wasBlockedSuffix() {
        long was = blockState.takeWasBlockedMs();
        return was < 0 ? "" : " (was blocked " + was + "ms)";
    }

    private SampleResult reject(SampleResult result, String code, String message) {
        result.sampleStart();
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode(code);
        result.setResponseMessage(message);
        return result;
    }

    private String notOpenText() {
        return "AMQP channel is not open (setupTest likely failed)"
                + (setupError == null ? "." : ": " + setupError);
    }

    /** confirm-select is sent once per channel, not once per sample. */
    private void ensureConfirmSelect() throws IOException {
        if (session != null && channel == session.publisherChannel) {
            session.ensureConfirmSelect();
            return;
        }
        if (!confirmSelected && channel != null) {
            channel.confirmSelect();
            confirmSelected = true;
        }
    }

    @Override
    public void teardownTest(JavaSamplerContext context) {
        final Connection conn = connection;
        connection = null;
        if (conn == null) {
            return;
        }
        // Bounded: a blocked broker stops reading, so it never answers the close. abort() waits at most
        // CLOSE_TIMEOUT_MS for close-ok, ignores errors, and shuts the socket either way; the thread join
        // is a second bound so a stuck write cannot hold the JMeter thread.
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                abortQuietly(conn);
            }
        }, "amqp-publish-teardown");
        t.setDaemon(true);
        t.start();
        try {
            t.join(CLOSE_TIMEOUT_MS + 1000L);
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
        }
    }

    private static void abortQuietly(Connection conn) {
        if (conn == null) {
            return;
        }
        try {
            conn.abort(CLOSE_TIMEOUT_MS);
        } catch (Exception ignored) {
            // best effort
        }
    }

    // --- S10: the only two sinks ------------------------------------------------

    private String scrub(String s) {
        return Redact.scrub(s, secrets);
    }

    /** A throwable as text with every secret removed; never its toString of a URI-bearing cause. */
    private String describe(Throwable e) {
        return scrub(e.getClass().getSimpleName() + ": " + e.getMessage());
    }

    /** The log sink: message only (the throwable's own text is already folded into it, scrubbed). */
    private void logError(String message) {
        log.error(scrub(message));
    }

    private static String[] merge(String[] a, String[] b) {
        java.util.LinkedHashSet<String> set = new java.util.LinkedHashSet<String>();
        java.util.Collections.addAll(set, a);
        java.util.Collections.addAll(set, b);
        return set.toArray(new String[0]);
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

    // --- test hooks -----------------------------------------------------

    // Visible for test. Lets unit tests inject a mock channel without running setupTest() (which
    // requires a real AMQP connection) — the same hook AmqpDirectReplySampler exposes.
    void setChannelForTesting(Channel channel) {
        this.channel = channel;
    }

    // Visible for test: the S4 state the listener feeds.
    BlockState blockStateForTesting() {
        return blockState;
    }

    private static byte[] buildPayload(int size) {
        if (size <= 0) size = 1;
        byte[] buf = new byte[size];
        for (int i = 0; i < buf.length; i++) {
            buf[i] = (byte) (i % 251);
        }
        return buf;
    }
}
