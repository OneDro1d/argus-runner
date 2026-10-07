package com.onedroid.jmeter.amqp;

import org.junit.jupiter.api.Test;

import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class BlockStateTest {

    @Test
    void tracksTheBlockedWindowAndReportsItOnce() {
        AtomicLong now = new AtomicLong(1_000);
        BlockState s = new BlockState(now::get);
        assertFalse(s.isBlocked());

        s.onBlocked("low on memory");
        now.set(1_750);
        assertTrue(s.isBlocked());
        assertEquals("blocked by broker: low on memory (since=1000)", s.blockedMessage());
        assertEquals(750, s.totalBlockedMs());

        now.set(3_000);
        s.onUnblocked();
        assertFalse(s.isBlocked());
        assertEquals(2_000, s.totalBlockedMs());
        assertEquals(1, s.periods());
        assertEquals(2_000, s.takeWasBlockedMs());
        assertEquals(-1, s.takeWasBlockedMs());
    }

    @Test
    void aSecondBlockIsASecondPeriod() {
        AtomicLong now = new AtomicLong(0);
        BlockState s = new BlockState(now::get);
        s.onBlocked("a");
        now.set(10);
        s.onUnblocked();
        now.set(20);
        s.onBlocked(null);
        assertEquals(2, s.periods());
        assertTrue(s.blockedMessage().startsWith("blocked by broker: no reason given"), s.blockedMessage());
    }
}
