package com.onedroid.jmeter.amqp;

import org.apache.logging.log4j.Level;
import org.apache.logging.log4j.LogManager;
import org.apache.logging.log4j.core.LogEvent;
import org.apache.logging.log4j.core.LoggerContext;
import org.apache.logging.log4j.core.appender.AbstractAppender;
import org.apache.logging.log4j.core.config.Configuration;
import org.apache.logging.log4j.core.config.Property;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;

/**
 * Records every log4j2 event (message AND the throwable's stack-trace text, i.e. everything an appender
 * would write). JMeter 5.6.3's {@code LoggingManager} is a shim over slf4j -> log4j2, so this is where the
 * sampler's log lines really end up.
 */
final class LogCapture implements AutoCloseable {

    private static final String NAME = "argus-capture";

    private final CopyOnWriteArrayList<String> lines = new CopyOnWriteArrayList<String>();
    private final AbstractAppender appender;

    LogCapture() {
        LoggerContext ctx = (LoggerContext) LogManager.getContext(false);
        Configuration cfg = ctx.getConfiguration();
        final List<String> sink = lines;
        appender = new AbstractAppender(NAME, null, null, true, Property.EMPTY_ARRAY) {
            @Override
            public void append(LogEvent e) {
                StringWriter sw = new StringWriter();
                sw.append(e.getMessage().getFormattedMessage());
                if (e.getThrown() != null) {
                    e.getThrown().printStackTrace(new PrintWriter(sw));
                }
                sink.add(sw.toString());
            }
        };
        appender.start();
        cfg.getRootLogger().addAppender(appender, Level.ALL, null);
        ctx.updateLoggers();
    }

    List<String> lines() {
        return lines;
    }

    String text() {
        return String.join("\n", lines);
    }

    void clear() {
        lines.clear();
    }

    @Override
    public void close() {
        LoggerContext ctx = (LoggerContext) LogManager.getContext(false);
        ctx.getConfiguration().getRootLogger().removeAppender(NAME);
        ctx.updateLoggers();
        appender.stop();
    }
}
