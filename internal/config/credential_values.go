package config

import (
	"net/url"
	"os"
	"strings"
)

// CredentialValues returns the RESOLVED secret values of the wiring/credential fields, for a caller that
// must scrub them out of text it is about to keep (ARGUS-CMP-3, design 1.5: a recorded output).
//
// It reads the one walker (walkCredentialFields), so a field added there is covered here. Which values:
//
//   - a field named password or bearer_token: the value, whatever its length (a secret must never be kept);
//
//   - a jdbc_url: the whole value, and its password parameter;
//
//   - a url / base_url / management_url: the password in its userinfo and the `user:password` pair (the
//     rest of a URL is not secret, and its userinfo shape is already scrubbed by amqpengine.RedactURL).
//
//   - every name declared under `check_env`: its value from the environment, when non-empty.
//
// A user name alone is not returned: it is not a secret, and a short one ("sa") would garble every
// response that contains those two letters. Call it on a Load()-ed config (the values are resolved);
// on a ParseUnresolved one it returns the ${VAR} text, which is harmless. Empty values are skipped.
func (c *Config) CredentialValues() []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	c.walkCredentialFields(func(field, label string) string {
		switch {
		case strings.HasSuffix(label, ".password"), strings.HasSuffix(label, ".bearer_token"):
			add(field)
		case strings.HasSuffix(label, ".jdbc_url"):
			add(field)
			if i := strings.IndexByte(field, '?'); i >= 0 {
				if q, err := url.ParseQuery(field[i+1:]); err == nil {
					add(q.Get("password"))
				}
			}
		case strings.HasSuffix(label, "url"):
			if u, err := url.Parse(field); err == nil && u.User != nil {
				if pw, ok := u.User.Password(); ok {
					add(pw)
					add(u.User.Username() + ":" + pw)
				}
			}
		}
		return field // never rewrite a field
	})
	// (amended): a value of a name declared under `check_env` is a secret exactly like a
	// credential field's, whatever field the check wrote it in. Read from the environment, non-empty;
	// an unset or empty name adds nothing.
	for _, n := range c.CheckEnvNames() {
		add(os.Getenv(n))
	}
	return out
}
