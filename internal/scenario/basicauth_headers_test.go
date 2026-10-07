package scenario

import (
	"strings"
	"testing"
)

// basicauth_headers_test.go — item 25(a): the basic-auth helper grammar (headers.go) and its
// authoring-time validation (validate.go), on both the TRIGGER Authorization header and a chain
// http step's own `headers`.

func TestBasicAuthMarker_ParsesWellFormed(t *testing.T) {
	user, passEnv, ok := BasicAuthMarker("Basic ${basic_auth:alice:ALICE_PASSWORD}")
	if !ok || user != "alice" || passEnv != "ALICE_PASSWORD" {
		t.Fatalf("got user=%q passEnv=%q ok=%v, want alice/ALICE_PASSWORD/true", user, passEnv, ok)
	}
}

func TestBasicAuthMarker_NotAttemptedFallsThrough(t *testing.T) {
	if _, _, ok := BasicAuthMarker("Bearer ${TOKEN}"); ok {
		t.Error("an ordinary Bearer header must not be read as the basic-auth marker")
	}
	if _, _, ok := BasicAuthMarker(""); ok {
		t.Error("an empty header value must not be read as the basic-auth marker")
	}
}

func TestValidateBasicAuthHeader_MalformedFormsRefusedByName(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"no env var named", "Basic ${basic_auth:alice}", "names no env var"},
		{"empty username", "Basic ${basic_auth::ALICE_PASSWORD}", "empty username"},
		{"bad env var name", "Basic ${basic_auth:alice:9BAD-NAME}", "not a valid environment variable name"},
	}
	for _, c := range cases {
		why := ValidateBasicAuthHeader(c.value)
		if why == "" {
			t.Errorf("%s: expected a refusal reason for %q, got none", c.name, c.value)
			continue
		}
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: reason %q does not mention %q", c.name, why, c.want)
		}
	}
}

func TestValidateBasicAuthHeader_WellFormedAndUnrelatedPass(t *testing.T) {
	if why := ValidateBasicAuthHeader("Basic ${basic_auth:alice:ALICE_PASSWORD}"); why != "" {
		t.Errorf("a well-formed marker must not be refused: %q", why)
	}
	if why := ValidateBasicAuthHeader("Bearer ${TOKEN}"); why != "" {
		t.Errorf("an unrelated header form must not be refused: %q", why)
	}
}

// Authoring-time refusal on the TRIGGER's own Authorization header.
func TestValidate_MalformedBasicAuthOnTriggerRefusedByName(t *testing.T) {
	md := strings.Replace(validMD(), "POST `${INGESTION_URL}/api/v1/orders`",
		"POST `${INGESTION_URL}/api/v1/orders`\nAuthorization: Basic ${basic_auth:alice}", 1)
	_, errs := Validate(md)
	if !find(errs, "names no env var") {
		t.Fatalf("a malformed basic-auth TRIGGER header must be refused by name; got %v", errs)
	}
}

func TestValidate_WellFormedBasicAuthOnTriggerAccepted(t *testing.T) {
	md := strings.Replace(validMD(), "POST `${INGESTION_URL}/api/v1/orders`",
		"POST `${INGESTION_URL}/api/v1/orders`\nAuthorization: Basic ${basic_auth:alice:ALICE_PASSWORD}", 1)
	_, errs := Validate(md)
	if find(errs, "basic-auth") {
		t.Fatalf("a well-formed basic-auth TRIGGER header must not be refused; got %v", errs)
	}
}

// Authoring-time refusal on a chain http step's own headers.
func TestValidate_MalformedBasicAuthOnChainHTTPStepHeaderRefusedByName(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"login","method":"GET","url":"http://x/y",` +
		`"headers":{"Authorization":"Basic ${basic_auth:alice}"}}]}`
	md := chainMD(trig, "### Runnable\n- step login: status=200\n")
	_, errs := Validate(md)
	if !find(errs, `step "login"`) || !find(errs, "names no env var") {
		t.Fatalf("a malformed basic-auth chain-step header must be refused by the step's name; got %v", errs)
	}
}
