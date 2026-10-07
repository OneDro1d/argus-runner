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
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Loopback stand-in for a RabbitMQ whose memory alarm is on, for MANY connections.
 *
 * {@link BlockedBrokerFake} keeps reading after it announces the block, so a client can always write and a
 * close frame is read (and ignored). A real blocked broker STOPS READING the socket of a publishing
 * connection: nothing the client sends is answered or even consumed, a queue.delete never gets its reply, a
 * connection.close never gets its close-ok. This fake does exactly that:
 *
 * - it accepts any number of connections and completes the handshake, channel.open, confirm.select,
 *   queue.declare / queue.bind / basic.qos / basic.consume for each (so an {@link AmqpSessionSetupSampler}
 *   session comes up against it);
 * - the first basic.publish on a connection is not confirmed; the fake sends connection.blocked for it and
 *   from then on READS NOTHING from that connection until {@link #close()} (the socket stays open);
 * - a connection that has not published is served normally, including queue.delete and close.
 *
 * It never opens a second listener and never touches anything but 127.0.0.1.
 */
final class DeafBrokerFake implements AutoCloseable {

    private final ServerSocket server;
    private final String reason;
    private final boolean announce;
    private final Thread acceptor;
    private final List<Socket> sockets = new CopyOnWriteArrayList<Socket>();
    private final AtomicInteger accepted = new AtomicInteger();
    private final AtomicInteger blocked = new AtomicInteger();
    private final AtomicInteger deleteSeen = new AtomicInteger();
    private final CountDownLatch released = new CountDownLatch(1);
    private volatile boolean closed;

    DeafBrokerFake(String reason) throws IOException {
        this(reason, true);
    }

    /** announce=false: the broker goes deaf at the first publish WITHOUT sending connection.blocked. */
    DeafBrokerFake(String reason, boolean announce) throws IOException {
        this.announce = announce;
        this.reason = reason;
        this.server = new ServerSocket(0, 1024, InetAddress.getByName("127.0.0.1"));
        this.acceptor = new Thread(new Runnable() {
            @Override
            public void run() {
                acceptLoop();
            }
        }, "deaf-broker-acceptor");
        this.acceptor.setDaemon(true);
        this.acceptor.start();
    }

    String uri() {
        return "amqp://guest:guest@127.0.0.1:" + server.getLocalPort();
    }

    int accepted() {
        return accepted.get();
    }

    /** Connections that published and were told connection.blocked (and are now deaf). */
    int blockedConnections() {
        return blocked.get();
    }

    /** queue.delete frames the fake READ (a deaf connection reads none). */
    int queueDeletesSeen() {
        return deleteSeen.get();
    }

    boolean awaitBlocked(int n, long ms) throws InterruptedException {
        long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms);
        while (blocked.get() < n && System.nanoTime() < deadline) {
            Thread.sleep(10);
        }
        return blocked.get() >= n;
    }

    private void acceptLoop() {
        while (!closed) {
            try {
                final Socket c = server.accept();
                sockets.add(c);
                accepted.incrementAndGet();
                Thread t = new Thread(new Runnable() {
                    @Override
                    public void run() {
                        serve(c);
                    }
                }, "deaf-broker-conn");
                t.setDaemon(true);
                t.start();
            } catch (IOException e) {
                return;
            }
        }
    }

    private void serve(Socket c) {
        try {
            OutputStream out = c.getOutputStream();
            DataInputStream in = new DataInputStream(c.getInputStream());
            byte[] header = new byte[8];
            in.readFully(header);
            ByteArrayOutputStream start = new ByteArrayOutputStream();
            DataOutputStream s = new DataOutputStream(start);
            s.writeByte(0);
            s.writeByte(9);
            s.writeInt(0);
            longstr(s, "PLAIN");
            longstr(s, "en_US");
            send(out, method(0, 10, 10, start.toByteArray()));
            readFrame(in); // start-ok
            ByteArrayOutputStream tune = new ByteArrayOutputStream();
            DataOutputStream t = new DataOutputStream(tune);
            t.writeShort(16);
            t.writeInt(131072);
            t.writeShort(0);
            send(out, method(0, 10, 30, tune.toByteArray()));
            readFrame(in); // tune-ok
            readFrame(in); // open
            send(out, method(0, 10, 41, new byte[] {0}));

            int consumerTags = 0;
            while (true) {
                byte[] f = readFrame(in);
                if (f[0] != 1) {
                    continue; // header, body, heartbeat
                }
                int channel = ((f[1] & 0xFF) << 8) | (f[2] & 0xFF);
                int cls = ((f[7] & 0xFF) << 8) | (f[8] & 0xFF);
                int mth = ((f[9] & 0xFF) << 8) | (f[10] & 0xFF);
                if (cls == 20 && mth == 10) {
                    send(out, method(channel, 20, 11, new byte[] {0, 0, 0, 0}));
                } else if (cls == 85 && mth == 10) {
                    if ((f[11] & 1) == 0) {
                        send(out, method(channel, 85, 11, new byte[0]));
                    }
                } else if (cls == 50 && mth == 10) { // queue.declare -> declare-ok(queue, 0, 0)
                    int n = f[13] & 0xFF;
                    byte[] ok = new byte[1 + n + 8];
                    ok[0] = (byte) n;
                    System.arraycopy(f, 14, ok, 1, n);
                    send(out, method(channel, 50, 11, ok));
                } else if (cls == 50 && mth == 20) { // queue.bind
                    send(out, method(channel, 50, 21, new byte[0]));
                } else if (cls == 60 && mth == 10) { // basic.qos
                    send(out, method(channel, 60, 11, new byte[0]));
                } else if (cls == 60 && mth == 20) { // basic.consume -> consume-ok(tag)
                    byte[] tag = ("ctag-" + (++consumerTags)).getBytes(StandardCharsets.UTF_8);
                    byte[] ok = new byte[1 + tag.length];
                    ok[0] = (byte) tag.length;
                    System.arraycopy(tag, 0, ok, 1, tag.length);
                    send(out, method(channel, 60, 21, ok));
                } else if (cls == 50 && mth == 40) { // queue.delete -> delete-ok(0)
                    deleteSeen.incrementAndGet();
                    send(out, method(channel, 50, 41, new byte[] {0, 0, 0, 0}));
                } else if (cls == 60 && mth == 40) { // basic.publish: block, then go deaf
                    byte[] r = reason.getBytes(StandardCharsets.UTF_8);
                    byte[] args = new byte[1 + r.length];
                    args[0] = (byte) r.length;
                    System.arraycopy(r, 0, args, 1, r.length);
                    if (announce) {
                        send(out, method(0, 10, 60, args));
                    }
                    blocked.incrementAndGet();
                    released.await(); // reads NOTHING more from this connection
                    return;
                } else if (cls == 10 && mth == 50) {
                    send(out, method(0, 10, 51, new byte[0]));
                    return;
                } else if (cls == 20 && mth == 40) {
                    send(out, method(channel, 20, 41, new byte[0]));
                }
            }
        } catch (IOException ignored) {
            // the client's side ended
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
        }
    }

    private static void longstr(DataOutputStream s, String v) throws IOException {
        byte[] b = v.getBytes(StandardCharsets.UTF_8);
        s.writeInt(b.length);
        s.write(b);
    }

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

    private static void send(OutputStream o, byte[] b) throws IOException {
        synchronized (o) {
            o.write(b);
            o.flush();
        }
    }

    @Override
    public void close() {
        closed = true;
        released.countDown();
        try {
            server.close();
        } catch (IOException ignored) {
            // best effort
        }
        for (Socket c : sockets) {
            try {
                c.close();
            } catch (IOException ignored) {
                // best effort
            }
        }
    }
}
