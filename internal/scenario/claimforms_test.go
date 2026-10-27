package scenario

import (
	"strings"
	"testing"
)

// The grammar of the new claim forms: accepted spellings, and each refusal BY NAME.

func TestParseBodyAsserts_NewForms_Accepted(t *testing.T) {
	cases := []struct {
		bullet string
		want   BodyAssert
	}{
		// 1. numeric equality
		{"- body has count == 5", BodyAssert{Field: "count", Op: BodyEQ, Value: "5"}},
		{"- body has price.usd == 3.14", BodyAssert{Field: "price.usd", Op: BodyEQ, Value: "3.14"}},
		{"- body has n == -2", BodyAssert{Field: "n", Op: BodyEQ, Value: "-2"}},
		{"- body has n==1e0", BodyAssert{Field: "n", Op: BodyEQ, Value: "1e0"}},
		{"- body has n == ${saved.base}", BodyAssert{Field: "n", Op: BodyEQ, Value: "${saved.base}"}},
		// 2. tolerance
		{"- body has amount within 0.5 of 100", BodyAssert{Field: "amount", Op: BodyWithin, Tol: "0.5", Value: "100"}},
		{"- body has amount within 2% of 100", BodyAssert{Field: "amount", Op: BodyWithin, Tol: "2%", Value: "100"}},
		{"- body has amount within 0 of -3.5", BodyAssert{Field: "amount", Op: BodyWithin, Tol: "0", Value: "-3.5"}},
		{"- body has amount WITHIN 1 OF 5", BodyAssert{Field: "amount", Op: BodyWithin, Tol: "1", Value: "5"}},
		{"- body has amount within 1.5% of ${saved.p}", BodyAssert{Field: "amount", Op: BodyWithin, Tol: "1.5%", Value: "${saved.p}"}},
		// 3. negatives
		{"- body not contains error", BodyAssert{Op: BodyNotContains, Value: "error"}},
		{`- body not contains "code":"x"`, BodyAssert{Op: BodyNotContains, Value: `"code":"x"`}},
		{"- body not containing error", BodyAssert{Op: BodyNotContains, Value: "error"}},
		{`- body not matching "status":\s*"failed"`, BodyAssert{Op: BodyNotMatches, Value: `"status":\s*"failed"`}},
		{"- body has s not containing bad", BodyAssert{Field: "s", Op: BodyNotContains, Value: "bad"}},
		{"- body has s not matching ^x", BodyAssert{Field: "s", Op: BodyNotMatches, Value: "^x"}},
		{"- body has s not equals b", BodyAssert{Field: "s", Op: BodyNotEquals, Value: "b"}},
		{"- body has a.b.c NOT EQUALS b", BodyAssert{Field: "a.b.c", Op: BodyNotEquals, Value: "b"}},
		// 4. absence
		{"- body has no secret", BodyAssert{Field: "secret", Op: BodyAbsent}},
		{"- body has no data.token", BodyAssert{Field: "data.token", Op: BodyAbsent}},
		// 5. every element
		{"- body every rows has status", BodyAssert{Every: "rows", Field: "status", Op: BodyExists}},
		{"- body every rows has status containing ok", BodyAssert{Every: "rows", Field: "status", Op: BodyContains, Value: "ok"}},
		{"- body every data.rows has status not equals failed", BodyAssert{Every: "data.rows", Field: "status", Op: BodyNotEquals, Value: "failed"}},
		{"- body every rows has no secret", BodyAssert{Every: "rows", Field: "secret", Op: BodyAbsent}},
		{"- body every rows has n > 0", BodyAssert{Every: "rows", Field: "n", Op: BodyGT, Value: "0"}},
		{"- body every rows has n == 1", BodyAssert{Every: "rows", Field: "n", Op: BodyEQ, Value: "1"}},
		{"- body every rows has n within 1% of 100", BodyAssert{Every: "rows", Field: "n", Op: BodyWithin, Tol: "1%", Value: "100"}},
		{"- body every $ has id", BodyAssert{Every: "$", Field: "id", Op: BodyExists}},
		{"- body every ids has . matching ^a\\d$", BodyAssert{Every: "ids", Field: ".", Op: BodyMatches, Value: `^a\d$`}},
		{"- body every ids has . within 0.5 of 10", BodyAssert{Every: "ids", Field: ".", Op: BodyWithin, Tol: "0.5", Value: "10"}},
	}
	for _, c := range cases {
		got, errs := ParseBodyAsserts([]string{c.bullet})
		if len(errs) != 0 || len(got) != 1 || got[0] != c.want {
			t.Errorf("%q: got %+v errs %v, want %+v", c.bullet, got, errs, c.want)
		}
	}
}

func TestParseBodyAsserts_NewForms_RefusedByName(t *testing.T) {
	cases := []struct{ bullet, mention string }{
		// bad operators stay refused
		{"- body has n => 5", `"=>"`},
		{"- body has n =< 5", `"=<"`},
		// `==` needs a plain number
		{"- body has n == five", "not a number"},
		{"- body has n == inf", "not a number"},
		{"- body has n == 0x10", "not a number"},
		// tolerance
		{"- body has n within 0.5", "tolerance claim"},
		{"- body has n within 0.5 of", "tolerance claim"},
		{"- body has n within of 5", "tolerance claim"},
		{"- body has n within -1 of 5", "negative"},
		{"- body has n within -2% of 5", "negative"},
		{"- body has n within abc of 5", "not a number"},
		{"- body has n within 2%% of 5", "not a number"},
		{"- body has n within 1 of abc", "not a number"},
		{"- body has n within 1 of ${saved.a}x", "not a number"},
		// negatives
		{"- body has s not", "does not support"},
		{"- body has s not like x", "does not support"},
		{"- body has s not matching (", "does not compile"},
		{"- body not matching (", "does not compile"},
		{"- body has s not containing", "matches no known form"},
		// every
		{"- body every rows has", "matches no known form"},
		{"- body every rows has n => 5", `"=>"`},
		{"- body every rows has n within 1", "tolerance claim"},
		{"- body every rows has s matching (", "does not compile"},
		{"- body every rows", "matches no known form"},
		{"- body every has n", "matches no known form"},
	}
	for _, c := range cases {
		got, errs := ParseBodyAsserts([]string{c.bullet})
		if len(errs) != 1 || len(got) != 0 {
			t.Errorf("%q: want exactly one refusal, got %+v %v", c.bullet, got, errs)
			continue
		}
		if !strings.Contains(errs[0].Error(), c.mention) {
			t.Errorf("%q: the refusal must say %q, got %v", c.bullet, c.mention, errs[0])
		}
	}
}

func TestParseBodyAsserts_NoLongerSwallowsTheOldForms(t *testing.T) {
	// a field literally named `no` or `not` still parses the old way
	got, errs := ParseBodyAsserts([]string{"- body has no", "- body has not containing x", "- body has every"})
	if len(errs) != 0 || len(got) != 3 || got[0].Field != "no" || got[0].Op != BodyExists ||
		got[1].Field != "not" || got[1].Op != BodyContains || got[2].Field != "every" {
		t.Errorf("old spellings changed meaning: %+v %v", got, errs)
	}
}

func TestMatchesNoKnownForm_ListsTheNewForms(t *testing.T) {
	_, errs := ParseBodyAsserts([]string{"- body wibble"})
	if len(errs) != 1 {
		t.Fatal(errs)
	}
	for _, form := range []string{"==", "within <tolerance> of", "body not contains", "not containing|matching|equals", "body has no", "body every <array-path> has"} {
		if !strings.Contains(errs[0].Error(), form) {
			t.Errorf("the long error sentence must list %q: %v", form, errs[0])
		}
	}
}

// ── elapsed ──────────────────────────────────────────────────────────────────────────────────────

func TestParseElapsedClaim(t *testing.T) {
	ok := []struct {
		claim string
		want  ElapsedClaim
	}{
		{"elapsed < 500ms", ElapsedClaim{MaxMs: 500}},
		{"elapsed <= 500ms", ElapsedClaim{MaxMs: 500, Inclusive: true}},
		{"elapsed < 500 ms", ElapsedClaim{MaxMs: 500}},
		{"elapsed<500ms", ElapsedClaim{MaxMs: 500}},
		{"ELAPSED < 2s", ElapsedClaim{MaxMs: 2000}},
		{"elapsed <= 2 s", ElapsedClaim{MaxMs: 2000, Inclusive: true}},
		{"elapsed < 600000ms", ElapsedClaim{MaxMs: 600000}},
		{"elapsed < 600s", ElapsedClaim{MaxMs: 600000}},
		{"elapsed < 1ms", ElapsedClaim{MaxMs: 1}},
	}
	for _, c := range ok {
		got, err := ParseElapsedClaim(c.claim)
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v %v, want %+v", c.claim, got, err, c.want)
		}
	}
	bad := []struct{ claim, mention string }{
		{"elapsed > 500ms", "operator"},
		{"elapsed == 500ms", "operator"},
		{"elapsed 500ms", "operator"},
		{"elapsed < 500", "no unit"},
		{"elapsed < 500m", `unit "m"`},
		{"elapsed < 5min", `unit "min"`},
		{"elapsed < 1.5s", "positive whole number"},
		{"elapsed < -5ms", "positive whole number"},
		{"elapsed < ms", "positive whole number"},
		{"elapsed < abc", "positive whole number"},
		{"elapsed <", "positive whole number"},
		{"elapsed < 0ms", "positive whole number"},
		{"elapsed < 0s", "positive whole number"},
		{"elapsed < 600001ms", "at most 600000"},
		{"elapsed < 601s", "at most 600000"},
		{"elapsed < 5ms and more", "positive whole number"},
	}
	for _, c := range bad {
		_, err := ParseElapsedClaim(c.claim)
		if err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%q: want a refusal saying %q, got %v", c.claim, c.mention, err)
		}
	}
}

const oneHTTPStep = `{"steps":[{"type":"http","name":"check","method":"GET","url":"http://x/y"}]}`

func TestValidate_ElapsedOnAnHTTPStep_Accepted(t *testing.T) {
	for _, claim := range []string{"elapsed < 500ms", "elapsed <= 2s"} {
		_, errs := Validate(chainMD(oneHTTPStep, "### Runnable\n- step check: "+claim+"\n"))
		if len(errs) != 0 {
			t.Errorf("%q on an http step must validate: %v", claim, errs)
		}
	}
	// beside a status and a body claim
	_, errs := Validate(chainMD(oneHTTPStep, "### Runnable\n- step check: status=200\n- step check: body has no error\n- step check: elapsed < 500ms\n"))
	if len(errs) != 0 {
		t.Errorf("elapsed beside other claims: %v", errs)
	}
}

func TestValidate_ElapsedRefusals_ByName(t *testing.T) {
	cases := []struct{ name, trigger, expect, mention string }{
		{"bad unit", oneHTTPStep, "- step check: elapsed < 5min", `unit "min"`},
		{"bad operator", oneHTTPStep, "- step check: elapsed > 5ms", "operator"},
		{"no unit", oneHTTPStep, "- step check: elapsed < 5", "no unit"},
		{"too big", oneHTTPStep, "- step check: elapsed < 700s", "at most 600000"},
		{"two bounds", oneHTTPStep, "- step check: elapsed < 5ms\n- step check: elapsed < 9ms", "more than one"},
		{"with unreachable", oneHTTPStep, "- step check: unreachable\n- step check: elapsed < 5ms", "no response to time"},
		{"on an mcp step", `{"steps":[{"type":"mcp","name":"check","tool":"t","args":{}}]}`,
			"- step check: result.isError == false\n- step check: elapsed < 5ms", "`elapsed` is a claim for an http step only"},
		{"on an amqp step", `{"steps":[{"type":"amqp","name":"check","op":"consume","url_env":"X","queue":"q"}]}`,
			"- step check: broker accepts\n- step check: elapsed < 5ms", "`elapsed` is a claim for an http step only"},
		{"bare bullet", oneHTTPStep, "- step check: status=200\n- elapsed < 5ms", "claim of a chain `http` step"},
	}
	for _, c := range cases {
		_, errs := Validate(chainMD(c.trigger, "### Runnable\n"+c.expect+"\n"))
		if !find(errs, c.mention) {
			t.Errorf("%s: want a refusal saying %q, got %v", c.name, c.mention, errs)
		}
	}
}

// ── every new form on a plain (JMeter-run) scenario is refused, naming the forms ───────────────────

func TestValidate_NewFormsRefusedOnAPlainHTTPScenario(t *testing.T) {
	for _, bullet := range []string{
		"- body has latency_ms == 100",
		"- body has latency_ms within 5 of 100",
		"- body not contains error",
		"- body not matching fail",
		"- body has s not containing x",
		"- body has s not equals x",
		"- body has no secret",
		"- body every rows has id",
	} {
		_, errs := Validate(insertRunnableBullet(validMD(), bullet))
		if !find(errs, "are only evaluated by the chain `http` step") {
			t.Errorf("%q on a plain scenario must be refused by name: %v", bullet, errs)
		}
	}
	// and a bare elapsed bullet
	_, errs := Validate(insertRunnableBullet(validMD(), "- elapsed < 500ms"))
	if !find(errs, "claim of a chain `http` step") {
		t.Errorf("a bare elapsed bullet is refused: %v", errs)
	}
}

func TestValidate_NewFormsAcceptedOnAChainHTTPStep(t *testing.T) {
	for _, claim := range []string{
		"body has n == 5", "body has n within 2% of 100", "body not contains error", "body has s not equals x",
		"body has no secret", "body every rows has id",
	} {
		_, errs := Validate(chainMD(oneHTTPStep, "### Runnable\n- step check: "+claim+"\n"))
		if find(errs, "are only evaluated by") || len(errs) != 0 {
			t.Errorf("%q on a chain http step must validate: %v", claim, errs)
		}
	}
}

func TestValidate_SavedReferenceOfEqualsAndWithinMustBeSavedEarlier(t *testing.T) {
	two := `{"steps":[{"type":"http","name":"first","method":"GET","url":"http://x/a","save":{"n":"count"}},` +
		`{"type":"http","name":"second","method":"GET","url":"http://x/b"}]}`
	_, errs := Validate(chainMD(two, "### Runnable\n- step first: status=200\n- step second: body has count == ${saved.n}\n"))
	if len(errs) != 0 {
		t.Errorf("an earlier step saves n: %v", errs)
	}
	_, errs = Validate(chainMD(two, "### Runnable\n- step first: status=200\n- step second: body has count within 1 of ${saved.nope}\n"))
	if !find(errs, "no earlier step saves") {
		t.Errorf("a reference nobody saves is refused: %v", errs)
	}
	_, errs = Validate(chainMD(two, "### Runnable\n- step first: body has count == ${saved.later}\n- step second: status=200\n"))
	if !find(errs, "no earlier step saves") {
		t.Errorf("== against a variable saved only by a LATER step is refused: %v", errs)
	}
}

func TestHTTPStepClaims_ElapsedIsNotMistakenForProse(t *testing.T) {
	st, body, err := HTTPStepClaims([]string{"status=200", "elapsed < 5ms", "body has no x"})
	if err != nil || st != 200 || len(body) != 1 || body[0].Op != BodyAbsent {
		t.Fatalf("got %d %+v %v", st, body, err)
	}
	ec, err := HTTPStepElapsed([]string{"status=200", "elapsed < 5ms"})
	if err != nil || ec == nil || ec.MaxMs != 5 {
		t.Fatalf("got %+v %v", ec, err)
	}
	if ec, _ := HTTPStepElapsed([]string{"status=200"}); ec != nil {
		t.Errorf("no elapsed claim: %+v", ec)
	}
}

func TestIsNewClaimForm(t *testing.T) {
	for _, bullet := range []string{"body has n == 1", "body not contains x", "body has no x", "body every r has x"} {
		got, _ := ParseBodyAsserts([]string{bullet})
		if len(got) != 1 || !IsNewClaimForm(got[0]) {
			t.Errorf("%q is a new form", bullet)
		}
	}
	for _, bullet := range []string{"body has n > 1", "body contains x", "body has x", "body has x equals y"} {
		got, _ := ParseBodyAsserts([]string{bullet})
		if len(got) != 1 || IsNewClaimForm(got[0]) {
			t.Errorf("%q is an old form", bullet)
		}
	}
}
