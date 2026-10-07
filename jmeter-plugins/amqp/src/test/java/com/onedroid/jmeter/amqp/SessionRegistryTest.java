package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.Channel;
import com.rabbitmq.client.Connection;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotSame;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

class SessionRegistryTest {

    @BeforeEach
    @AfterEach
    void clean() {
        SessionRegistry.closeAll();
    }

    private static Session session(String key, Connection conn, Channel consumer) {
        return new Session(key, "q-" + key, "amq.direct", "rk", "classic", 100, false, conn,
                mock(Channel.class), consumer, new BlockState(), new String[0]);
    }

    private static Connection openConn() {
        Connection c = mock(Connection.class);
        when(c.isOpen()).thenReturn(true);
        return c;
    }

    @Test
    void oneSessionPerKeyAndASecondOpenReturnsTheSame() {
        Session a = session("k", openConn(), mock(Channel.class));
        Session b = session("k", openConn(), mock(Channel.class));
        assertSame(a, SessionRegistry.putIfAbsent(a));
        assertSame(a, SessionRegistry.putIfAbsent(b));
        assertSame(a, SessionRegistry.get("k"));
        assertEquals(1, SessionRegistry.size());
    }

    @Test
    void aDeadSessionUnderTheKeyIsReplaced() {
        Connection dead = mock(Connection.class);
        when(dead.isOpen()).thenReturn(false);
        Session old = session("k", dead, mock(Channel.class));
        SessionRegistry.putIfAbsent(old);
        Session fresh = session("k", openConn(), mock(Channel.class));
        assertSame(fresh, SessionRegistry.putIfAbsent(fresh));
        assertNotSame(old, SessionRegistry.get("k"));
    }

    @Test
    void closeAllClosesEachSessionOnceDeletesItsOwnQueueAndIsIdempotent() throws Exception {
        Connection c1 = openConn();
        Connection c2 = openConn();
        Channel ch1 = mock(Channel.class);
        Channel ch2 = mock(Channel.class);
        when(ch1.isOpen()).thenReturn(true);
        when(ch2.isOpen()).thenReturn(true);
        SessionRegistry.putIfAbsent(session("a", c1, ch1));
        SessionRegistry.putIfAbsent(session("b", c2, ch2));
        assertEquals(2, SessionRegistry.closeAll());
        verify(ch1).queueDelete("q-a");
        verify(ch2).queueDelete("q-b");
        verify(c1, times(1)).abort(anyInt());
        verify(c2, times(1)).abort(anyInt());
        assertEquals(0, SessionRegistry.closeAll());
        verify(c1, times(1)).abort(anyInt());
        assertNull(SessionRegistry.get("a"));
    }

    @Test
    void aSessionClosesOnlyOnce() throws Exception {
        Connection c = openConn();
        Session s = session("k", c, mock(Channel.class));
        assertEquals(true, s.close(100));
        assertEquals(false, s.close(100));
        verify(c, times(1)).abort(anyInt());
    }
}
