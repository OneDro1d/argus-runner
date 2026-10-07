package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.AMQP;
import com.rabbitmq.client.AuthenticationFailureException;
import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import com.rabbitmq.client.ConnectionFactory;
import com.rabbitmq.client.DefaultConsumer;
import com.rabbitmq.client.Envelope;
import com.rabbitmq.client.ShutdownSignalException;
import com.rabbitmq.client.impl.nio.NioParams;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.apache.log.Logger;
import org.apache.jorphan.logging.LoggingManager;

import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * S6 -- PER-SESSION QUEUE SETUP (design §2.C). The FIRST sampler of each JMeter thread: opens the thread's
 * {@link Session} (one connection, a publisher channel and a consumer channel), declares the per-session
 * queue, binds it, sets the prefetch and ARMS the consumer ({@code basicConsume}), so no message can be
 * published before something is listening.
 *
 * Parameters (exactly what {@code templates/amqp-load.jmx} passes): {@code amqp_uri}, {@code username},
 * {@code password}, {@code session} (the registry key), {@code exchange} (default amq.direct),
 * {@code routing_key}, {@code queue}, {@code queue_type} (classic|quorum), {@code queue_expires_ms}
 * (x-expires; the broker reaps a queue a killed run left behind), {@code prefetch} (default 100),
 * {@code ack} (manual|auto, default manual), {@code connect_timeout_ms}, {@code label}.
 * {@code durable} is optional (forced true for quorum).
 *
 * The sample is the setup: elapsed = connect + declare + bind + consume. Failure: the AMQP reply code when
 * the broker closed a channel (404/403/405/406...), 403 for a refused login, 400 for a bad parameter, 500
 * otherwise. Every message passes through {@link Redact} (S10).
 *
 * {@code teardownTest} closes EVERY registered session (JMeter calls it on a fresh instance at test end):
 * best-effort {@code queueDelete} of each session's own queue, then a bounded connection close.
 */
public class AmqpSessionSetupSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_URI = "amqp_uri";
    private static final String PARAM_USERNAME = "username";
    private static final String PARAM_PASSWORD = "password";
    private static final String PARAM_SESSION = "session";
    private static final String PARAM_EXCHANGE = "exchange";
    private static final String PARAM_ROUTING_KEY = "routing_key";
    private static final String PARAM_QUEUE = "queue";
    private static final String PARAM_QUEUE_TYPE = "queue_type";
    private static final String PARAM_EXPIRES = "queue_expires_ms";
    private static final String PARAM_DURABLE = "durable";
    private static final String PARAM_PREFETCH = "prefetch";
    private static final String PARAM_ACK = "ack";
    private static final String PARAM_TIMEOUT_MS = "connect_timeout_ms";
    private static final String PARAM_LABEL = "label";
    private static final String DEFAULT_LABEL = "AMQP Session Setup";

    static final Logger log = LoggingManager.getLoggerForClass();

    /** Opens a connection. A seam for the unit tests; the default builds a real one from a shared factory. */
    interface ConnectionOpener {
        Connection open(String uri, String username, String password, int timeoutMs) throws Exception;
    }

    static volatile ConnectionOpener opener = new ConnectionOpener() {
        @Override
        public Connection open(String uri, String username, String password, int timeoutMs) throws Exception {
            return factoryFor(uri, username, password, timeoutMs).newConnection("argus-amqp-load-session");
        }
    };

    // Generator efficiency (design R-2): ONE ConnectionFactory per broker identity, so the NIO I/O threads and
    // the consumer work pool are shared by every session instead of being created per connection.
    private static final ConcurrentHashMap<String, ConnectionFactory> FACTORIES = new ConcurrentHashMap<String, ConnectionFactory>();
    private static volatile ExecutorService consumerPool;

    private static synchronized ExecutorService consumerPool() {
        if (consumerPool == null) {
            final AtomicInteger n = new AtomicInteger();
            consumerPool = Executors.newFixedThreadPool(8, new ThreadFactory() {
                @Override
                public Thread newThread(Runnable r) {
                    Thread t = new Thread(r, "argus-amqp-consumer-" + n.incrementAndGet());
                    t.setDaemon(true);
                    return t;
                }
            });
        }
        return consumerPool;
    }

    private static ConnectionFactory factoryFor(String uri, String username, String password, int timeoutMs) throws Exception {
        // The map key holds the password; it is never printed or logged.
        String id = uri + "\u0000" + username + "\u0000" + password + "\u0000" + timeoutMs;
        ConnectionFactory f = FACTORIES.get(id);
        if (f != null) {
            return f;
        }
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
        // a silent reconnect would hide the broker restart the load report is meant to show
        factory.setAutomaticRecoveryEnabled(false);
        factory.setTopologyRecoveryEnabled(false);
        factory.setShutdownTimeout(AmqpPublishSampler.CLOSE_TIMEOUT_MS);
        factory.setRequestedHeartbeat(30);
        factory.useNio();
        factory.setNioParams(new NioParams().setNbIoThreads(4));
        factory.setSharedExecutor(consumerPool());
        ConnectionFactory prev = FACTORIES.putIfAbsent(id, factory);
        return prev == null ? factory : prev;
    }

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_URI, "amqp://localhost:5672");
        args.addArgument(PARAM_USERNAME, "");
        args.addArgument(PARAM_PASSWORD, "");
        args.addArgument(PARAM_SESSION, "");
        args.addArgument(PARAM_EXCHANGE, "amq.direct");
        args.addArgument(PARAM_ROUTING_KEY, "");
        args.addArgument(PARAM_QUEUE, "");
        args.addArgument(PARAM_QUEUE_TYPE, "classic");
        args.addArgument(PARAM_EXPIRES, "0");
        args.addArgument(PARAM_PREFETCH, "100");
        args.addArgument(PARAM_ACK, "manual");
        args.addArgument(PARAM_TIMEOUT_MS, "10000");
        args.addArgument(PARAM_LABEL, DEFAULT_LABEL);
        return args;
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        String label = context.getParameter(PARAM_LABEL);
        result.setSampleLabel(label == null || label.trim().isEmpty() ? DEFAULT_LABEL : label.trim());

        String uri = context.getParameter(PARAM_URI);
        String username = context.getParameter(PARAM_USERNAME);
        String password = context.getParameter(PARAM_PASSWORD);
        String[] secrets = Redact.secretsOf(uri, username, password);

        String key = trim(context.getParameter(PARAM_SESSION));
        String exchange = trim(context.getParameter(PARAM_EXCHANGE));
        if (exchange.isEmpty()) {
            exchange = "amq.direct";
        }
        String routingKey = trim(context.getParameter(PARAM_ROUTING_KEY));
        String queue = trim(context.getParameter(PARAM_QUEUE));
        String queueType = trim(context.getParameter(PARAM_QUEUE_TYPE)).toLowerCase();
        if (queueType.isEmpty()) {
            queueType = "classic";
        }
        String ackText = trim(context.getParameter(PARAM_ACK)).toLowerCase();
        if (ackText.isEmpty()) {
            ackText = "manual";
        }

        // ---- validate before anything touches the network: a typo is a loud 400
        if (key.isEmpty()) {
            return reject(result, "400", "session must be set (the key the publisher and subscriber find this session by)");
        }
        if (queue.isEmpty()) {
            return reject(result, "400", "queue must be set");
        }
        if (routingKey.isEmpty()) {
            return reject(result, "400", "routing_key must be set");
        }
        if (!queueType.equals("classic") && !queueType.equals("quorum")) {
            return reject(result, "400", "queue_type must be classic or quorum");
        }
        if (!ackText.equals("manual") && !ackText.equals("auto")) {
            return reject(result, "400", "ack must be manual or auto");
        }
        Long expiresMs = parseLong(context.getParameter(PARAM_EXPIRES), 0L);
        if (expiresMs == null || expiresMs < 0) {
            return reject(result, "400", "queue_expires_ms must be a non-negative integer (0 = no x-expires)");
        }
        Long prefetchL = parseLong(context.getParameter(PARAM_PREFETCH), 100L);
        if (prefetchL == null || prefetchL < 0 || prefetchL > 65535) {
            return reject(result, "400", "prefetch must be 0..65535 (0 = unlimited)");
        }
        Long timeoutL = parseLong(context.getParameter(PARAM_TIMEOUT_MS), 10000L);
        if (timeoutL == null || timeoutL < 1 || timeoutL > 600000) {
            return reject(result, "400", "connect_timeout_ms must be 1..600000");
        }
        boolean autoAck = ackText.equals("auto");
        boolean quorum = queueType.equals("quorum");
        boolean durable = quorum || parseBool(context.getParameter(PARAM_DURABLE), false);
        int prefetch = prefetchL.intValue();

        // ---- idempotent per key: a second open of a live session returns the same one
        Session existing = SessionRegistry.get(key);
        if (existing != null && existing.isOpen()) {
            result.sampleStart();
            result.sampleEnd();
            return succeed(result, existing, 0L, true);
        }

        result.sampleStart();
        long startNs = System.nanoTime();
        Connection conn = null;
        try {
            conn = opener.open(uri, username, password, timeoutL.intValue());
            BlockState block = new BlockState();
            conn.addBlockedListener(block.listener());
            Channel publisher = conn.createChannel();
            Channel consumer = conn.createChannel();

            Map<String, Object> qargs = new HashMap<String, Object>();
            if (quorum) {
                qargs.put("x-queue-type", "quorum");
            }
            if (expiresMs > 0) {
                qargs.put("x-expires", expiresMs);
            }
            consumer.queueDeclare(queue, durable, false, false, qargs.isEmpty() ? null : qargs);
            consumer.queueBind(queue, exchange, routingKey);
            consumer.basicQos(prefetch);

            Session s = new Session(key, queue, exchange, routingKey, queueType, prefetch, autoAck,
                    conn, publisher, consumer, block, secrets);
            // armed BEFORE the sample returns: the thread's first publish cannot race the consumer
            consumer.basicConsume(queue, autoAck, new InboxConsumer(consumer, s));
            Session winner = SessionRegistry.putIfAbsent(s);
            if (winner != s) {
                s.close(AmqpPublishSampler.CLOSE_TIMEOUT_MS); // lost a same-key race (never expected)
                conn = null;
            } else {
                conn = null; // ownership moved to the registry
            }
            result.sampleEnd();
            return succeed(result, winner, (System.nanoTime() - startNs) / 1000L, false);
        } catch (Exception e) {
            result.sampleEnd();
            abortQuietly(conn);
            return fail(result, e, secrets);
        }
    }

    private static SampleResult succeed(SampleResult result, Session s, long us, boolean reused) {
        result.setSuccessful(true);
        result.setResponseCode("200");
        result.setResponseMessage("session " + (reused ? "already up" : "up") + " queue=" + s.queue
                + " exchange=" + s.exchange + " routing_key=" + s.routingKey + " type=" + s.queueType
                + " prefetch=" + s.prefetch + " ack=" + (s.autoAck ? "auto" : "manual")
                + (reused ? "" : " us=" + us));
        result.setDataType(SampleResult.TEXT);
        return result;
    }

    private static SampleResult fail(SampleResult result, Exception e, String[] secrets) {
        result.setSuccessful(false);
        String code = "500";
        String text;
        ShutdownSignalException sse = shutdownOf(e);
        if (sse != null && sse.getReason() instanceof AMQP.Channel.Close) {
            AMQP.Channel.Close c = (AMQP.Channel.Close) sse.getReason();
            code = String.valueOf(c.getReplyCode());
            text = "channel closed by broker: " + c.getReplyCode() + (isEmpty(c.getReplyText()) ? "" : " " + c.getReplyText());
        } else if (sse != null && sse.getReason() instanceof AMQP.Connection.Close) {
            AMQP.Connection.Close c = (AMQP.Connection.Close) sse.getReason();
            code = String.valueOf(c.getReplyCode());
            text = "connection closed by broker: " + c.getReplyCode() + (isEmpty(c.getReplyText()) ? "" : " " + c.getReplyText());
        } else if (e instanceof AuthenticationFailureException
                || e.getClass().getSimpleName().equals("PossibleAuthenticationFailureException")) {
            code = "403";
            text = "session setup failed: " + e.getClass().getSimpleName() + ": " + e.getMessage();
        } else {
            text = "session setup failed: " + e.getClass().getSimpleName() + ": " + e.getMessage();
        }
        String scrubbed = Redact.scrub(text, secrets);
        result.setResponseCode(code);
        result.setResponseMessage(scrubbed);
        log.error(scrubbed);
        return result;
    }

    private static ShutdownSignalException shutdownOf(Throwable e) {
        for (Throwable t = e; t != null; t = t.getCause()) {
            if (t instanceof ShutdownSignalException) {
                return (ShutdownSignalException) t;
            }
            if (t.getCause() == t) {
                break;
            }
        }
        return null;
    }

    @Override
    public void teardownTest(JavaSamplerContext context) {
        SessionRegistry.closeAll();
    }

    private static void abortQuietly(Connection conn) {
        if (conn == null) {
            return;
        }
        try {
            conn.abort(AmqpPublishSampler.CLOSE_TIMEOUT_MS);
        } catch (Exception ignored) {
            // best effort
        }
    }

    private static SampleResult reject(SampleResult result, String code, String message) {
        result.sampleStart();
        result.sampleEnd();
        result.setSuccessful(false);
        result.setResponseCode(code);
        result.setResponseMessage(message);
        return result;
    }

    private static String trim(String s) {
        return s == null ? "" : s.trim();
    }

    private static boolean isEmpty(String s) {
        return s == null || s.isEmpty();
    }

    private static Long parseLong(String s, long dflt) {
        if (s == null || s.trim().isEmpty()) {
            return dflt;
        }
        try {
            return Long.valueOf(s.trim());
        } catch (NumberFormatException e) {
            return null;
        }
    }

    private static boolean parseBool(String s, boolean dflt) {
        if (s == null) {
            return dflt;
        }
        String v = s.trim().toLowerCase();
        if (v.equals("true") || v.equals("1") || v.equals("yes")) {
            return true;
        }
        if (v.equals("false") || v.equals("0") || v.equals("no")) {
            return false;
        }
        return dflt;
    }

    /**
     * The consumer callback: records the receipt time AT RECEIPT (not when the sampler takes it) and queues
     * a {@link Session.Delivery}. Holds the size, never the body.
     */
    static final class InboxConsumer extends DefaultConsumer {
        private final Session session;

        InboxConsumer(Channel channel, Session session) {
            super(channel);
            this.session = session;
        }

        @Override
        public void handleDelivery(String consumerTag, Envelope envelope, AMQP.BasicProperties props, byte[] body) {
            long ns = System.nanoTime();
            long ms = System.currentTimeMillis();
            Long sent = null;
            String run = null;
            Map<String, Object> headers = props == null ? null : props.getHeaders();
            if (headers != null) {
                Object v = headers.get(AmqpPublishSampler.HEADER_SENT_NS);
                if (v instanceof Number) {
                    sent = ((Number) v).longValue();
                }
                Object r = headers.get(AmqpPublishSampler.HEADER_RUN);
                if (r != null) {
                    run = r.toString();
                }
            }
            session.inbox.add(new Session.Delivery(envelope.getDeliveryTag(), body == null ? 0 : body.length,
                    envelope.isRedeliver(), sent, run, ns, ms));
        }
    }
}
