package com.onedroid.jmeter.amqp;

import org.junit.jupiter.api.Assertions;
import org.junit.jupiter.api.Assumptions;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URI;
import java.net.URL;
import java.nio.channels.FileChannel;
import java.nio.channels.FileLock;
import java.nio.charset.StandardCharsets;
import java.nio.file.Paths;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.List;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * The rules every broker-backed ("live") test runs under (HARD-STOPS of M-20261002-ARGUS-AMQP-LOAD):
 *
 *  - it refuses unless ARGUS_TEST_AMQP_URL points at a LOOPBACK host (the port-forward to the mission's own
 *    broker) and both alarm commands name {@code -n argus-amqp-test}; they are argv strings, split on
 *    whitespace and executed WITHOUT a shell;
 *  - one live run at a time on the shared broker: an exclusive file lock (ARGUS_TEST_AMQP_LOCK);
 *  - a test that raises the memory alarm ALWAYS lowers it again (finally) and this class verifies through
 *    the management API that the alarm is gone, failing loudly, with the manual restore command, if not;
 *  - ARGUS_REQUIRE_AMQP=1 (set by {@code mvn -Plive}) turns a missing variable into a FAILURE, never a
 *    skip: "ok" is the same word whether tests ran or skipped.
 *
 * No value of any ARGUS_TEST_AMQP_* variable is ever printed or put in an assertion message.
 */
final class AmqpLiveHarness implements AutoCloseable {

    static final String NAMESPACE = "argus-amqp-test";

    private static final Pattern MEM_ALARM = Pattern.compile("\"mem_alarm\"\\s*:\\s*(true|false)");

    private final URI amqpUri;
    private final String mgmtBase;
    private final String mgmtAuth;
    private final String[] alarmOn;
    private final String[] alarmOff;
    private final String alarmOffText;
    private final FileChannel lockChannel;
    private final FileLock lock;
    private boolean alarmRaised;

    private AmqpLiveHarness(URI amqpUri, String mgmtBase, String mgmtAuth, String on, String off,
                            FileChannel lockChannel, FileLock lock) {
        this.amqpUri = amqpUri;
        this.mgmtBase = mgmtBase;
        this.mgmtAuth = mgmtAuth;
        this.alarmOn = on.trim().split("\\s+");
        this.alarmOff = off.trim().split("\\s+");
        this.alarmOffText = off.trim();
        this.lockChannel = lockChannel;
        this.lock = lock;
    }

    static boolean required() {
        return "1".equals(System.getenv("ARGUS_REQUIRE_AMQP"));
    }

    /** Validates the environment and takes the lock. Never returns a harness that could touch another broker. */
    static AmqpLiveHarness acquire() throws Exception {
        String url = env("ARGUS_TEST_AMQP_URL");
        String mgmt = env("ARGUS_TEST_AMQP_MGMT_URL");
        String on = env("ARGUS_TEST_AMQP_ALARM_ON");
        String off = env("ARGUS_TEST_AMQP_ALARM_OFF");
        String lockPath = env("ARGUS_TEST_AMQP_LOCK");
        if (url == null || mgmt == null || on == null || off == null || lockPath == null) {
            String msg = "live AMQP tests need ARGUS_TEST_AMQP_URL, _MGMT_URL, _ALARM_ON, _ALARM_OFF and _LOCK in the environment";
            if (required()) {
                Assertions.fail(msg + " (ARGUS_REQUIRE_AMQP=1: a missing broker is a failure, not a skip)");
            }
            Assumptions.assumeTrue(false, msg);
        }

        URI amqp = URI.create(url);
        String host = amqp.getHost();
        Assertions.assertTrue("localhost".equals(host) || "127.0.0.1".equals(host),
                "ARGUS_TEST_AMQP_URL must point at a loopback host (the port-forward to the mission broker); refusing to run");
        URI mg = URI.create(mgmt);
        Assertions.assertTrue("localhost".equals(mg.getHost()) || "127.0.0.1".equals(mg.getHost()),
                "ARGUS_TEST_AMQP_MGMT_URL must point at a loopback host; refusing to run");
        requireNamespace("ARGUS_TEST_AMQP_ALARM_ON", on);
        requireNamespace("ARGUS_TEST_AMQP_ALARM_OFF", off);

        String userinfo = amqp.getRawUserInfo();
        Assertions.assertNotNull(userinfo, "ARGUS_TEST_AMQP_URL must carry the broker user:password");
        String mgmtAuth = "Basic " + Base64.getEncoder().encodeToString(decode(userinfo).getBytes(StandardCharsets.UTF_8));

        FileChannel ch = FileChannel.open(Paths.get(lockPath), StandardOpenOption.CREATE, StandardOpenOption.WRITE);
        FileLock lk = null;
        long deadline = System.nanoTime() + TimeUnit.MINUTES.toNanos(20);
        while (lk == null) {
            lk = ch.tryLock();
            if (lk == null) {
                Assertions.assertTrue(System.nanoTime() < deadline, "gave up waiting 20 minutes for the shared-broker lock");
                Thread.sleep(2000);
            }
        }
        return new AmqpLiveHarness(amqp, trimSlash(mgmt), mgmtAuth, on, off, ch, lk);
    }

    /** The URI to hand to the sampler: the broker URL with no path (default vhost "/"). Holds the password. */
    String samplerUri() {
        String raw = amqpUri.toString();
        int at = raw.indexOf("://");
        int slash = raw.indexOf('/', at + 3);
        return slash < 0 ? raw : raw.substring(0, slash);
    }

    String user() {
        String ui = decode(amqpUri.getRawUserInfo());
        int c = ui.indexOf(':');
        return c < 0 ? ui : ui.substring(0, c);
    }

    String hostPort() {
        return amqpUri.getHost() + ":" + amqpUri.getPort();
    }

    // --- the memory alarm ---------------------------------------------------------------------------

    /** Raises the alarm and waits until the management API reports it. Pair EVERY call with alarmOff in a finally. */
    void alarmOn() throws Exception {
        alarmRaised = true; // from here on, restoring is owed even if the command below fails half way
        run(alarmOn);
        Assertions.assertTrue(awaitAlarm(true, 30), "the memory alarm did not come on within 30s");
    }

    /**
     * Lowers the alarm and VERIFIES it cleared. If it did not, fails loudly and prints the manual restore
     * command. Idempotent and safe to call when nothing was raised.
     */
    void alarmOff() throws Exception {
        boolean ok = false;
        try {
            run(alarmOff);
            ok = awaitAlarm(false, 30);
        } catch (Exception e) {
            System.err.println("ARGUS LIVE TEST: restoring the memory alarm failed: " + e.getMessage());
        }
        if (!ok) {
            String msg = "THE BROKER'S MEMORY ALARM IS STILL ON. Restore it by hand NOW (it blocks every publisher on the mission broker): "
                    + alarmOffText;
            System.err.println(msg);
            Assertions.fail(msg);
        }
        alarmRaised = false;
    }

    boolean alarmOwed() {
        return alarmRaised;
    }

    boolean memAlarm() throws Exception {
        String body = get("/api/nodes");
        Matcher m = MEM_ALARM.matcher(body);
        boolean any = false;
        boolean found = false;
        while (m.find()) {
            found = true;
            any |= "true".equals(m.group(1));
        }
        Assertions.assertTrue(found, "the management API /api/nodes did not report mem_alarm");
        return any;
    }

    private boolean awaitAlarm(boolean want, int seconds) throws Exception {
        long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(seconds);
        while (System.nanoTime() < end) {
            try {
                if (memAlarm() == want) {
                    return true;
                }
            } catch (java.io.IOException transientError) {
                // the port-forward can blip; keep polling until the deadline
            }
            Thread.sleep(500);
        }
        return false;
    }

    // --- the management API (queue facts for PR-C) -----------------------------------------------------

    /** GET on the management API; null when the broker answers 404. */
    String mgmtGetOrNull(String path) throws Exception {
        try {
            return get(path);
        } catch (java.io.FileNotFoundException notFound) {
            return null;
        }
    }

    /** The queue's management JSON on the default vhost, or null if it does not exist. */
    String queueJson(String queue) throws Exception {
        return mgmtGetOrNull("/api/queues/%2F/" + java.net.URLEncoder.encode(queue, "UTF-8"));
    }

    /** DELETE a queue through the management API (test cleanup; 404 is fine). */
    void deleteQueue(String queue) {
        try {
            HttpURLConnection c = (HttpURLConnection) new URL(mgmtBase + "/api/queues/%2F/"
                    + java.net.URLEncoder.encode(queue, "UTF-8")).openConnection();
            c.setRequestMethod("DELETE");
            c.setConnectTimeout(5000);
            c.setReadTimeout(10000);
            c.setRequestProperty("Authorization", mgmtAuth);
            c.getResponseCode();
            c.disconnect();
        } catch (Exception ignored) {
            // cleanup only; x-expires is the backstop
        }
    }

    // --- plumbing -----------------------------------------------------------------------------------

    private String get(String path) throws Exception {
        HttpURLConnection c = (HttpURLConnection) new URL(mgmtBase + path).openConnection();
        c.setConnectTimeout(5000);
        c.setReadTimeout(10000);
        c.setRequestProperty("Authorization", mgmtAuth);
        try (InputStream in = c.getInputStream()) {
            ByteArrayOutputStream out = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) {
                out.write(buf, 0, n);
            }
            return new String(out.toByteArray(), StandardCharsets.UTF_8);
        }
    }

    /** argv, no shell. Output (kubectl/rabbitmqctl) never carries the broker password; it is only shown on failure. */
    private static void run(String[] argv) throws Exception {
        Process p = new ProcessBuilder(Arrays.asList(argv)).redirectErrorStream(true).start();
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        byte[] buf = new byte[4096];
        InputStream in = p.getInputStream();
        long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(90);
        while (System.nanoTime() < end && (p.isAlive() || in.available() > 0)) {
            int n = in.available() > 0 ? in.read(buf) : -1;
            if (n > 0) {
                out.write(buf, 0, n);
            } else {
                Thread.sleep(50);
            }
        }
        if (p.isAlive()) {
            p.destroyForcibly();
            Assertions.fail("command did not finish within 90s: " + argv[0]);
        }
        if (p.exitValue() != 0) {
            Assertions.fail("command failed (exit " + p.exitValue() + "): " + new String(out.toByteArray(), StandardCharsets.UTF_8));
        }
    }

    private static void requireNamespace(String name, String cmd) {
        List<String> t = new ArrayList<String>(Arrays.asList(cmd.trim().split("\\s+")));
        int i = t.indexOf("-n");
        Assertions.assertTrue(i >= 0 && i + 1 < t.size() && NAMESPACE.equals(t.get(i + 1)),
                name + " must contain '-n " + NAMESPACE + "'; refusing to run");
        Assertions.assertEquals(i, t.lastIndexOf("-n"), name + " must name exactly one namespace");
        Assertions.assertFalse(t.contains("--namespace") || t.contains("--context") || t.contains("--kubeconfig"),
                name + " must not select another namespace, context or kubeconfig");
        Assertions.assertTrue(cmd.contains("rabbitmqctl"), name + " must be a rabbitmqctl command");
    }

    private static String env(String name) {
        String v = System.getenv(name);
        return v == null || v.trim().isEmpty() ? null : v;
    }

    private static String trimSlash(String s) {
        return s.endsWith("/") ? s.substring(0, s.length() - 1) : s;
    }

    private static String decode(String s) {
        try {
            return java.net.URLDecoder.decode(s, "UTF-8");
        } catch (Exception e) {
            return s;
        }
    }

    @Override
    public void close() throws Exception {
        try {
            if (alarmRaised) {
                alarmOff(); // the @AfterAll safety: never hand the broker back with the alarm on
            }
        } finally {
            try {
                lock.release();
            } finally {
                lockChannel.close();
            }
        }
    }
}
