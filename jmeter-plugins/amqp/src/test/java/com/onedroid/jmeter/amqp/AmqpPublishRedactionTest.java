package com.onedroid.jmeter.amqp;

import com.rabbitmq.client.ConnectionFactory;
import org.apache.jmeter.protocol.java.sampler.JavaSamplerContext;
import org.apache.jmeter.samplers.SampleResult;
import org.junit.jupiter.api.Test;

import java.net.URISyntaxException;
import java.nio.charset.StandardCharsets;
import java.util.Base64;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * S10 on the sampler: the broker password reaches neither the sample result nor the log.
 *
 * The failure used is a URI the client cannot parse; its exception message quotes the whole URI, password
 * included. The test first PROVES that (the positive control: the secret is in the raw input), so a green
 * result cannot mean "the secret never got near the output".
 */
class AmqpPublishRedactionTest {

    @Test
    void aBrokenUriNeverEchoesThePasswordIntoTheResultOrTheLog() throws Exception {
        for (String pw : new String[] {"Sup3r-S3cret!pw", "p%40ss%20w0rd"}) {
            String uri = "amqp://svc:" + pw + "@bad host:notaport/";

            // --- positive control: the secret IS in the raw failure text -----------------------------
            String raw = null;
            try {
                new ConnectionFactory().setUri(uri);
            } catch (URISyntaxException e) {
                raw = e.getMessage();
            } catch (Exception e) {
                raw = String.valueOf(e.getMessage());
            }
            assertNotNull(raw, "the control URI must fail to parse");
            assertTrue(raw.contains(pw), "CONTROL: the raw exception text must contain the password, got: " + raw);

            // --- the sampler, same input -------------------------------------------------------------
            try (LogCapture log = new LogCapture()) {
                AmqpPublishSampler sampler = new AmqpPublishSampler();
                JavaSamplerContext ctx = TestCtx.params().with("amqp_uri", uri).with("password", pw).with("confirm", "each").build();
                sampler.setupTest(ctx);
                SampleResult r = sampler.runTest(ctx);

                assertFalse(r.isSuccessful());
                assertTrue(log.lines().size() > 0, "setupTest must have logged its failure (else the log assertion is vacuous)");
                String everything = r.getResponseMessage() + "\n" + r.getResponseDataAsString() + "\n"
                        + r.getFirstAssertionFailureMessage() + "\n" + log.text();
                for (String form : forms(pw)) {
                    assertFalse(everything.contains(form), "leaked " + (form.equals(pw) ? "the password" : "an encoded form of it") + " in: " + everything);
                }
                assertTrue(r.getResponseMessage().contains("channel is not open"), r.getResponseMessage());
            }
        }
    }

    static String[] forms(String pw) {
        String user = "svc";
        return new String[] {
                pw,
                Redact.urlEncode(pw),
                Redact.urlEncode(pw).replace("+", "%20"),
                Base64.getEncoder().encodeToString((user + ":" + pw).getBytes(StandardCharsets.UTF_8)),
        };
    }
}
