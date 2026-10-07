package com.onedroid.jmeter.amqp;

import java.io.UnsupportedEncodingException;
import java.net.URLDecoder;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * S10 (Java side): no broker credential reaches a sample result or a log line. The twin of the Go
 * {@code amqpengine.RedactURL} (internal/amqpengine/redact.go), widened the way S10 asks: it removes
 * the secret VALUES the sampler knows (a shape redaction alone would leave a bare password inside any
 * text that is not URL-shaped) as well as the {@code scheme://userinfo@} shape.
 *
 * Applied at the two sinks only: the sample's {@code responseMessage}/{@code responseData}, and every
 * log call (see {@link AmqpPublishSampler}).
 */
final class Redact {

    static final String MASK = "[REDACTED]";

    /** scheme://userinfo@ -- the userinfo (anything up to the first '/', '?', '#', '@' or whitespace). */
    private static final Pattern USERINFO =
            Pattern.compile("([A-Za-z][A-Za-z0-9+.\\-]*://)[^/?#@\\s]*@");

    private Redact() {
    }

    /**
     * Replaces every non-empty secret, its URL-encoded forms, and the {@code scheme://userinfo@} shape
     * with {@link #MASK}. Longest secret first, so a secret that contains another is removed whole.
     */
    static String scrub(String s, String... secrets) {
        if (s == null || s.isEmpty()) {
            return s;
        }
        List<String> forms = new ArrayList<String>();
        if (secrets != null) {
            for (String secret : secrets) {
                if (secret == null || secret.isEmpty()) {
                    continue;
                }
                forms.add(secret);
                String enc = urlEncode(secret);
                forms.add(enc);
                forms.add(enc.replace("+", "%20"));
            }
        }
        Collections.sort(forms, new Comparator<String>() {
            @Override
            public int compare(String a, String b) {
                return b.length() - a.length();
            }
        });
        String out = s;
        for (String f : forms) {
            if (!f.isEmpty()) {
                out = out.replace(f, MASK);
            }
        }
        Matcher m = USERINFO.matcher(out);
        return m.replaceAll("$1" + MASK + "@");
    }

    /**
     * Every secret form the sampler can know from its three credential inputs: the password parameter,
     * the password inside the URI's userinfo (raw and percent-decoded), and the Base64 of
     * {@code user:password} (what an Authorization header would carry) and of the password alone.
     */
    static String[] secretsOf(String uri, String username, String password) {
        Set<String> out = new LinkedHashSet<String>();
        List<String[]> pairs = new ArrayList<String[]>(); // {user, password}
        if (password != null && !password.isEmpty()) {
            pairs.add(new String[] {username == null ? "" : username, password});
        }
        if (uri != null) {
            Matcher m = USERINFO.matcher(uri);
            if (m.find()) {
                String ui = m.group().substring(m.group(1).length(), m.group().length() - 1);
                int colon = ui.indexOf(':');
                if (colon >= 0) {
                    String user = ui.substring(0, colon);
                    String pw = ui.substring(colon + 1);
                    if (!pw.isEmpty()) {
                        pairs.add(new String[] {user, pw});
                        pairs.add(new String[] {urlDecode(user), urlDecode(pw)});
                    }
                }
            }
        }
        for (String[] p : pairs) {
            String user = p[0];
            String pw = p[1];
            if (pw.isEmpty()) {
                continue;
            }
            out.add(pw);
            addBase64(out, user + ":" + pw);
            addBase64(out, pw);
            if (username != null && !username.isEmpty() && !username.equals(user)) {
                addBase64(out, username + ":" + pw);
            }
        }
        return out.toArray(new String[0]);
    }

    private static void addBase64(Set<String> out, String plain) {
        byte[] b = plain.getBytes(StandardCharsets.UTF_8);
        String padded = Base64.getEncoder().encodeToString(b);
        out.add(padded);
        out.add(Base64.getEncoder().withoutPadding().encodeToString(b));
    }

    static String urlEncode(String s) {
        try {
            return URLEncoder.encode(s, "UTF-8");
        } catch (UnsupportedEncodingException e) {
            throw new IllegalStateException(e); // UTF-8 is always present
        }
    }

    private static String urlDecode(String s) {
        try {
            return URLDecoder.decode(s, "UTF-8");
        } catch (Exception e) {
            return s;
        }
    }
}
