package com.onedroid.jmeter.amqp;

import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Loopback stand-in for a RabbitMQ with a memory alarm: the Java port of {@code blockedBroker} in
 * internal/amqpengine/blocked_socket_test.go. It completes the AMQP handshake, answers channel.open and
 * confirm.select, and then behaves like a broker whose alarm is on:
 *
 * - the FIRST basic.publish it receives is not confirmed, and only then does it send connection.blocked
 *   (RabbitMQ announces a block to a connection that publishes: the first blocked publish is unannounced);
 * - while blocked it answers nothing else, including connection.close (a blocked broker has stopped reading);
 * - {@link #unblock()} sends connection.unblocked and confirms every publish so far, and from then on it
 *   confirms each publish at once.
 *
 * No broker, no credentials: it always runs.
 */
final class BlockedBrokerFake implements AutoCloseable {

    private final ServerSocket server;
    private final String reason;
    private final Thread thread;
    private volatile Socket client;
    private volatile OutputStream out;
    private volatile boolean blocked;
    private volatile boolean alarmArmed = true;
    private volatile boolean closeSeen;
    private volatile long publishes;
    private final CountDownLatch blockedSent = new CountDownLatch(1);
    private final CountDownLatch clientGone = new CountDownLatch(1);

    BlockedBrokerFake(String reason) throws IOException {
        this(reason, true);
    }

    /** alarm=false: a healthy broker that confirms every publish (for the success-path sample). */
    BlockedBrokerFake(String reason, boolean alarm) throws IOException {
        this.reason = reason;
        this.alarmArmed = alarm;
        this.server = new ServerSocket(0, 1, InetAddress.getByName("127.0.0.1"));
        this.thread = new Thread(new Runnable() {
            @Override
            public void run() {
                serve();
            }
        }, "blocked-broker-fake");
        this.thread.setDaemon(true);
        this.thread.start();
    }

    String uri() {
        return "amqp://guest:guest@127.0.0.1:" + server.getLocalPort();
    }

    long publishes() {
        return publishes;
    }

    boolean awaitBlockedSent(long ms) throws InterruptedException {
        return blockedSent.await(ms, TimeUnit.MILLISECONDS);
    }

    boolean awaitClientGone(long ms) throws InterruptedException {
        return clientGone.await(ms, TimeUnit.MILLISECONDS);
    }

    boolean closeSeen() {
        return closeSeen;
    }

    /** The alarm clears: connection.unblocked, then a multiple-ack for everything published so far. */
    void unblock() throws IOException {
        alarmArmed = false;
        blocked = false;
        send(method(0, 10, 61, new byte[0]));
        if (publishes > 0) {
            send(ack(publishes, true));
        }
    }

    private void serve() {
        try (Socket c = server.accept()) {
            client = c;
            out = c.getOutputStream();
            DataInputStream in = new DataInputStream(c.getInputStream());
            byte[] header = new byte[8];
            in.readFully(header); // protocol header
            ByteArrayOutputStream start = new ByteArrayOutputStream();
            DataOutputStream s = new DataOutputStream(start);
            s.writeByte(0);
            s.writeByte(9);
            s.writeInt(0); // empty server-properties table
            longstr(s, "PLAIN");
            longstr(s, "en_US");
            send(method(0, 10, 10, start.toByteArray()));
            readFrame(in); // start-ok
            ByteArrayOutputStream tune = new ByteArrayOutputStream();
            DataOutputStream t = new DataOutputStream(tune);
            t.writeShort(16);   // channel-max
            t.writeInt(131072); // frame-max
            t.writeShort(0);    // heartbeat
            send(method(0, 10, 30, tune.toByteArray()));
            readFrame(in); // tune-ok
            readFrame(in); // open
            send(method(0, 10, 41, new byte[] {0}));

            while (true) {
                byte[] f = readFrame(in);
                int type = f[0];
                if (type != 1) {
                    continue; // header, body, heartbeat
                }
                int channel = ((f[1] & 0xFF) << 8) | (f[2] & 0xFF);
                int cls = ((f[7] & 0xFF) << 8) | (f[8] & 0xFF);
                int mth = ((f[9] & 0xFF) << 8) | (f[10] & 0xFF);
                if (cls == 20 && mth == 10) { // channel.open -> open-ok
                    send(method(channel, 20, 11, new byte[] {0, 0, 0, 0}));
                } else if (cls == 85 && mth == 10) { // confirm.select -> select-ok (unless no-wait)
                    boolean noWait = (f[11] & 1) != 0;
                    if (!noWait) {
                        send(method(channel, 85, 11, new byte[0]));
                    }
                } else if (cls == 60 && mth == 40) { // basic.publish
                    long n = ++publishes;
                    if (alarmArmed && n == 1) {
                        blocked = true;
                        byte[] r = reason.getBytes(StandardCharsets.UTF_8);
                        byte[] args = new byte[1 + r.length];
                        args[0] = (byte) r.length;
                        System.arraycopy(r, 0, args, 1, r.length);
                        send(method(0, 10, 60, args)); // connection.blocked, never a confirm
                        blockedSent.countDown();
                    } else if (!blocked) {
                        send(ack(n, false));
                    }
                } else if (cls == 10 && mth == 50) { // connection.close
                    closeSeen = true;
                    if (!blocked) {
                        send(method(0, 10, 51, new byte[0]));
                        return;
                    }
                } else if (cls == 20 && mth == 40 && !blocked) { // channel.close
                    send(method(channel, 20, 41, new byte[0]));
                }
            }
        } catch (IOException ignored) {
            // the client's side ended
        } finally {
            clientGone.countDown();
        }
    }

    private static byte[] ack(long tag, boolean multiple) {
        byte[] a = new byte[9];
        for (int i = 0; i < 8; i++) {
            a[i] = (byte) (tag >>> (56 - 8 * i));
        }
        a[8] = (byte) (multiple ? 1 : 0);
        return method(1, 60, 80, a);
    }

    private static void longstr(DataOutputStream s, String v) throws IOException {
        byte[] b = v.getBytes(StandardCharsets.UTF_8);
        s.writeInt(b.length);
        s.write(b);
    }

    /** One method frame: type 1, channel, size, class, method, args, 0xCE. */
    private static byte[] method(int channel, int cls, int mth, byte[] args) {
        int size = 4 + args.length;
        byte[] f = new byte[7 + size + 1];
        f[0] = 1;
        f[1] = (byte) (channel >> 8);
        f[2] = (byte) channel;
        f[3] = (byte) (size >>> 24);
        f[4] = (byte) (size >>> 16);
        f[5] = (byte) (size >>> 8);
        f[6] = (byte) size;
        f[7] = (byte) (cls >> 8);
        f[8] = (byte) cls;
        f[9] = (byte) (mth >> 8);
        f[10] = (byte) mth;
        System.arraycopy(args, 0, f, 11, args.length);
        f[f.length - 1] = (byte) 0xCE;
        return f;
    }

    /** Returns the whole frame: [type][channel 2][size 4][payload...][0xCE]. */
    private static byte[] readFrame(DataInputStream in) throws IOException {
        byte[] hdr = new byte[7];
        in.readFully(hdr);
        int size = ((hdr[3] & 0xFF) << 24) | ((hdr[4] & 0xFF) << 16) | ((hdr[5] & 0xFF) << 8) | (hdr[6] & 0xFF);
        byte[] rest = new byte[size + 1];
        in.readFully(rest);
        byte[] f = new byte[7 + rest.length];
        System.arraycopy(hdr, 0, f, 0, 7);
        System.arraycopy(rest, 0, f, 7, rest.length);
        return f;
    }

    private synchronized void send(byte[] b) throws IOException {
        OutputStream o = out;
        if (o != null) {
            o.write(b);
            o.flush();
        }
    }

    @Override
    public void close() {
        try {
            server.close();
        } catch (IOException ignored) {
            // best effort
        }
        Socket c = client;
        if (c != null) {
            try {
                c.close();
            } catch (IOException ignored) {
                // best effort
            }
        }
    }
}
