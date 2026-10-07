package scenario

import "testing"

// reserved_step_headers_test.go — #606: the two names an author may not set (V29-018) are refused on a
// chain http step's own `headers` too, BY NAME and in any letter case, not only on ## TRIGGER.
func TestValidate_ReservedHeaderOnChainHTTPStepRefusedByName(t *testing.T) {
	for _, name := range []string{"X-Correlation-Id", "x-correlation-id", "MCP-SESSION-ID"} {
		trig := `{"steps":[{"type":"http","name":"call","method":"GET","url":"http://x/y",` +
			`"headers":{"` + name + `":"mine"}}]}`
		md := chainMD(trig, "### Runnable\n- step call: status=200\n")
		_, errs := Validate(md)
		if !find(errs, `step "call" declares the header "`+name+`"`) || !find(errs, "may not set") {
			t.Errorf("%s on a chain http step must be refused by the step's name; got %v", name, errs)
		}
	}
}

func TestValidate_OrdinaryHeaderOnChainHTTPStepIsNotRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"call","method":"GET","url":"http://x/y",` +
		`"headers":{"X-Request-Source":"argus"}}]}`
	md := chainMD(trig, "### Runnable\n- step call: status=200\n")
	_, errs := Validate(md)
	if find(errs, "may not set") {
		t.Fatalf("an ordinary step header must not be refused as reserved; got %v", errs)
	}
}
