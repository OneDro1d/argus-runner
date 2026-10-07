package scenario

import "testing"

// — the chain http-step claim `- step <name>: unreachable`. These tests pin the
// WRITE-time half (the validator author__validate_scenario uses); the run-time half is pinned in
// internal/chain (unreachable_test.go) and the validator/executor agreement in internal/argus.

const unreachableTrigger = `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  {"type":"http","name":"blocked","method":"GET","url":"http://y/b"}
]}`

func unreachableMD(trigger, blocked string) string {
	return chainMD(trigger, "### Runnable\n- step alive: status=200\n"+blocked)
}

func TestClaimsToBeUnreachable(t *testing.T) {
	for _, c := range []string{"unreachable", "  unreachable  ", "Unreachable"} {
		if !ClaimsToBeUnreachable(c) {
			t.Errorf("%q must be the unreachable claim", c)
		}
	}
	for _, c := range []string{"status=200", "unreachable and status=200", "not unreachable", "body contains unreachable", ""} {
		if ClaimsToBeUnreachable(c) {
			t.Errorf("%q must NOT be the unreachable claim", c)
		}
	}
}

func TestValidate_UnreachableClaimIsAcceptedOnAnHTTPStep(t *testing.T) {
	_, errs := Validate(unreachableMD(unreachableTrigger, "- step blocked: unreachable\n"))
	if len(errs) != 0 {
		t.Fatalf("an http step's `unreachable` claim must validate; got %v", errs)
	}
}

func TestValidate_UnreachableCannotBeCombinedWithAnotherClaim(t *testing.T) {
	for _, other := range []string{"status=200", "status=403", "body contains x", "unreachable"} {
		_, errs := Validate(unreachableMD(unreachableTrigger, "- step blocked: unreachable\n- step blocked: "+other+"\n"))
		if !find(errs, "unreachable") || !find(errs, "combined") || !find(errs, `step "blocked"`) {
			t.Errorf("combined with %q: want a refusal naming the step and saying it cannot be combined; got %v", other, errs)
		}
	}
}

func TestValidate_UnreachableRefusesPollAndAlways(t *testing.T) {
	poll := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  {"type":"http","name":"blocked","method":"GET","url":"http://y/b","poll":{"timeout":"10s","interval":"1s"}}
]}`
	_, errs := Validate(unreachableMD(poll, "- step blocked: unreachable\n"))
	if !find(errs, "unreachable") || !find(errs, "poll") {
		t.Errorf("poll + unreachable must be refused by name; got %v", errs)
	}
	always := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  {"type":"http","name":"blocked","method":"GET","url":"http://y/b","always":true}
]}`
	_, errs = Validate(unreachableMD(always, "- step blocked: unreachable\n"))
	if !find(errs, "unreachable") || !find(errs, "always") {
		t.Errorf("always + unreachable must be refused by name; got %v", errs)
	}
}

// An `unreachable` bullet on an mcp step has no assertion operator, so without a refusal it would be read
// as prose: the step would be judged "expect success" and the author's claim would prove nothing.
func TestValidate_UnreachableIsRefusedOnANonHTTPStep(t *testing.T) {
	trig := `{"steps":[
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"},
  {"type":"mcp","name":"call","tool":"t","args":{}}
]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step alive: status=200\n- step call: unreachable\n"))
	if !find(errs, "unreachable") || !find(errs, "http step") || !find(errs, `step "call"`) {
		t.Errorf("`unreachable` on an mcp step must be refused (it would otherwise read as prose); got %v", errs)
	}
}

// With no earlier POSITIVE step the claim can only ever read not-measured (the run-time guard: only a
// passed step that does not claim `unreachable` opens it), so it is refused when written.
func TestValidate_UnreachableNeedsAnEarlierPositiveStep(t *testing.T) {
	first := `{"steps":[
  {"type":"http","name":"blocked","method":"GET","url":"http://y/b"},
  {"type":"http","name":"alive","method":"GET","url":"http://x/a"}
]}`
	_, errs := Validate(chainMD(first, "### Runnable\n- step blocked: unreachable\n- step alive: status=200\n"))
	if !find(errs, `step "blocked"`) || !find(errs, "earlier") {
		t.Errorf("an unreachable step with no earlier step must be refused by name; got %v", errs)
	}
	onlyUnreachableBefore := `{"steps":[
  {"type":"http","name":"b1","method":"GET","url":"http://y/b"},
  {"type":"http","name":"b2","method":"GET","url":"http://y/c"}
]}`
	_, errs = Validate(chainMD(onlyUnreachableBefore, "### Runnable\n- step b1: unreachable\n- step b2: unreachable\n"))
	if !find(errs, `step "b2"`) {
		t.Errorf("an earlier unreachable step does not open the guard, so b2 must be refused too; got %v", errs)
	}
}

func TestHTTPStepClaims_UnreachableAloneParses(t *testing.T) {
	status, body, err := HTTPStepClaims([]string{"unreachable"})
	if err != nil || status != 0 || len(body) != 0 {
		t.Fatalf("want (0, none, nil); got %d %v %v", status, body, err)
	}
	if _, _, err := HTTPStepClaims([]string{"unreachable", "status=200"}); err == nil {
		t.Fatal("a combined claim must be refused by the shared parser, so the executor refuses it too")
	}
}
