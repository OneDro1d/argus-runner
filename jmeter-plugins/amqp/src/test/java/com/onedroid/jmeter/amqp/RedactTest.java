package com.onedroid.jmeter.amqp;

import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.Base64;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** S10: {@link Redact}. Every case first proves the secret IS in the input (the positive control). */
class RedactTest {

    private static final String PW = "p@ss w0rd/9+x";

    private static void present(String in, String secret) {
        assertTrue(in.contains(secret), "control: the secret must be in the input, or the test proves nothing");
    }

    @Test
    void removesTheBareSecret() {
        String in = "login refused for pw=" + PW + " on host";
        present(in, PW);
        String out = Redact.scrub(in, PW);
        assertFalse(out.contains(PW), out);
        assertTrue(out.contains(Redact.MASK), out);
    }

    @Test
    void removesTheUrlEncodedForms() {
        String enc = Redact.urlEncode(PW);
        String in = "amqp-uri-fragment " + enc + " and " + enc.replace("+", "%20");
        present(in, enc);
        String out = Redact.scrub(in, PW);
        assertFalse(out.contains(enc), out);
        assertFalse(out.contains(enc.replace("+", "%20")), out);
    }

    @Test
    void removesTheBase64OfUserColonSecret() {
        String b64 = Base64.getEncoder().encodeToString(("svc:" + PW).getBytes(StandardCharsets.UTF_8));
        String in = "Authorization: Basic " + b64;
        present(in, b64);
        String out = Redact.scrub(in, Redact.secretsOf(null, "svc", PW));
        assertFalse(out.contains(b64), out);
    }

    @Test
    void removesTheUserinfoShapeEvenForAnUnknownSecret() {
        String in = "bad uri amqp://someone:unknown-secret@bad host:notaport/";
        present(in, "unknown-secret");
        String out = Redact.scrub(in); // no secret list at all: shape redaction only
        assertFalse(out.contains("unknown-secret"), out);
        assertFalse(out.contains("someone"), out);
        assertTrue(out.contains("amqp://" + Redact.MASK + "@"), out);
    }

    @Test
    void secretsOfFindsThePasswordInsideTheUri() {
        String[] s = Redact.secretsOf("amqp://u:inuri%40pw@host:5672", "", "");
        String joined = String.join("|", s);
        assertTrue(joined.contains("inuri%40pw"), joined);
        assertTrue(joined.contains("inuri@pw"), "the percent-decoded form is a secret too: " + joined);
    }

    @Test
    void emptyAndNullAreHarmless() {
        assertNull(Redact.scrub(null, "x"));
        assertEquals("", Redact.scrub("", "x"));
        assertEquals("nothing to hide", Redact.scrub("nothing to hide", "", null));
    }

    @Test
    void longestSecretGoesFirstSoNoRemnantIsLeft() {
        String out = Redact.scrub("value=abcdef", "abc", "abcdef");
        assertEquals("value=" + Redact.MASK, out);
    }
}
