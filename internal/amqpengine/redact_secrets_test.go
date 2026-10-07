package amqpengine

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// S10: RedactURL is a SHAPE redaction; RedactSecrets scrubs VALUES.
func TestRedactSecrets_ValueEncodedAndBase64Forms_WithPositiveControl(t *testing.T) {
	secret := "Zq7" + "-ld-" + "k9x W4/+" // spaces and url-special characters on purpose
	pair := "loaduser:" + secret
	enc := url.QueryEscape(secret)
	b64 := base64.StdEncoding.EncodeToString([]byte(pair))
	in := "connect failed amqp://loaduser:" + secret + "@host:5672/ raw=" + secret + " enc=" + enc + " basic=" + b64

	for name, needle := range map[string]string{"raw": secret, "encoded": enc, "base64": b64} { // CONTROL
		if !strings.Contains(in, needle) {
			t.Fatalf("control failed: the input lacks the %s form", name)
		}
	}
	got := RedactSecrets(in, secret, pair)
	for name, needle := range map[string]string{"raw": secret, "encoded": enc, "base64": b64} {
		if strings.Contains(got, needle) {
			t.Errorf("the %s form survives: %s", name, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("nothing was replaced: %s", got)
	}
}

func TestRedactSecrets_EmptySecretsAreIgnoredAndOtherTextIsUntouched(t *testing.T) {
	in := "NOT_FOUND - no queue 'argus-load-1-s1-0001'"
	if got := RedactSecrets(in, "", ""); got != in {
		t.Errorf("an empty secret rewrote the text: %q", got)
	}
}

func TestRedactSecrets_AlsoAppliesTheUserinfoShape(t *testing.T) {
	got := RedactSecrets("dial amqp://u:unlisted@h:5672/ failed")
	if strings.Contains(got, "unlisted") {
		t.Errorf("userinfo shape not redacted: %s", got)
	}
}
