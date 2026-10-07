package scenario

import "testing"

// VR12-E8: unquote strips ONE surrounding layer and must never touch inner quotes — the asserted
// value `"code":"missing_token"` is an ordinary substring and mangling it would silently change
// what the author asked for.
//
// It lives here rather than in internal/argus because the grammar moved: R3 has to bite on all
// THREE author paths, two of which call scenario.Validate alone and never reach argus at all.
func TestUnquoteKeepsInnerQuotes(t *testing.T) {
	for in, want := range map[string]string{
		"`" + `"code":"missing_token"` + "`": `"code":"missing_token"`,
		`"items"`:                            "items",
		`'exceeds'`:                          "exceeds",
		`plain`:                              "plain",
		`"code":"missing_token"`:             `"code":"missing_token"`, // inner quotes → left alone
	} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

// VR12-E8 R3 — A BULLET THAT CLAIMS TO BE A BODY ASSERTION AND PARSES AS NONE IS AN ERROR.
//
// This is the exact `if m == nil { continue }` V29-020 named as the moment the product decides it
// does not understand a line and says nothing. The parser must hand the caller an error it cannot
// ignore, and ClaimsToBeBodyAssert must agree about which bullets are its business — if the two
// disagree, a bullet is either policed by nobody or refused by everybody.
func TestBodyClaimAndParserAgree(t *testing.T) {
	claimsAndParses := []string{
		"- body has order_id",
		"- body has order_id containing 01",
		"- body has data.token matching ^ey[A-Za-z0-9_.-]+$",
		"- body contains accepted",
		"- Body Matching ^\\{",
	}
	claimsAndFails := []string{
		"- body should probably mention the order",
		"- body has order_id containing", // no value
		"- body has data.token matching ([unclosed",
	}
	neitherClaims := []string{
		"- status=202",
		"- the response body is documented in the runbook", // does not OPEN with "body"
		"- result.isError == false",
	}

	for _, b := range claimsAndParses {
		if !ClaimsToBeBodyAssert(b) {
			t.Errorf("ClaimsToBeBodyAssert(%q) = false — a bullet the parser executes must be claimed", b)
		}
		got, errs := ParseBodyAsserts([]string{b})
		if len(errs) > 0 {
			t.Errorf("ParseBodyAsserts(%q) errored: %v", b, errs[0])
		}
		if len(got) != 1 {
			t.Errorf("ParseBodyAsserts(%q) produced %d assertions, want 1", b, len(got))
		}
	}
	for _, b := range claimsAndFails {
		if !ClaimsToBeBodyAssert(b) {
			t.Errorf("ClaimsToBeBodyAssert(%q) = false — it opens with `body`, so it is this grammar's business", b)
		}
		if _, errs := ParseBodyAsserts([]string{b}); len(errs) == 0 {
			t.Errorf("ParseBodyAsserts(%q) returned NO error — this is the silent skip V29-020 exists to remove", b)
		}
	}
	for _, b := range neitherClaims {
		if ClaimsToBeBodyAssert(b) {
			t.Errorf("ClaimsToBeBodyAssert(%q) = true — a bullet that does not open with `body` is somebody else's", b)
		}
		if got, errs := ParseBodyAsserts([]string{b}); len(got) > 0 || len(errs) > 0 {
			t.Errorf("ParseBodyAsserts(%q) claimed a bullet that is not its business: %v / %v", b, got, errs)
		}
	}
}
