package argus

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ── THE APP-UNDER-TEST'S CREDENTIALS NEVER TRAVEL AS `-J` ────────────────────────────────────────
//
// JMeter echoes every `-J` property it receives into its run log — JMeter rel/v5.6.3,
// src/core/src/main/java/org/apache/jmeter/JMeter.java, initializeProperties():
//
//	case JMETER_PROPERTY:
//	    log.info("Setting JMeter property: {}={}", name, value);
//
// so `-Jauth.header=Bearer …` used to land, in cleartext, in <results>/<template>__<id>.jtl.log.
// A properties file passed with `-q` is different: the same method logs only its PATH,
//
//	case PROPFILE2_OPT:
//	    log.info("Loading additional properties from: {}", name);
//	    … Properties tmp = new Properties(); tmp.load(fis); jmeterProps.putAll(tmp);
//
// and nothing in JMeter enumerates the loaded property set afterwards. Both routes fill the same
// jmeterProps, so `${__P(auth.header)}` in a template resolves exactly as before. The runners
// therefore split the property set: the credential-bearing names go through a private, 0600,
// run-scoped properties file that is removed when JMeter exits; everything else stays `-J`.
// As a side effect the credentials are no longer visible in the process list either.
//
// ⛔ The split is by NAME, and the list is closed on purpose: a new property that carries a
// credential must be added here (secret_props_test.go pins the set), never worked around by
// reusing a public name.

// secretPropNames are the JMeter properties that carry the app-under-test's credentials.
var secretPropNames = map[string]bool{
	"auth.header":      true, // targets.auth.bearer_token, or the scenario's own Authorization header
	"db.password":      true, // targets.database.password
	"mgmt.auth.header": true, // the broker's management-API basic auth, from targets.message_broker.url
	"mq.amqp.username": true, // the AMQP login: visible in `ps` as -J, and half of the user:password pair
	"mq.amqp.password": true, // AC-D16: the AMQP connection password, split out of targets.message_broker.url
}

// isSecretProp reports whether a JMeter property may carry a credential. Besides the named set,
// every author-declared header VALUE (`trigger.header_<n>_value`, scenario/headers.go) is treated
// as one: a header value is the likeliest place in a scenario to hold a credential, and a
// two-identity SUT writes its second token there (`X-Api-Key: ${OPERATOR_KEY}`).
func isSecretProp(name string) bool {
	if secretPropNames[name] {
		return true
	}
	return strings.HasPrefix(name, "trigger.header_") && strings.HasSuffix(name, "_value")
}

// splitSecretProps partitions props into the public set (safe as `-J`) and the secret set (the
// properties file). Neither map aliases the input.
func splitSecretProps(props map[string]string) (public, secret map[string]string) {
	public, secret = map[string]string{}, map[string]string{}
	for k, v := range props {
		if isSecretProp(k) {
			secret[k] = v
		} else {
			public[k] = v
		}
	}
	return public, secret
}

// sortedKeys is the deterministic iteration order every argv/file builder here uses.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// encodeProperties renders m in java.util.Properties format, the way Properties.store does: one
// `key=value` per line, with `\`, `=`, `:`, `#`, `!`, leading spaces and the control characters
// escaped, and everything outside printable ASCII as \uXXXX — Properties.load(InputStream) reads
// ISO-8859-1, so a token with a non-Latin1 character would otherwise arrive corrupted.
func encodeProperties(m map[string]string) []byte {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(escapeProperty(k, true))
		b.WriteByte('=')
		b.WriteString(escapeProperty(m[k], false))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func escapeProperty(s string, isKey bool) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == ' ' && (isKey || i == 0):
			b.WriteString(`\ `)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '=' || r == ':' || r == '#' || r == '!':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeSecretPropsFile writes secret to a fresh, private (0600) file in the temp dir and returns
// its path with the function that removes it. An empty secret set writes nothing and returns "".
// os.CreateTemp creates the file with mode 0600 and O_EXCL, so no other user can read it and no
// pre-placed path can be reused.
func writeSecretPropsFile(secret map[string]string) (path string, remove func(), err error) {
	if len(secret) == 0 {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "argus-run-*.properties")
	if err != nil {
		return "", nil, fmt.Errorf("private properties file: %w", err)
	}
	path = f.Name()
	remove = func() { _ = os.Remove(path) }
	if _, err := f.Write(encodeProperties(secret)); err != nil {
		_ = f.Close()
		remove()
		return "", nil, fmt.Errorf("private properties file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("private properties file: %w", err)
	}
	return path, remove, nil
}
