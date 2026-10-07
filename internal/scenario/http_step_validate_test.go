package scenario

import "testing"

// AC-D20 — an "http" chain step is refused BY NAME at authoring time, never left to fail
// unhelpfully at preflight or (worse) to silently run with a nonsensical shape. Each case here
// pins the SPECIFIC diagnosis: a missing method/url, a poll with no claim to check on each
// attempt, a poll timeout past the 10m bound, and a field the http step shape does not carry.

func httpChainMD(trigger, expect string) string { return chainMD(trigger, expect) }

// A missing method or url is refused by name.
func TestValidate_HTTPStepMissingMethodOrURLRefused(t *testing.T) {
	noMethod := `{"steps":[{"type":"http","name":"create","url":"http://x/y"}]}`
	_, errs := Validate(httpChainMD(noMethod, "### Runnable\n- step create: status=200\n"))
	if !find(errs, "declares no `method`/`url`") {
		t.Fatalf("a missing method must be refused by name; got %v", errs)
	}

	noURL := `{"steps":[{"type":"http","name":"create","method":"POST"}]}`
	_, errs2 := Validate(httpChainMD(noURL, "### Runnable\n- step create: status=200\n"))
	if !find(errs2, "declares no `method`/`url`") {
		t.Fatalf("a missing url must be refused by name; got %v", errs2)
	}
}

// A poll with no claim is refused — it would just spin for the whole timeout every run, since
// nothing decides "pass" on any given attempt.
func TestValidate_HTTPStepPollWithoutClaimRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"poll","method":"GET","url":"http://x/y",` +
		`"poll":{"timeout":"30s","interval":"5s"}}]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n"))
	if !find(errs, "declares `poll` but has no claim") {
		t.Fatalf("a poll step with no claim must be refused by name; got %v", errs)
	}
}

// A poll timeout above the 10-minute bound is refused.
func TestValidate_HTTPStepPollTimeoutOver10MinutesRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"poll","method":"GET","url":"http://x/y",` +
		`"poll":{"timeout":"11m","interval":"5s"}}]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n- step poll: status=200\n"))
	if !find(errs, "more than the 10m bound") {
		t.Fatalf("a poll.timeout over 10m must be refused by name; got %v", errs)
	}
}

// An unknown field on an http step is refused — the wire struct is shared across mcp/ui/http, so a
// stray field (e.g. `tool`, left over from copy-pasting an mcp step) must not be silently ignored.
func TestValidate_HTTPStepUnknownFieldRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"create","method":"POST","url":"http://x/y","tool":"t_create"}]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n- step create: status=200\n"))
	if !find(errs, `carries the field "tool"`) {
		t.Fatalf("an unknown field must be refused by name; got %v", errs)
	}
}

// A well-formed http step (method, url, a claim) is accepted outright — the guards above must not
// be so broad that they refuse a valid step.
func TestValidate_HTTPStepWellFormedAccepted(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"create","method":"POST","url":"http://x/y",` +
		`"headers":{"X-A":"b"},"body":{"a":1},"save":{"id":"id"},"always":false}]}`
	_, errs := Validate(httpChainMD(trig, "### Runnable\n- step create: status=201\n"))
	if len(errs) != 0 {
		t.Fatalf("a well-formed http step must be accepted, got %v", errs)
	}
}
