package amqpengine

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// userinfoPattern matches the "scheme://userinfo@" prefix of any URL-shaped
// substring — e.g. it finds "amqp://produser:s3cr3t@" inside a longer string
// such as a dial error or a redirect message, and captures the scheme so it
// can be put back without the credentials.
var userinfoPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)

// RedactURL replaces the userinfo component of every URL-shaped substring in
// s with "REDACTED@". It is applied to every error amqpengine returns or
// wraps, so a connection URL's credentials can never surface in a probe's
// Outcome.Text, a Connect error, or a test log — see TestRedactURL for the
// cases this must hold for (bare userinfo, userinfo with a colon-password,
// multiple URLs in one string, and a string with none at all).
func RedactURL(s string) string {
	return userinfoPattern.ReplaceAllString(s, "${1}REDACTED@")
}

// secretMask is what a scrubbed secret becomes.
const secretMask = "[REDACTED]"

// RedactSecrets scrubs known secret VALUES (the S10 rule): every non-empty secret, its
// URL-encoded forms and its Base64 forms, then the userinfo of URL-shaped substrings (RedactURL).
// RedactURL alone is a SHAPE redaction: a bare password in a string that is not a URL is not touched, and
// JMeter's own error text can carry exactly that. A caller that must also catch an HTTP basic-auth blob
// passes "user:secret" as one more secret, whose Base64 form is then covered too.
func RedactSecrets(s string, secrets ...string) string {
	var forms []string
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		forms = append(forms, sec, url.QueryEscape(sec), url.PathEscape(sec),
			base64.StdEncoding.EncodeToString([]byte(sec)), base64.RawStdEncoding.EncodeToString([]byte(sec)),
			base64.URLEncoding.EncodeToString([]byte(sec)), base64.RawURLEncoding.EncodeToString([]byte(sec)))
	}
	// longest first: a form that contains another (the raw secret inside its own URL-encoding) must not be
	// half-replaced into a fragment that still identifies it.
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, f := range forms {
		s = strings.ReplaceAll(s, f, secretMask)
	}
	return RedactURL(s)
}
