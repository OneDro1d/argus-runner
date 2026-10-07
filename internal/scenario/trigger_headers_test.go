package scenario

import (
	"strings"
	"testing"
)

// Unit 6 (M25-FX3 / 4.8): custom headers in a TRIGGER (notably Authorization) are parsed so
// the runner can inject a per-scenario token. Without it, PERM-style scenarios silently rode
// the default valid token -> a false "auth bypass".
func TestParse_TriggerHeaders(t *testing.T) {
	s := Parse("## TRIGGER\nPOST `${INGESTION_URL}/api/v1/orders`\nAuthorization: Bearer wrong-token\nX-Custom: hi\n\n```json\n{\"a\":1}\n```\n")
	if s.Trigger.Headers["Authorization"] != "Bearer wrong-token" {
		t.Errorf("Authorization not parsed: %v", s.Trigger.Headers)
	}
	if s.Trigger.Headers["X-Custom"] != "hi" {
		t.Errorf("custom header not parsed: %v", s.Trigger.Headers)
	}
	if _, bad := s.Trigger.Headers["A"]; bad {
		t.Errorf("a JSON body line leaked as a header: %v", s.Trigger.Headers)
	}
	// case-insensitive name; empty value = present (send no token)
	s2 := Parse("## TRIGGER\nPOST `${INGESTION_URL}/x`\nauthorization:\n\n```json\n{}\n```\n")
	if v, ok := s2.Trigger.Headers["Authorization"]; !ok || v != "" {
		t.Errorf("empty authorization should be present-with-empty, ok=%v v=%q", ok, v)
	}
	// no header -> no Authorization override
	s3 := Parse("## TRIGGER\nPOST `${INGESTION_URL}/x`\n\n```json\n{}\n```\n")
	if _, ok := s3.Trigger.Headers["Authorization"]; ok {
		t.Error("no header should mean no Authorization override")
	}
}

// Guard: a header buried inside a code fence must FAIL validation (PERM-002 / PERM-007 footgun) —
// parseTriggerHeaders only reads headers before the first fence, so a fenced Authorization is
// silently dropped and the scenario rides the default valid token (a false "auth bypass").
func TestValidate_BuriedTriggerHeaderRejected(t *testing.T) {
	buried := "## Metadata\n- **ID**: PERM-099\n- **Layer**: Permissions\n\n" +
		"## TRIGGER\nPOST `${INGESTION_URL}/x`\n\n```\nAuthorization: Bearer wrong\n```\n\n```json\n{\"a\":1}\n```\n\n" +
		"## EXPECT\n- status=401\n"
	_, errs := Validate(buried)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "code fence") {
			found = true
		}
	}
	if !found {
		t.Errorf("a header buried in a code fence should be rejected with a code-fence error; got %v", errs)
	}

	// the fix (header bare, before the payload fence) must NOT trip the guard
	fixed := "## Metadata\n- **ID**: PERM-099\n- **Layer**: Permissions\n\n" +
		"## TRIGGER\nPOST `${INGESTION_URL}/x`\nAuthorization: Bearer wrong\n\n```json\n{\"a\":1}\n```\n\n" +
		"## EXPECT\n- status=401\n"
	_, errs2 := Validate(fixed)
	for _, e := range errs2 {
		if strings.Contains(e.Message, "code fence") {
			t.Errorf("a bare header before the payload fence must not trip the guard; got %v", e)
		}
	}
}
