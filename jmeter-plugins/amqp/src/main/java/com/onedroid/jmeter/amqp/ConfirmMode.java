package com.onedroid.jmeter.amqp;

/** The {@code confirm} parameter of {@link AmqpPublishSampler}: {@code off | each | batch:<n>}. */
final class ConfirmMode {

    static final int OFF = 0;
    static final int EACH = 1;
    static final int BATCH = 2;

    static final String INVALID = "confirm must be off, each or batch:<n>";

    final int kind;
    final int batchSize;

    private ConfirmMode(int kind, int batchSize) {
        this.kind = kind;
        this.batchSize = batchSize;
    }

    /** Returns null when the value is not one of the three forms (n is 2..1000). */
    static ConfirmMode parse(String raw) {
        String v = raw == null ? "" : raw.trim();
        if (v.isEmpty() || v.equals("off")) {
            return new ConfirmMode(OFF, 0);
        }
        if (v.equals("each")) {
            return new ConfirmMode(EACH, 0);
        }
        if (v.startsWith("batch:")) {
            try {
                int n = Integer.parseInt(v.substring("batch:".length()).trim());
                if (n >= 2 && n <= 1000) {
                    return new ConfirmMode(BATCH, n);
                }
            } catch (NumberFormatException ignored) {
                // falls through to null
            }
        }
        return null;
    }
}
