package com.onedroid.jmeter.amqp;

import org.apache.jmeter.config.Arguments;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;

import java.util.LinkedHashMap;
import java.util.Map;

/** Builds a {@link JavaSamplerContext} from the sampler's default parameters plus overrides. */
final class TestCtx {

    private final Map<String, String> overrides = new LinkedHashMap<String, String>();

    static TestCtx params() {
        return new TestCtx();
    }

    TestCtx with(String name, String value) {
        overrides.put(name, value);
        return this;
    }

    JavaSamplerContext build() {
        return buildFor(new AmqpPublishSampler().getDefaultParameters());
    }

    JavaSamplerContext buildFor(Arguments defaults) {
        Arguments merged = new Arguments();
        for (int i = 0; i < defaults.getArgumentCount(); i++) {
            String name = defaults.getArgument(i).getName();
            merged.addArgument(name, overrides.containsKey(name) ? overrides.get(name) : defaults.getArgument(i).getValue());
        }
        for (Map.Entry<String, String> e : overrides.entrySet()) {
            boolean found = false;
            for (int i = 0; i < merged.getArgumentCount(); i++) {
                if (merged.getArgument(i).getName().equals(e.getKey())) {
                    found = true;
                    break;
                }
            }
            if (!found) {
                merged.addArgument(e.getKey(), e.getValue());
            }
        }
        return new JavaSamplerContext(merged);
    }
}
