package com.onedroid.jmeter.amqp;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.AbstractJavaSamplerClient;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;

import java.util.concurrent.TimeUnit;

/**
 * S5 -- A SUBSCRIBING CONSUMER (design §2.C). Takes the next delivery from the session's inbox (filled by the
 * consumer {@link AmqpSessionSetupSampler} armed with {@code basicConsume}) and waits at most
 * {@code timeout_ms} (default 5000).
 *
 * LATENCY is publish->deliver: the consumer callback recorded {@code System.nanoTime()} AT RECEIPT (not at
 * take), the publisher stamped {@code x-argus-sent-ns} with {@code System.nanoTime()} of the same JVM, so the
 * two are comparable. A delivery whose {@code x-argus-run} differs from the session's (or that carries no
 * stamp) is FOREIGN: counted, acked, skipped, never a latency.
 *
 * Row: {@code setStampAndTime(sentMs, deliverMs - sentMs)}, so the JTL {@code timeStamp} is the SEND time (the
 * step window uses it); 200 {@code delivered <n>B us=<micros> redelivered=<bool>}; 408
 * {@code no delivery within <ms>ms}; 503 {@code blocked by broker: ...} when the connection is blocked and
 * nothing is waiting; 500 when there is no such session; 400 on a bad parameter or when {@code ack} disagrees
 * with how the session consumes.
 *
 * Parameters (what {@code templates/amqp-load.jmx} passes): {@code session}, {@code timeout_ms}, {@code ack},
 * {@code label}.
 */
public class AmqpSubscribeSampler extends AbstractJavaSamplerClient {

    private static final String PARAM_SESSION = "session";
    private static final String PARAM_TIMEOUT_MS = "timeout_ms";
    private static final String PARAM_ACK = "ack";
    private static final String PARAM_LABEL = "label";
    private static final String DEFAULT_LABEL = "AMQP Subscribe";
    /** How often a waiting subscriber looks at the block state. */
    private static final long SLICE_MS = 50;

    @Override
    public Arguments getDefaultParameters() {
        Arguments args = new Arguments();
        args.addArgument(PARAM_SESSION, "");
        args.addArgument(PARAM_TIMEOUT_MS, "5000");
        args.addArgument(PARAM_ACK, "manual");
        args.addArgument(PARAM_LABEL, DEFAULT_LABEL);
        return args;
    }

    @Override
    public SampleResult runTest(JavaSamplerContext context) {
        SampleResult result = new SampleResult();
        String label = context.getParameter(PARAM_LABEL);
        result.setSampleLabel(label == null || label.trim().isEmpty() ? DEFAULT_LABEL : label.trim());

        String key = context.getParameter(PARAM_SESSION) == null ? "" : context.getParameter(PARAM_SESSION).trim();
        if (key.isEmpty()) {
            return reject(result, "400", "session must be set");
        }
        long timeoutMs;
        try {
            String t = context.getParameter(PARAM_TIMEOUT_MS);
            timeoutMs = t == null || t.trim().isEmpty() ? 5000L : Long.parseLong(t.trim());
        } catch (NumberFormatException e) {
            return reject(result, "400", "timeout_ms must be an integer 1..600000");
        }
        if (timeoutMs < 1 || timeoutMs > 600000) {
            return reject(result, "400", "timeout_ms must be an integer 1..600000");
        }
        String ack = context.getParameter(PARAM_ACK) == null ? "manual" : context.getParameter(PARAM_ACK).trim().toLowerCase();
        if (ack.isEmpty()) {
            ack = "manual";
        }
        if (!ack.equals("manual") && !ack.equals("auto")) {
            return reject(result, "400", "ack must be manual or auto");
        }

        Session s = SessionRegistry.get(key);
        if (s == null || !s.isOpen()) {
            return reject(result, "500", "no open session for this thread (the session setup sampler did not run or failed)");
        }
        if (ack.equals("auto") != s.autoAck) {
            return reject(result, "400", "ack=" + ack + " but the session consumes with ack=" + (s.autoAck ? "auto" : "manual"));
        }

        long startMs = System.currentTimeMillis();
        long startNs = System.nanoTime();
        long deadlineNs = startNs + TimeUnit.MILLISECONDS.toNanos(timeoutMs);
        try {
            while (true) {
                Session.Delivery d = s.inbox.poll();
                if (d == null) {
                    if (s.blockState.isBlocked()) {
                        result.setStampAndTime(startMs, System.currentTimeMillis() - startMs);
                        return fail(result, "503", Redact.scrub(s.blockState.blockedMessage(), s.secrets));
                    }
                    long leftNs = deadlineNs - System.nanoTime();
                    if (leftNs <= 0) {
                        result.setStampAndTime(startMs, System.currentTimeMillis() - startMs);
                        return fail(result, "408", "no delivery within " + timeoutMs + "ms");
                    }
                    d = s.inbox.poll(Math.min(TimeUnit.NANOSECONDS.toMillis(leftNs) + 1, SLICE_MS), TimeUnit.MILLISECONDS);
                    if (d == null) {
                        continue;
                    }
                }
                if (d.sentNs == null || d.run == null || !d.run.equals(s.run)) {
                    s.foreign.incrementAndGet();
                    ackIfNeeded(s, d); // never leave a foreign message unacked: it would eat the prefetch window
                    continue;
                }
                long latencyNs = Math.max(0L, d.receiptNs - d.sentNs);
                long sentMs = d.receiptMs - latencyNs / 1_000_000L;
                long micros = latencyNs / 1000L;
                result.setStampAndTime(sentMs, latencyNs / 1_000_000L);
                boolean acked = ackIfNeeded(s, d);
                if (!acked) {
                    return fail(result, "500", "could not ack the delivery (the consumer channel is closed)");
                }
                result.setSuccessful(true);
                result.setResponseCode("200");
                result.setResponseMessage("delivered " + d.sizeBytes + "B us=" + micros + " redelivered=" + d.redelivered);
                result.setDataType(SampleResult.TEXT);
                return result;
            }
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
            result.setStampAndTime(startMs, System.currentTimeMillis() - startMs);
            return fail(result, "500", "interrupted while waiting for a delivery");
        }
    }

    /** Manual ack on the consumer channel; true when nothing was owed or the ack was sent. */
    private static boolean ackIfNeeded(Session s, Session.Delivery d) {
        if (s.autoAck) {
            return true;
        }
        try {
            s.consumerChannel.basicAck(d.deliveryTag, false);
            return true;
        } catch (Exception e) {
            return false;
        }
    }

    private static SampleResult fail(SampleResult result, String code, String message) {
        result.setSuccessful(false);
        result.setResponseCode(code);
        result.setResponseMessage(message);
        return result;
    }

    private static SampleResult reject(SampleResult result, String code, String message) {
        result.sampleStart();
        result.sampleEnd();
        return fail(result, code, message);
    }
}
