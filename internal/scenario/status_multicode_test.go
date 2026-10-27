package scenario

import (
	"strings"
	"testing"
)

// (a tester's acceptance run) — A STATUS CLAIM THAT LISTED TWO CODES KEPT THE FIRST AND
// SAID NOTHING.
//
// statusDeclRe ends in `\b`, which matches before `/`, `,`, `|` or a space, so `status=200/201`,
// `status=200,201`, `status=200|201` and `status=200 or 201` all counted as the declaration `status=200`
// and the second code vanished: the run judged 200 only, and an author who meant "either" was told
// nothing. The decision is NOT to implement either-of matching but to REFUSE the shape loudly, in the one
// reader both doors share (CheckDeclaredStatuses): the validator (Validate, HTTPStepClaims) and the
// executor (DeriveProps, the run loop's preflight, the chain http step).

func TestCheckDeclaredStatuses_AcceptedAndRefusedForms(t *testing.T) {
	accepted := []string{
		"status=200",
		"status: 404",
		"STATUS = 201",
		"status=201 and the body has an id",
		"status=200, the body has an id",      // a comma, then prose: not a second code
		"status=200 or the order is rejected", // `or`, then prose
		"status2=409",
		"status2: 409 on the replay",
		"status=200 and 2 items are returned", // a number that is not a 3-digit code
		"body has status containing 201",      // not a status declaration at all
		"status=200 (see RFC 7231 section 6.3.1)",
	}
	for _, b := range accepted {
		if err := CheckDeclaredStatuses([]string{b}); err != nil {
			t.Errorf("%q must keep working, got refusal: %v", b, err)
		}
	}

	refused := []string{
		"status=200/201",
		"status=200 / 201",
		"status=200,201",
		"status=200, 201",
		"status=200|201",
		"status=200 | 201",
		"status=200 or 201",
		"status=200 OR 201",
		"status: 200/201",
		"status2=409/410",
		"status2: 409 or 410",
		"status=200/201/202",
		"status=200/201 and the body has an id",
	}
	for _, b := range refused {
		err := CheckDeclaredStatuses([]string{b})
		if err == nil {
			t.Errorf("%q lists two codes and must be REFUSED, not read as its first code", b)
			continue
		}
		msg := err.Error()
		for _, want := range []string{"lists two codes", "exactly one code", "status2=", "second REQUEST"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: the refusal must say %q; got %q", b, want, msg)
			}
		}
	}

	// the refusal names the claim the author wrote
	err := CheckDeclaredStatuses([]string{"status=202", "status=200/201"})
	if err == nil || !strings.Contains(err.Error(), "status=200/201") {
		t.Errorf("the refusal must quote the offending claim; got %v", err)
	}
	// and one bad bullet among good ones refuses the lot
	if err := CheckDeclaredStatuses([]string{"status=202", "status2=409", "body has order_id containing 01"}); err != nil {
		t.Errorf("an ordinary idempotency pair must be accepted: %v", err)
	}
}

// DeclaredStatuses (the reader) is unchanged for what it accepts: the first code is still the one read.
// This pins that the refusal is a SEPARATE check on the same grammar, not a behaviour change of the
// reader the other classifiers (tier 3, the money guard, severity) call without an error return.
func TestDeclaredStatuses_ReaderUnchanged(t *testing.T) {
	if first, second := DeclaredStatuses([]string{"status=202", "status2=409"}); first != 202 || second != 409 {
		t.Errorf("DeclaredStatuses = %d,%d want 202,409", first, second)
	}
}

// THE VALIDATOR, plain scenario: Validate refuses it, naming the problem.
func TestValidate_RefusesAStatusClaimListingTwoCodes(t *testing.T) {
	for _, claim := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		md := strings.Replace(validMD(), "- status=202", "- "+claim, 1)
		if !strings.Contains(md, "- "+claim) {
			t.Fatalf("fixture did not take %q", claim)
		}
		_, errs := Validate(md)
		if !find(errs, "lists two codes") || !find(errs, "exactly one code") {
			t.Errorf("%q: Validate must refuse a two-code status claim; got %v", claim, errs)
		}
	}
	// a second REQUEST's status is not an alternative and stays legal beside the first
	md := strings.Replace(validMD(), "- status=202", "- status=202\n- status2=409", 1)
	if _, errs := Validate(md); find(errs, "lists two codes") {
		t.Errorf("status + status2 is the idempotency pair and must not be refused: %v", errs)
	}
	// and an ordinary form keeps validating clean of this rule
	if _, errs := Validate(validMD()); find(errs, "lists two codes") {
		t.Errorf("status=202 must not be refused: %v", errs)
	}
}

// THE VALIDATOR, chain http step: the claim goes through HTTPStepClaims, which the executor calls too.
func TestValidate_RefusesATwoCodeStatusClaimOnAChainHTTPStep(t *testing.T) {
	_, errs := Validate(httpSavedMD("status=200/201"))
	if !find(errs, "lists two codes") || !find(errs, `step "second"`) {
		t.Fatalf("a chain http step's two-code status claim must be refused at write time, naming the step; got %v", errs)
	}
}

// THE EXECUTOR'S CHAIN DOOR: HTTPStepClaims is what internal/argus.httpStepExpect delegates to.
func TestHTTPStepClaims_RefusesATwoCodeStatusClaim(t *testing.T) {
	for _, c := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		if st, _, err := HTTPStepClaims([]string{c}); err == nil {
			t.Errorf("%q: HTTPStepClaims returned status %d and no error — the second code was dropped silently", c, st)
		} else if !strings.Contains(err.Error(), "lists two codes") {
			t.Errorf("%q: the refusal must name the problem; got %v", c, err)
		}
	}
	if st, _, err := HTTPStepClaims([]string{"status=201 and the body has an id"}); err != nil || st != 201 {
		t.Errorf("an ordinary claim must keep working: status %d err %v", st, err)
	}
}
